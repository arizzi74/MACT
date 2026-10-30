package worker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iaia/telegramgw/internal/claudeadapter"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
)

func (m *RuntimeManager) startClaudeProfile(ctx context.Context, profile config.RuntimeProfile) (*codexadapter.Client, error) {
	base := filepath.Join(filepath.Dir(m.cfg.StateFile), "runtimes")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, "cc-")
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(profile.ID))
	bridge, err := claudeadapter.Start(ctx, claudeadapter.Config{
		Command: profile.ClaudeBinary, WorkingDirectory: profile.WorkingDirectory,
		StateDirectory: filepath.Join(filepath.Dir(m.cfg.StateFile), "claude", fmt.Sprintf("%x", digest[:8])),
		SocketPath:     filepath.Join(dir, "s.sock"), Stderr: io.Discard,
		Redact: func(value string) string { return m.store.redactor.Load().Redact(value) },
		LoadSettings: func() (claudeadapter.LaunchSettings, error) {
			settings, err := readClaudeSettings(claudeSettingsPath(m.cfg))
			if err != nil {
				return claudeadapter.LaunchSettings{}, err
			}
			// Clear all account/model overrides first, so selecting inherited
			// login cannot accidentally keep a service environment API token.
			env := map[string]string{
				// Runtime maintenance owns version changes for worker children.
				// Keep native settings and separately launched CLIs untouched.
				"DISABLE_AUTOUPDATER": "1",
				"ANTHROPIC_API_KEY":   "", "ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_BASE_URL": settings.BaseURL,
				"ANTHROPIC_MODEL":                settings.Model,
				"ANTHROPIC_DEFAULT_OPUS_MODEL":   "",
				"ANTHROPIC_DEFAULT_SONNET_MODEL": "",
				"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "",
			}
			if settings.BaseURL != "" {
				// Custom gateways historically use one configured provider model for
				// Claude's family aliases. Official accounts keep native alias routing.
				for _, key := range []string{"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
					env[key] = settings.Model
				}
			}
			if settings.AuthMode != "login" {
				// A configured API account takes precedence over inherited OAuth
				// and cloud-provider environment switches in the service account.
				for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_CUSTOM_HEADERS"} {
					env[key] = ""
				}
			}
			if settings.AuthMode == "api_key" {
				env["ANTHROPIC_API_KEY"] = settings.Credential
			} else if settings.AuthMode == "auth_token" {
				env["ANTHROPIC_AUTH_TOKEN"] = settings.Credential
			}
			return claudeadapter.LaunchSettings{Env: env, Model: settings.Model, ReasoningEffort: settings.ReasoningEffort, PermissionMode: settings.PermissionMode}, nil
		},
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m.mu.Lock()
	m.claude[profile.ID] = bridge
	m.mu.Unlock()
	go m.claudeCreditsLoop(bridge)
	go func() {
		<-bridge.Done()
		m.mu.Lock()
		if m.claude[profile.ID] == bridge {
			delete(m.claude, profile.ID)
		}
		m.mu.Unlock()
		_ = os.Remove(dir)
	}()
	return bridge.Client, nil
}

var claudeVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)

func (a *Agent) newThreadOptions(cwd, historyMode string) (codexadapter.ThreadOptions, error) {
	options := codexadapter.ThreadOptions{CWD: cwd, HistoryMode: historyMode}
	if a.cfg.AgentKind != "claude" {
		defaults, err := readCodexDefaults(codexDefaultsPath(a.cfg))
		if err != nil {
			return options, validationError("Codex session defaults are unavailable. Check this worker's runtime settings before creating a session.")
		}
		options = codexDefaultThreadOptions(defaults, cwd, historyMode)
	}
	return options, nil
}

func claudeExecutableVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	command := exec.CommandContext(ctx, binary, "--version")
	command.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1")
	data, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("Claude Code version unavailable")
	}
	parts := strings.Fields(string(data))
	if len(parts) == 0 || !claudeVersionPattern.MatchString(parts[0]) {
		return "", fmt.Errorf("Claude Code version is invalid")
	}
	return parts[0], nil
}
