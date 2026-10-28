package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Defaults belong to the worker state file, never the native Codex home. Two
// workers sharing a state directory must not change each other's preferences.
func codexDefaultsPath(cfg config.WorkerConfig) string {
	return cfg.StateFile + ".codex-defaults.json"
}

func readCodexDefaults(path string) (protocol.CodexSessionDefaults, error) {
	var defaults protocol.CodexSessionDefaults
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 16<<10 {
		return defaults, errors.New("Codex session defaults file is unavailable or not private")
	}
	f, err := os.Open(path)
	if err != nil {
		return defaults, errors.New("Codex session defaults could not be read")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return defaults, errors.New("Codex session defaults changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(f, (16<<10)+1))
	decoder.DisallowUnknownFields()
	var decoded *protocol.CodexSessionDefaults
	if decoder.Decode(&decoded) != nil || decoded == nil || decoder.Decode(new(any)) != io.EOF || decoded.Validate() != nil {
		return protocol.CodexSessionDefaults{}, errors.New("Codex session defaults are invalid")
	}
	return *decoded, nil
}

func saveCodexDefaults(path string, defaults protocol.CodexSessionDefaults) error {
	if defaults.Validate() != nil || !filepath.IsAbs(path) {
		return errors.New("Codex session defaults are invalid")
	}
	return savePrivateRuntimeSettings(path, defaults)
}

func codexDefaultsPublic(defaults protocol.CodexSessionDefaults) *protocol.WorkerRuntimeSettings {
	return &protocol.WorkerRuntimeSettings{Provider: "codex", CodexDefaults: &defaults}
}

func (a *Agent) codexRuntimeSettings(ctx context.Context, request protocol.WorkerRuntimeSettingsRequest) protocol.WorkerRuntimeSettingsResult {
	response := protocol.WorkerRuntimeSettingsResult{RequestID: request.RequestID}
	if request.Settings != nil && request.Settings.CodexDefaults == nil {
		response.ErrorCode = "unsupported"
		return response
	}
	if ctx.Err() != nil {
		response.ErrorCode = "unavailable"
		return response
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	if request.Action == "get" {
		defaults, err := readCodexDefaults(codexDefaultsPath(a.cfg))
		if err != nil {
			response.ErrorCode = "unavailable"
			return response
		}
		response.Settings = codexDefaultsPublic(defaults)
		return response
	}
	if a.update != nil {
		response.ErrorCode = "busy"
		return response
	}
	// Existing sessions keep their own model, effort and permissions. Saving
	// defaults changes only later thread/start calls, so active turns need no fence.
	defaults := *request.Settings.CodexDefaults
	if ctx.Err() != nil {
		response.ErrorCode = "unavailable"
		return response
	}
	if err := saveCodexDefaults(codexDefaultsPath(a.cfg), defaults); err != nil {
		response.ErrorCode = "save_failed"
		return response
	}
	response.Settings = codexDefaultsPublic(defaults)
	return response
}

func codexDefaultThreadOptions(defaults protocol.CodexSessionDefaults, cwd, historyMode string) codexadapter.ThreadOptions {
	options := codexadapter.ThreadOptions{
		CWD: cwd, HistoryMode: historyMode, Model: defaults.Model, ReasoningEffort: defaults.ReasoningEffort,
		ApprovalPolicy: "on-request", ApprovalsReviewer: "user",
	}
	switch defaults.PermissionLevel {
	case "ask-for-approval":
		options.PermissionProfile = ":workspace"
	case "read-only":
		options.PermissionProfile = ":read-only"
	case "full-access":
		options.PermissionProfile, options.ApprovalPolicy = ":danger-full-access", "never"
	default:
		options.Sandbox = "workspace-write"
	}
	return options
}

func applySavedNativeCodexDefaults(payload []byte, path string) ([]byte, error) {
	// A worker upgrade alone must preserve terminal-supplied launch settings.
	// Saving even an empty defaults object opts this worker into reset defaults.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return payload, nil
	}
	return applyNativeCodexDefaults(payload, func() (protocol.CodexSessionDefaults, error) {
		return readCodexDefaults(path)
	})
}

// Native terminal /new and /clear issue thread/start directly through the
// attachment proxy. Replace the previous session's carried preferences before
// forwarding the single atomic start RPC; resumes and later edits pass through.
func applyNativeCodexDefaults(payload []byte, load func() (protocol.CodexSessionDefaults, error)) ([]byte, error) {
	message, ok := nativeObject(payload)
	if !ok {
		return payload, nil
	}
	var method string
	if json.Unmarshal(message["method"], &method) != nil || method != "thread/start" {
		return payload, nil
	}
	defaults, err := load()
	if err != nil || defaults.Validate() != nil {
		return nil, errors.New("Codex session defaults are unavailable. Check this worker's runtime settings before creating a session.")
	}
	params := map[string]json.RawMessage{}
	if raw := message["params"]; len(raw) > 0 && string(raw) != "null" {
		params, ok = nativeObject(raw)
		if !ok {
			return nil, errors.New("Invalid new session parameters.")
		}
	}
	config := map[string]json.RawMessage{}
	if raw := params["config"]; len(raw) > 0 && string(raw) != "null" {
		config, ok = nativeObject(raw)
		if !ok {
			return nil, errors.New("Invalid new session configuration.")
		}
	}
	// Codex clients can carry both top-level overrides and native config
	// overrides. Neither may preserve a previous session's preferences on reset.
	for _, key := range []string{"model", "model_reasoning_effort", "approval_policy", "approvals_reviewer", "sandbox_mode", "permissions", "default_permissions"} {
		delete(config, key)
	}
	for _, key := range []string{"model", "approvalPolicy", "approvalsReviewer", "sandbox", "permissions"} {
		delete(params, key)
	}
	put := func(key string, value any) { params[key], _ = json.Marshal(value) }
	options := codexDefaultThreadOptions(defaults, "", "")
	if options.Model != "" {
		put("model", options.Model)
	}
	if options.ReasoningEffort != "" {
		config["model_reasoning_effort"], _ = json.Marshal(options.ReasoningEffort)
	}
	put("approvalPolicy", options.ApprovalPolicy)
	put("approvalsReviewer", options.ApprovalsReviewer)
	if options.PermissionProfile != "" {
		put("permissions", options.PermissionProfile)
	} else {
		put("sandbox", options.Sandbox)
	}
	delete(params, "config")
	if len(config) != 0 {
		put("config", config)
	}
	message["params"], _ = json.Marshal(params)
	return json.Marshal(message)
}
