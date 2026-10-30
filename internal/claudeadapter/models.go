package claudeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Native ModelInfo and initialize.models are documented by the Agent SDK:
// https://code.claude.com/docs/en/agent-sdk/typescript#modelinfo
// Capability lists come from the installed runtime, including its provider,
// custom models and organization policy. Never borrow effort levels from Codex.
type nativeModel struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

type nativeCapabilities struct {
	models           []nativeModel
	permissionModes  []string
	manualPermission bool
}

var nativePermissionModes = []string{"default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions"}

func reasoningEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func (s *server) supportedPermissionModes() []string {
	return append([]string{}, s.capabilities.permissionModes...)
}

func (s *server) supportsPermissionMode(mode string) bool {
	for _, supported := range s.capabilities.permissionModes {
		if supported == mode {
			return true
		}
	}
	return false
}

func decodeNativeModels(raw json.RawMessage) []nativeModel {
	var result struct {
		Models []nativeModel `json:"models"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	models := make([]nativeModel, 0, len(result.Models))
	seen := make(map[string]bool)
	for _, model := range result.Models {
		if validateModel(model.Value) != nil || seen[model.Value] {
			continue
		}
		seen[model.Value] = true
		if model.ResolvedModel != "" && validateModel(model.ResolvedModel) != nil {
			model.ResolvedModel = ""
		}
		model.DisplayName = boundedText(model.DisplayName, 256)
		model.Description = boundedText(model.Description, 2048)
		if model.DisplayName == "" {
			model.DisplayName = model.Value
		}
		efforts := []string{}
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			for _, advertised := range model.SupportedEffortLevels {
				if advertised == effort {
					efforts = append(efforts, effort)
					break
				}
			}
		}
		// Older runtimes expose supportsEffort without a per-model level list.
		// The common native levels are low/medium/high; max and xhigh require
		// an explicit advertisement rather than a guess based on a model name.
		if model.SupportsEffort && len(model.SupportedEffortLevels) == 0 {
			efforts = []string{"low", "medium", "high"}
		}
		model.SupportedEffortLevels = efforts
		models = append(models, model)
	}
	return models
}

func (s *server) observeCapabilities(raw json.RawMessage) {
	if models := decodeNativeModels(raw); len(models) != 0 {
		s.capabilities.models = models
	}
}

// This handshake never submits a user message. --no-session-persistence keeps
// catalog discovery out of native history. Administrator Args are deliberately
// excluded: a positional argument there could otherwise become a paid prompt.
func (s *server) discoverCapabilities(parent context.Context, settings LaunchSettings) nativeCapabilities {
	capabilities := nativeCapabilities{permissionModes: []string{"default"}}
	env, err := childEnvironment(s.cfg.Env, settings.Env)
	if err != nil {
		return capabilities
	}
	timeout := min(s.cfg.StartupTimeout, 10*time.Second)
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	help := exec.CommandContext(ctx, s.cfg.Command, "--help")
	help.Dir, help.Env, help.Stderr = s.cfg.WorkingDirectory, env, io.Discard
	configureProcess(help)
	help.Cancel = func() error { stopProcess(help); return nil }
	help.WaitDelay = 250 * time.Millisecond
	var output cappedOutput
	help.Stdout = &output
	if help.Run() == nil {
		capabilities.permissionModes = permissionModesFromHelp(output.Bytes())
		capabilities.manualPermission = strings.Contains(string(output.Bytes()), `"manual"`)
	}
	if ctx.Err() != nil {
		return capabilities
	}
	initialized, _, err := s.metadataProbe(ctx, settings, false)
	if err == nil {
		capabilities.models = decodeNativeModels(initialized)
	}
	return capabilities
}

type cappedOutput struct{ bytes.Buffer }

func (w *cappedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := (256 << 10) - w.Len(); remaining > 0 {
		_, _ = w.Buffer.Write(data[:min(remaining, len(data))])
	}
	return n, nil
}

func permissionModesFromHelp(help []byte) []string {
	text := string(help)
	start := strings.Index(text, "--permission-mode")
	if start < 0 {
		return []string{"default"}
	}
	text = text[start+len("--permission-mode"):]
	if next := strings.Index(text, "\n  -"); next >= 0 {
		text = text[:next]
	}
	modes := []string{"default"}
	for _, mode := range nativePermissionModes {
		if mode == "default" {
			continue
		}
		// The native help prints a quoted choices list. Exact quoted tokens
		// avoid confusing a description mentioning default with a capability.
		if strings.Contains(text, `"`+mode+`"`) || strings.Contains(text, `'`+mode+`'`) {
			modes = append(modes, mode)
		}
	}
	return modes
}

func (s *server) effortLevels(model string) []string {
	for _, entry := range s.capabilities.models {
		if entry.Value == model || (entry.ResolvedModel != "" && entry.ResolvedModel == model) {
			return entry.SupportedEffortLevels
		}
	}
	return nil
}

func (s *server) supportsEffort(model, effort string) bool {
	for _, supported := range s.effortLevels(model) {
		if supported == effort {
			return true
		}
	}
	return false
}

func (s *server) modelList(settings LaunchSettings) map[string]any {
	models := append([]nativeModel(nil), s.capabilities.models...)
	// The documented aliases are usable even when a legacy CLI cannot return
	// metadata. Their moving target and custom IDs get no invented effort list:
	// https://code.claude.com/docs/en/model-config#model-aliases
	if len(models) == 0 {
		for _, value := range []string{"default", "sonnet", "opus", "haiku", "sonnet[1m]", "opus[1m]", "opusplan"} {
			models = append(models, nativeModel{Value: value, DisplayName: value})
		}
	}
	known := map[string]bool{}
	for _, model := range models {
		known[model.Value] = true
	}
	add := func(value string) {
		if value == "" || validateModel(value) != nil || known[value] {
			return
		}
		known[value] = true
		models = append(models, nativeModel{Value: value, DisplayName: value, SupportedEffortLevels: s.effortLevels(value)})
	}
	add(settings.Model)
	for _, thread := range sortedThreads(s.threads) {
		if s.deletions[thread.ID] == nil {
			add(thread.RequestedModel)
		}
	}
	defaultModel := settings.Model
	if defaultModel == "" {
		defaultModel = "default"
	}
	data := make([]any, 0, len(models))
	for _, model := range models {
		efforts := make([]any, 0, len(model.SupportedEffortLevels))
		for _, effort := range model.SupportedEffortLevels {
			efforts = append(efforts, map[string]any{"reasoningEffort": effort, "description": EffortLabel(effort)})
		}
		data = append(data, map[string]any{"id": model.Value, "model": model.Value, "resolvedModel": model.ResolvedModel, "displayName": model.DisplayName, "description": model.Description, "isDefault": model.Value == defaultModel, "supportedReasoningEfforts": efforts, "supportsPersonality": false})
	}
	return map[string]any{"data": data, "nextCursor": nil}
}
