package releasemanager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// This initializes the same native stream-json control protocol used by the
// adapter. It never writes a user message or starts a model query. The child
// receives a private home and an offline API configuration, with no inherited
// credentials, hooks, MCP servers, session history or production settings.
func (m *Manager) preflightClaudeRuntime(ctx context.Context, distribution *claudeDistribution) error {
	if distribution == nil || distribution.Binary == "" {
		return errors.New("Claude compatibility check requires a candidate binary")
	}
	if m.claudePreflight != nil {
		return m.claudePreflight(ctx, distribution)
	}
	dir, err := os.MkdirTemp("", "claude-preflight-")
	if err != nil {
		return errors.New("cannot create isolated Claude compatibility check")
	}
	defer os.RemoveAll(dir)
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"disableAllHooks":true,"autoUpdatesChannel":"latest","env":{"DISABLE_UPDATES":"1","DISABLE_AUTOUPDATER":"1","CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`), 0600); err != nil {
		return errors.New("cannot configure isolated Claude compatibility check")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"--print", "--verbose", "--input-format=stream-json", "--output-format=stream-json", "--include-partial-messages", "--permission-prompt-tool=stdio", "--setting-sources=", "--settings=" + settings, "--strict-mcp-config", "--mcp-config={\"mcpServers\":{}}", "--no-session-persistence"}
	command := exec.CommandContext(ctx, distribution.Binary, args...)
	command.Dir = dir
	command.Env = claudePreflightEnvironment(dir)
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	command.WaitDelay = time.Second
	input, err := command.StdinPipe()
	if err != nil {
		return errors.New("cannot open isolated Claude compatibility input")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return errors.New("cannot open isolated Claude compatibility output")
	}
	if err := command.Start(); err != nil {
		input.Close()
		output.Close()
		return errors.New("cannot start isolated Claude compatibility check")
	}
	defer func() {
		input.Close()
		output.Close()
		_ = command.Cancel()
		_ = command.Wait()
	}()
	if _, err := io.WriteString(input, "{\"type\":\"control_request\",\"request_id\":\"mact-compatibility-check\",\"request\":{\"subtype\":\"initialize\",\"hooks\":{}}}\n"); err != nil {
		return errors.New("candidate Claude runtime rejected its isolated compatibility request")
	}
	scanner := bufio.NewScanner(io.LimitReader(output, 4*1024*1024+1))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var response struct {
			Type     string `json:"type"`
			Subtype  string `json:"subtype"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			return errors.New("candidate Claude runtime failed the isolated protocol compatibility check")
		}
		// Native Claude can emit local UI invalidation notifications before
		// initialize completes. The adapter accepts these system envelopes too.
		if response.Type == "system" && response.Subtype != "" {
			continue
		}
		if response.Type != "control_response" || response.Response.RequestID != "mact-compatibility-check" || response.Response.Subtype != "success" {
			return errors.New("candidate Claude runtime failed the isolated protocol compatibility check")
		}
		if !strings.HasPrefix(strings.TrimSpace(string(response.Response.Response)), "{") || !json.Valid(response.Response.Response) {
			return errors.New("candidate Claude runtime returned an invalid compatibility response")
		}
		return nil
	}
	return errors.New("candidate Claude runtime did not complete the isolated protocol compatibility check")
}

func claudePreflightEnvironment(home string) []string {
	env := []string{
		"HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"), "XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"TMPDIR=" + home, "CLAUDE_CODE_TMPDIR=" + home,
		"ANTHROPIC_API_KEY=sk-ant-api03-isolated-compatibility-check",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1", "DISABLE_UPDATES=1", "DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_ENABLE_TELEMETRY=0", "DO_NOT_TRACK=1",
	}
	for _, key := range []string{"PATH", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
