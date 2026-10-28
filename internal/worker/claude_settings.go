package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

// This file belongs only to the dedicated worker. Credentials never enter
// gateway storage, command records, events, or configuration responses.
type claudeSettingsFile struct {
	Model          string `json:"model"`
	BaseURL        string `json:"base_url"`
	AuthMode       string `json:"auth_mode"`
	PermissionMode string `json:"permission_mode"`
	Credential     string `json:"credential,omitempty"`
}

func (s claudeSettingsFile) public() protocol.WorkerRuntimeSettings {
	return protocol.WorkerRuntimeSettings{Provider: "claude", Model: s.Model, BaseURL: s.BaseURL, AuthMode: s.AuthMode, PermissionMode: s.PermissionMode, APIKeyConfigured: s.Credential != ""}
}

func readClaudeSettings(path string) (claudeSettingsFile, error) {
	s := claudeSettingsFile{AuthMode: "login", PermissionMode: "default"}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 16<<10 {
		return s, errors.New("Claude runtime settings file is unavailable or not private")
	}
	f, err := os.Open(path)
	if err != nil {
		return s, errors.New("Claude runtime settings could not be read")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return s, errors.New("Claude runtime settings changed while opening")
	}
	d := json.NewDecoder(io.LimitReader(f, (16<<10)+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return s, errors.New("Claude runtime settings are invalid")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return s, errors.New("Claude runtime settings are invalid")
	}
	check := protocol.WorkerRuntimeSettingsUpdate{Model: s.Model, BaseURL: s.BaseURL, AuthMode: s.AuthMode, PermissionMode: s.PermissionMode, APIKey: &s.Credential}
	if check.Validate() != nil {
		return s, errors.New("Claude runtime settings are invalid")
	}
	return s, nil
}

func saveClaudeSettings(path string, s claudeSettingsFile) error {
	return savePrivateRuntimeSettings(path, s)
}

func savePrivateRuntimeSettings(path string, s any) error {
	if !filepath.IsAbs(path) {
		return errors.New("Runtime settings path must be absolute")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0) {
		return errors.New("Runtime settings target must be a private regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".runtime-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = io.Copy(tmp, bytes.NewReader(append(data, '\n'))); err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Close()
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func claudeSettingsPath(cfg config.WorkerConfig) string {
	if len(cfg.Runtimes) == 1 && cfg.Runtimes[0].ClaudeSettingsFile != "" {
		return cfg.Runtimes[0].ClaudeSettingsFile
	}
	return filepath.Join(filepath.Dir(cfg.StateFile), "runtime-settings.json")
}

func (a *Agent) runtimeSettings(ctx context.Context, request protocol.WorkerRuntimeSettingsRequest) protocol.WorkerRuntimeSettingsResult {
	response := protocol.WorkerRuntimeSettingsResult{RequestID: request.RequestID}
	if request.Validate() != nil || request.WorkerID != a.cfg.WorkerID {
		response.ErrorCode = "invalid_settings"
		return response
	}
	if a.cfg.AgentKind == "codex" || a.cfg.AgentKind == "" {
		return a.codexRuntimeSettings(ctx, request)
	}
	if a.cfg.AgentKind != "claude" || len(a.cfg.Runtimes) != 1 {
		response.ErrorCode = "unsupported"
		return response
	}
	if request.Settings != nil && request.Settings.CodexDefaults != nil {
		response.ErrorCode = "invalid_settings"
		return response
	}
	if err := ctx.Err(); err != nil {
		response.ErrorCode = "unavailable"
		return response
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	path := claudeSettingsPath(a.cfg)
	settings, err := readClaudeSettings(path)
	if err != nil {
		response.ErrorCode = "unavailable"
		return response
	}
	if request.Action == "get" {
		public := settings.public()
		response.Settings = &public
		return response
	}
	if a.update != nil || a.store.checkUpdateIdle() != nil {
		response.ErrorCode = "busy"
		return response
	}
	update := request.Settings
	next := claudeSettingsFile{Model: update.Model, BaseURL: update.BaseURL, AuthMode: update.AuthMode, PermissionMode: update.PermissionMode, Credential: settings.Credential}
	if update.APIKey != nil {
		next.Credential = *update.APIKey
	}
	if next.AuthMode != "login" && next.Credential == "" {
		response.ErrorCode = "invalid_settings"
		return response
	}
	// The bridge shares this admission fence with native browser turn starts.
	// An active process is never silently switched to a different account.
	a.manager.lifecycle.RLock()
	defer a.manager.lifecycle.RUnlock()
	a.manager.mu.RLock()
	bridge := a.manager.claude[a.cfg.Runtimes[0].ID]
	a.manager.mu.RUnlock()
	if bridge == nil {
		response.ErrorCode = "unavailable"
		return response
	}
	entered := false
	err = bridge.WithIdleSettingsUpdate(func() error {
		entered = true
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Extend the atomic outbound redactor before any new process can use
		// the credential. Keep the old value protected in already queued text.
		for _, credential := range []string{settings.Credential, next.Credential} {
			if credential != "" {
				a.redactor.AddLiteral(credential)
				a.manager.statsRedactor.AddLiteral(credential)
				a.store.redactor.Load().AddLiteral(credential)
			}
		}
		if err := a.store.configureRedactor(a.store.redactor.Load()); err != nil {
			return err
		}
		if err := saveClaudeSettings(path, next); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		response.ErrorCode = "busy"
		if entered {
			response.ErrorCode = "save_failed"
		}
		return response
	}
	public := next.public()
	response.Settings = &public
	return response
}
