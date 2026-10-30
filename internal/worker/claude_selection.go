package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// A custom provider model can be selected by ID without pretending that native
// metadata advertised its reasoning capabilities.
func (s *sessionActor) claudeModel(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if !protocol.ValidModelMenuArgs(args) || strings.HasPrefix(args, "--") {
		return "", validationError("usage: /model MODEL [EFFORT]")
	}
	fields := strings.Fields(args)
	if strings.HasPrefix(fields[0], "-") {
		return "", validationError("invalid model ID")
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return "", err
	}
	model := fields[0]
	effort := ""
	if len(fields) == 2 {
		effort = fields[1]
	}
	var selected *codexadapter.ModelInfo
	for i := range models {
		if model == models[i].ID || model == models[i].Model {
			selected = &models[i]
			break
		}
	}
	if selected != nil {
		model = valueOr(selected.Model, selected.ID)
	} else {
		for i := range models {
			if model == models[i].ResolvedModel {
				selected = &models[i]
				// Preserve an explicitly pinned native model ID instead of
				// changing it back to a moving alias such as opus or sonnet.
				break
			}
		}
	}
	if effort != "" && (selected == nil || !containsString(selected.ReasoningEfforts, effort)) {
		return "", validationError("reasoning effort %q is not advertised for model %s", effort, model)
	}
	if err := s.ensureClaudeSettingsIdle(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateClaudeThreadSettings(ctx, s.session.ThreadID, &model, &effort, nil); err != nil {
		return "", err
	}
	if effort != "" {
		return fmt.Sprintf("Model set to %s with %s reasoning.", model, effort), nil
	}
	return "Model set to " + model + " with default reasoning.", nil
}

func (s *sessionActor) claudeReasoning(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	status, err := client.ReadClaudeStatus(ctx, s.session.ThreadID)
	if err != nil {
		return "", err
	}
	if args == "" {
		effort := "default"
		if status.ReasoningEffort != nil {
			effort = valueOr(*status.ReasoningEffort, "default")
		}
		return "Reasoning effort: " + effort, nil
	}
	if len(strings.Fields(args)) != 1 {
		return "", validationError("usage: /reasoning EFFORT")
	}
	effort := args
	if args == "default" {
		effort = ""
	}
	if !protocol.ValidClaudeReasoningEffort(effort) {
		return "", validationError("invalid Claude reasoning effort %q", args)
	}
	if effort != "" {
		models, err := client.ListModels(ctx)
		if err != nil {
			return "", err
		}
		model := "default"
		if status.Model != nil {
			model = *status.Model
		}
		allowed := false
		for _, m := range models {
			if (m.ID == model || m.Model == model || m.ResolvedModel == model) && containsString(m.ReasoningEfforts, effort) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", validationError("reasoning effort %q is not advertised for model %s", effort, valueOr(model, "current"))
		}
	}
	if err := s.ensureClaudeSettingsIdle(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateClaudeThreadSettings(ctx, s.session.ThreadID, nil, &effort, nil); err != nil {
		return "", err
	}
	return "Reasoning effort set to " + valueOr(effort, "default") + ".", nil
}

func (s *sessionActor) ensureClaudeSettingsIdle(ctx context.Context, client *codexadapter.Client) error {
	if !updateSessionIdle(s.session) || s.activeCommand != nil || s.awaitingTurnStart || len(s.queue) != 0 || len(s.pending) != 0 {
		return validationError("this session has an active turn or pending work")
	}
	return s.ensureCodexThreadLoaded(ctx, client)
}

func claudePermissionOptions(modes []string) []protocol.PermissionOption {
	var result []protocol.PermissionOption
	seen := map[string]bool{}
	for _, mode := range modes {
		if seen[mode] || !protocol.ValidClaudePermissionMode(mode) {
			continue
		}
		seen[mode] = true
		option := protocol.PermissionOption{ID: mode}
		switch mode {
		case "default":
			option.Label = "Default"
			option.Description = "Ask for approval according to native Claude Code permissions."
		case "acceptEdits":
			option.Label = "Accept edits"
			option.Description = "Automatically accept file edits; keep native checks for other tools."
		case "plan":
			option.Label = "Plan"
			option.Description = "Use Claude Code's native plan mode."
		case "dontAsk":
			option.Label = "Don't ask"
			option.Description = "Deny tool actions that require approval instead of prompting."
		case "auto":
			option.Label = "Auto"
			option.Description = "Let Claude Code automatically review tool permissions."
		case "bypassPermissions":
			option.Label = "Bypass permissions"
			option.Description = "Skip native permission checks. Requires confirmation."
		}
		result = append(result, option)
	}
	return result
}

func (s *sessionActor) claudePermissions(ctx context.Context, client *codexadapter.Client, args string) (protocol.Result, error) {
	if args == "cancel" {
		return protocol.Result{Text: "Permission change cancelled. Session settings are unchanged."}, nil
	}
	modes, err := client.ReadClaudePermissionModes(ctx, s.session.CWD)
	if err != nil {
		return protocol.Result{}, err
	}
	choices := claudePermissionOptions(modes)
	if args == "" {
		result := protocol.Result{Text: "Choose native Claude Code permissions for this session. Changes apply to subsequent turns."}
		if len(choices) == 0 {
			result.Text = "This Claude Code runtime does not advertise permission choices."
			return result, nil
		}
		result.Permissions = &protocol.PermissionMenu{Options: append(choices, protocol.PermissionOption{ID: "cancel", Label: "Cancel"})}
		return result, result.Permissions.Validate()
	}
	lookup := args
	if args == "confirm-bypass-permissions" {
		lookup = "bypassPermissions"
	}
	var selected *protocol.PermissionOption
	for i := range choices {
		if choices[i].ID == lookup {
			selected = &choices[i]
			break
		}
	}
	if selected == nil {
		return protocol.Result{}, validationError("this native Claude permission mode is unavailable; run /permissions to see current choices")
	}
	if args == "bypassPermissions" {
		return protocol.Result{Text: "Bypass Claude Code permission checks for this session? Claude Code will run tool actions without asking for permission.", Permissions: &protocol.PermissionMenu{Options: []protocol.PermissionOption{{ID: "confirm-bypass-permissions", Label: "Bypass permissions"}, {ID: "cancel", Label: "Cancel"}}}}, nil
	}
	if err := s.ensureClaudeSettingsIdle(ctx, client); err != nil {
		return protocol.Result{}, err
	}
	mode := selected.ID
	if err := client.UpdateClaudeThreadSettings(ctx, s.session.ThreadID, nil, nil, &mode); err != nil {
		return protocol.Result{}, err
	}
	return protocol.Result{Text: "Session permissions set to “" + selected.Label + "” for subsequent turns."}, nil
}
