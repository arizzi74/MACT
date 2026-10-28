package sessionarchive

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Verify restores into a private, credential-free HOME and CODEX_HOME, resumes
// the parent and reads every descendant, then repeats after app-server restart.
// It never starts a model turn or runs a project command.
func Verify(ctx context.Context, archivePath string, o VerifyOptions) (Manifest, error) {
	m, err := Read(ctx, archivePath)
	if err != nil {
		return Manifest{}, err
	}
	root, err := os.MkdirTemp("", "telegramgw-archive-verify-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(root)
	home := filepath.Join(root, "codex")
	project := filepath.Join(root, "project")
	// The profile's executable can have been updated independently of the
	// running app-server. Never prove an archive using a different native version.
	if err := verifyExecutableVersion(ctx, o.CodexBinary, m.CodexVersion, root); err != nil {
		return Manifest{}, err
	}
	progress(o.Progress, "Verifying restore in an isolated Codex home")
	if _, err := Restore(ctx, RestoreOptions{ArchivePath: archivePath, CodexHome: home, SQLiteHome: home, Destination: project, CodexVersion: m.CodexVersion}); err != nil {
		return Manifest{}, fmt.Errorf("isolated native import: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_provider = \"archive_verify\"\nmodel = \"archive-proof\"\ncheck_for_update_on_startup = false\n[model_providers.archive_verify]\nname = \"Offline archive verification\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n"), 0600); err != nil {
		return Manifest{}, err
	}
	for pass := 0; pass < 2; pass++ {
		if pass == 1 {
			progress(o.Progress, "Verifying native resume after app-server restart")
		}
		verifyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		client, err := startVerificationServer(verifyCtx, o.CodexBinary, home, project, root)
		if err != nil {
			cancel()
			return Manifest{}, err
		}
		err = verifyRestoredThreads(client, m, project, pass+1)
		client.close()
		cancel()
		if err != nil {
			return Manifest{}, err
		}
	}
	m.VerifiedSelfContained = true
	return m, nil
}

func verifyExecutableVersion(ctx context.Context, binary, archiveVersion, root string) error {
	if binary == "" {
		binary = "codex"
	}
	probeRoot := filepath.Join(root, "version-check")
	home := filepath.Join(probeRoot, "codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	environment, err := verificationEnvironment(home, probeRoot)
	if err != nil {
		return err
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(versionCtx, binary, "--version")
	cmd.Dir = probeRoot
	cmd.Env = environment
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var output verificationVersionOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil || output.overflow {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: the selected Codex executable's version could not be confirmed", ErrUnsupported)
	}
	return CheckVersionCompatibility(archiveVersion, string(output.data))
}

// An executable's diagnostics must not turn a bounded version probe into an
// unbounded allocation. Continue draining output while retaining only 4 KiB.
type verificationVersionOutput struct {
	data     []byte
	overflow bool
}

func (o *verificationVersionOutput) Write(data []byte) (int, error) {
	n := len(data)
	remaining := 4096 - len(o.data)
	if len(data) > remaining {
		o.overflow = true
		data = data[:remaining]
	}
	o.data = append(o.data, data...)
	return n, nil
}

func verifyRestoredThreads(client *verificationServer, m Manifest, project string, pass int) error {
	check := func(id string, parent bool) error {
		method, operation := "thread/read", "subagent read"
		params := map[string]any{"threadId": id, "includeTurns": false}
		if parent {
			method, operation = "thread/resume", "resume"
			params = map[string]any{"threadId": id, "cwd": project, "modelProvider": "archive_verify", "model": "archive-proof", "excludeTurns": true}
		}
		var response struct {
			Thread struct {
				ID  string `json:"id"`
				CWD string `json:"cwd"`
			} `json:"thread"`
		}
		if err := client.call(method, params, &response); err != nil {
			return fmt.Errorf("native %s pass %d for thread %s: %w", operation, pass, id, err)
		}
		cwd := response.Thread.CWD
		validCWD := cwd == project
		if !parent {
			// A subagent may work in a project subdirectory. Restore preserves
			// that suffix; reads must not override it or accept an outside path.
			validCWD = filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd && within(project, cwd)
		}
		if response.Thread.ID != id || !validCWD {
			return fmt.Errorf("native %s returned wrong thread or project (pass %d, thread %s)", operation, pass, id)
		}
		// Metadata reads alone do not prove that native history is available.
		// Force bounded paginated history to be projected for every thread;
		// legacy transcripts use the same native listing contract.
		var history json.RawMessage
		if err := client.call("thread/turns/list", map[string]any{"threadId": id, "limit": 1, "itemsView": "notLoaded"}, &history); err != nil {
			return fmt.Errorf("native history read pass %d for thread %s: %w", pass, id, err)
		}
		return nil
	}
	// Manifest validation already proves the complete native spawn tree. Its
	// root is the resumable user session; unloaded multi-agent v2 descendants
	// must be inspected without trying to resume them as standalone sessions.
	if err := check(m.ThreadID, true); err != nil {
		return err
	}
	for _, id := range m.ThreadIDs {
		if id != m.ThreadID {
			if err := check(id, false); err != nil {
				return err
			}
		}
	}
	return nil
}

type verificationServer struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	scanner *bufio.Scanner
	next    int
}

func startVerificationServer(ctx context.Context, binary, home, project, root string) (*verificationServer, error) {
	if binary == "" {
		binary = "codex"
	}
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://", "-c", "features.plugins=false")
	cmd.Dir = project
	// A complete environment prevents inherited credentials, real HOME, remote
	// state overrides, proxy settings, and connected service configuration.
	environment, err := verificationEnvironment(home, root)
	if err != nil {
		return nil, err
	}
	cmd.Env = environment
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &verificationServer{cmd: cmd, in: in, scanner: bufio.NewScanner(out)}
	c.scanner.Buffer(make([]byte, 4096), 64<<20)
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]any{"name": "telegramgw-archive-verification", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
		c.close()
		return nil, err
	}
	if err := json.NewEncoder(c.in).Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func verificationEnvironment(home, root string) ([]string, error) {
	userHome := filepath.Join(root, "user-home")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		return nil, err
	}
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + userHome, "CODEX_HOME=" + home, "CODEX_SQLITE_HOME=" + home, "TERM=xterm-256color"}, nil
}
func (c *verificationServer) call(method string, params any, result any) error {
	c.next++
	if err := json.NewEncoder(c.in).Encode(map[string]any{"id": c.next, "method": method, "params": params}); err != nil {
		return err
	}
	for c.scanner.Scan() {
		var response struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(c.scanner.Bytes(), &response); err != nil {
			return err
		}
		if response.ID == nil || *response.ID != c.next {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("%s (%d): %s", method, response.Error.Code, response.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(response.Result, result)
		}
		return nil
	}
	if err := c.scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("native verification app-server exited during %s", method)
}
func (c *verificationServer) close() { c.in.Close(); c.cmd.Process.Kill(); c.cmd.Wait() }
