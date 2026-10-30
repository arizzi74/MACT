package claudeadapter

import (
	"encoding/json"
	"errors"
	"time"
)

func (t *savedThread) effectiveSettings(defaults LaunchSettings) LaunchSettings {
	if t.RequestedModel != "" {
		defaults.Model = t.RequestedModel
	}
	if t.RequestedEffort != nil {
		defaults.ReasoningEffort = *t.RequestedEffort
	}
	if t.RequestedPermissionMode != "" {
		defaults.PermissionMode = t.RequestedPermissionMode
	}
	return defaults
}

func (t *savedThread) settingsSnapshot() map[string]any {
	model := t.StatusConfigModel
	if model == "" {
		model = "default"
	}
	return map[string]any{"model": model, "effort": optionalString(t.StatusConfigEffort), "permissionMode": optionalString(t.StatusConfigPermission)}
}

func (s *server) threadResult(t *savedThread, includeTurns bool) map[string]any {
	settings := t.settingsSnapshot()
	return map[string]any{"thread": s.threadMetadata(t, includeTurns), "model": settings["model"], "reasoningEffort": settings["effort"], "permissionMode": settings["permissionMode"]}
}

func (s *server) broadcastSettings(t *savedThread) {
	if s.deletions[t.ID] == nil {
		s.broadcast("thread/settings/updated", map[string]any{"threadId": t.ID, "threadSettings": t.settingsSnapshot()})
	}
}

func (s *server) checkThreadSettingsIdle(t *savedThread) error {
	if t.active() != nil || len(t.Queue) > 0 {
		return errors.New("Claude session has an active or queued turn; wait until it is idle")
	}
	for _, pending := range s.pending {
		if pending.Process != nil && pending.Process.threadID == t.ID {
			return errors.New("Claude session has pending approvals or questions")
		}
	}
	if proc := s.runners[t.ID]; proc != nil && proc.hasBackgroundWork() {
		return errors.New("Claude session has active background work")
	}
	return nil
}

func decodeSettingsUpdate(raw json.RawMessage) (map[string]string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, errors.New("Invalid Claude settings update")
	}
	update := map[string]string{}
	for name, value := range fields {
		if name == "threadId" {
			continue
		}
		switch name {
		case "model", "effort", "permissionMode":
		default:
			return nil, errors.New("Claude Code supports only model, effort and permissionMode session settings")
		}
		var text string
		if string(value) == "null" || json.Unmarshal(value, &text) != nil {
			return nil, errors.New("Claude session settings must be strings")
		}
		update[name] = text
	}
	if len(update) == 0 {
		return nil, errors.New("At least one Claude session setting is required")
	}
	return update, nil
}

// Admission, persistence and retirement share server.mu. Failure leaves the
// process and preferences untouched; success retires only this idle session.
func (s *server) updateThreadSettings(t *savedThread, raw json.RawMessage) error {
	if err := s.checkThreadSettingsIdle(t); err != nil {
		return err
	}
	update, err := decodeSettingsUpdate(raw)
	if err != nil {
		return err
	}
	defaults, err := s.settings()
	if err != nil {
		return err
	}
	candidate := *t
	if model, ok := update["model"]; ok {
		if model != "" && validateModel(model) != nil {
			return errors.New("Invalid Claude model")
		}
		candidate.RequestedModel = model
	}
	if effort, ok := update["effort"]; ok {
		if effort != "" && !reasoningEffort(effort) {
			return errors.New("Invalid Claude effort level")
		}
		candidate.RequestedEffort = &effort
	}
	if mode, ok := update["permissionMode"]; ok {
		if mode != "" && !s.supportsPermissionMode(mode) {
			return errors.New("This permission mode is not advertised by the installed Claude runtime")
		}
		candidate.RequestedPermissionMode = mode
	}
	effective := candidate.effectiveSettings(defaults)
	model := effective.Model
	if model == "" {
		model = "default"
	}
	_, changesModel := update["model"]
	_, changesEffort := update["effort"]
	if (changesModel || changesEffort) && effective.ReasoningEffort != "" && !s.supportsEffort(model, effective.ReasoningEffort) {
		return errors.New("This effort level is not advertised for the selected Claude model; select a supported effort or reset effort to the model default")
	}
	// Deep-copy mutable status before calculating the candidate's observation.
	status := ClaudeStatus{Provider: "claude"}
	if t.ClaudeStatus != nil {
		status = *t.ClaudeStatus
	}
	candidate.ClaudeStatus = &status
	candidate.UpdatedAt = time.Now().Unix()
	s.applyStatusSettings(&candidate, defaults, true)
	if err := s.save(&candidate); err != nil {
		return err
	}
	*t = candidate
	if proc := s.runners[t.ID]; proc != nil {
		proc.stop()
		delete(s.runners, t.ID)
	}
	return nil
}
