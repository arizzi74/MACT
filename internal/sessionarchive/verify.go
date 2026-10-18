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
// every selected native thread, then restarts app-server and resumes again.
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
		for _, id := range m.ThreadIDs {
			params := map[string]any{"threadId": id, "cwd": project, "modelProvider": "archive_verify", "model": "archive-proof", "excludeTurns": true}
			var response struct {
				Thread struct {
					ID  string `json:"id"`
					CWD string `json:"cwd"`
				} `json:"thread"`
			}
			if err = client.call("thread/resume", params, &response); err != nil {
				client.close()
				cancel()
				return Manifest{}, fmt.Errorf("native resume pass %d: %w", pass+1, err)
			}
			if response.Thread.ID != id || response.Thread.CWD != project {
				client.close()
				cancel()
				return Manifest{}, fmt.Errorf("native resume returned wrong thread or project")
			}
			// Force canonical paginated history to be projected and read. Legacy
			// transcripts are exposed by the same native listing contract.
			var history json.RawMessage
			if err = client.call("thread/turns/list", map[string]any{"threadId": id, "limit": 1, "itemsView": "notLoaded"}, &history); err != nil {
				client.close()
				cancel()
				return Manifest{}, fmt.Errorf("native history read: %w", err)
			}
		}
		client.close()
		cancel()
	}
	m.VerifiedSelfContained = true
	return m, nil
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
	userHome := filepath.Join(root, "user-home")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		return nil, err
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + userHome, "CODEX_HOME=" + home, "CODEX_SQLITE_HOME=" + home, "TERM=xterm-256color"}
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
