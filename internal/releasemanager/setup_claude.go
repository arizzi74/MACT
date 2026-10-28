package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Claude Code is owned by the account running this dedicated worker. Keep its
// installation and sign-in independent of the Codex installer and updater.
func (m *Manager) setupWorkerClaude(ctx context.Context, l *Layout) (string, error) {
	binary, err := exec.LookPath("claude")
	if err != nil {
		binary, err = exec.LookPath(filepath.Join(l.Bin, "claude"))
	}
	if err != nil {
		return "", errors.New("Claude Code is required; install the Claude CLI for this account, then rerun mact setup claude-worker")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	version, err := m.command(ctx, binary, "--version")
	if err != nil || strings.TrimSpace(string(version)) == "" {
		return "", errors.New("the installed Claude Code CLI did not pass its version check")
	}
	fmt.Fprintln(m.Out, "Using the installed Claude Code CLI. Its existing login is available to the worker; model, endpoint and account credentials can be configured in the gateway admin console.")
	return binary, nil
}
