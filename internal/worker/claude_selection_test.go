package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func installClaudeSelectionCatalog(t *testing.T, server *codextest.Server, threadID string) {
	t.Helper()
	for method, result := range map[string]any{
		"model/list": map[string]any{"data": []any{
			map[string]any{"id": "claude-opus-test", "model": "claude-opus-test", "displayName": "Claude Opus test", "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "low"}, map[string]any{"reasoningEffort": "high"}, map[string]any{"reasoningEffort": "max"}}},
		}},
		"config/read":        map[string]any{"config": map[string]any{"supportedPermissionModes": []string{"default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions"}}},
		"thread/status/read": map[string]any{"threadId": threadID, "claudeStatus": map[string]any{"provider": "claude", "model": "claude-opus-test", "reasoningEffort": "high", "permissionMode": "default"}},
	} {
		if err := server.SetMethodResult(method, result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeModelMenusApplyOnlyFinalSelectionAndAllowCustomID(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "claude-model-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	installClaudeSelectionCatalog(t, server, actor.session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	for _, args := range []string{"", "--menu claude-opus-test", "--cancel"} {
		result, err := actor.executeCodexCommand(t.Context(), client, "model", args)
		if err != nil {
			t.Fatalf("browse %q: %v", args, err)
		}
		if args == "--menu claude-opus-test" && !modelOptionPresent(result, "claude-opus-test max", "Max") {
			t.Fatalf("native reasoning choice omitted: %#v", result)
		}
	}
	if hasCall(server.Calls(), "thread/resume") || hasCall(server.Calls(), "thread/settings/update") {
		t.Fatal("model menu changed session before final selection")
	}
	for _, args := range []string{"claude-opus-test high", "provider/custom[1m]"} {
		if _, err := actor.executeCodexCommand(t.Context(), client, "model", args); err != nil {
			t.Fatalf("apply %q: %v", args, err)
		}
	}
	var updates []map[string]any
	for _, call := range server.Calls() {
		if call.Method == "thread/settings/update" {
			var params map[string]any
			if json.Unmarshal(call.Params, &params) != nil {
				t.Fatal(string(call.Params))
			}
			updates = append(updates, params)
		}
	}
	if len(updates) != 2 || updates[0]["threadId"] != actor.session.ThreadID || updates[0]["model"] != "claude-opus-test" || updates[0]["effort"] != "high" || updates[1]["model"] != "provider/custom[1m]" || updates[1]["effort"] != "" {
		t.Fatalf("settings update was not exact target/model/effort: %#v", updates)
	}
	before := len(updates)
	for _, args := range []string{"provider/custom[1m] high", "claude-opus-test xhigh", "bad model high", "--invalid", "-option"} {
		if _, err := actor.executeCodexCommand(t.Context(), client, "model", args); err == nil {
			t.Fatalf("unadvertised or malformed choice accepted: %q", args)
		}
	}
	if countCall(server.Calls(), "thread/settings/update") != before {
		t.Fatal("invalid selection changed session")
	}
}

func TestClaudeReasoningUsesCurrentModelAdvertisementAndNativeReset(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "claude-reasoning-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	installClaudeSelectionCatalog(t, server, actor.session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(t.Context(), client, "reasoning", "")
	if err != nil || result.Text != "Effort: High" {
		t.Fatalf("current effort: %#v %v", result, err)
	}
	for _, args := range []string{"low", "default"} {
		if _, err := actor.executeCodexCommand(t.Context(), client, "reasoning", args); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range []string{"xhigh", "none", "ultra", "low high"} {
		if _, err := actor.executeCodexCommand(t.Context(), client, "reasoning", args); err == nil {
			t.Fatalf("unadvertised reasoning accepted: %q", args)
		}
	}
	var efforts []string
	for _, call := range server.Calls() {
		if call.Method == "thread/settings/update" {
			var params map[string]string
			if json.Unmarshal(call.Params, &params) != nil {
				t.Fatal(string(call.Params))
			}
			if len(params) != 2 || params["threadId"] != actor.session.ThreadID {
				t.Fatalf("reasoning update changed another setting: %s", call.Params)
			}
			efforts = append(efforts, params["effort"])
		}
	}
	if len(efforts) != 2 || efforts[0] != "low" || efforts[1] != "" {
		t.Fatalf("native effort reset lost: %#v", efforts)
	}
}

func TestClaudePermissionsAdvertisedOnlyAndBypassRequiresConfirmation(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "claude-permissions-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	installClaudeSelectionCatalog(t, server, actor.session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(t.Context(), client, "permissions", "")
	if err != nil || result.Permissions.Validate() != nil {
		t.Fatalf("menu: %#v %v", result, err)
	}
	for _, mode := range []string{"default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions", "cancel"} {
		if !permissionOptionPresent(result, mode) {
			t.Fatalf("advertised mode missing: %q", mode)
		}
	}
	if _, err := actor.executeCodexCommand(t.Context(), client, "permissions", "read-only"); err == nil {
		t.Fatal("Codex read-only silently mapped to native plan")
	}
	result, err = actor.executeCodexCommand(t.Context(), client, "permissions", "bypassPermissions")
	if err != nil || !permissionOptionPresent(result, "confirm-bypass-permissions") {
		t.Fatalf("bypass confirmation: %#v %v", result, err)
	}
	if hasCall(server.Calls(), "thread/settings/update") || hasCall(server.Calls(), "thread/resume") {
		t.Fatal("confirmation request changed settings")
	}
	if _, err := actor.executeCodexCommand(t.Context(), client, "permissions", "confirm-bypass-permissions"); err != nil {
		t.Fatal(err)
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/settings/update" {
			var params map[string]string
			if json.Unmarshal(call.Params, &params) != nil || len(params) != 2 || params["threadId"] != actor.session.ThreadID || params["permissionMode"] != "bypassPermissions" {
				t.Fatalf("incorrect native permission update: %s", call.Params)
			}
		}
	}
	if err := server.SetMethodResult("config/read", map[string]any{"config": map[string]any{"supportedPermissionModes": []string{"default"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.executeCodexCommand(t.Context(), client, "permissions", "confirm-bypass-permissions"); err == nil {
		t.Fatal("stale confirmation bypassed current capability advertisement")
	}
	result, err = actor.executeCodexCommand(t.Context(), client, "permissions", "")
	if err != nil || permissionOptionPresent(result, "auto") || permissionOptionPresent(result, "plan") {
		t.Fatalf("unadvertised modes offered: %#v %v", result, err)
	}
}

func TestClaudeSelectionFencesTargetActiveAndQueuedWork(t *testing.T) {
	for _, state := range []string{"active", "queued", "approval", "awaiting"} {
		t.Run(state, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			a.cfg.AgentKind = "claude"
			actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "claude-busy-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
			switch state {
			case "active":
				actor.session.ActiveTurnID = "turn"
				actor.session.State = "running"
			case "queued":
				actor.queue = []protocol.Command{{ID: "queued"}}
			case "approval":
				actor.session.State = "waiting_approval"
			case "awaiting":
				actor.awaitingTurnStart = true
			}
			installClaudeSelectionCatalog(t, server, actor.session.ThreadID)
			client, _, _ := a.manager.Client(runtime.ID)
			for name, args := range map[string]string{"model": "claude-opus-test high", "reasoning": "low", "permissions": "plan"} {
				if _, err := actor.executeCodexCommand(t.Context(), client, name, args); err == nil {
					t.Fatalf("%s mutation allowed during %s", name, state)
				}
			}
			if hasCall(server.Calls(), "thread/settings/update") || hasCall(server.Calls(), "thread/resume") {
				t.Fatal("busy target changed settings")
			}
			result, err := actor.executeCodexCommand(t.Context(), client, "model", "")
			if err != nil || result.ModelMenu == nil {
				t.Fatalf("read-only menu blocked: %#v %v", result, err)
			}
			result, err = actor.executeCodexCommand(t.Context(), client, "permissions", "bypassPermissions")
			if err != nil || !strings.Contains(result.Text, "Bypass") {
				t.Fatalf("confirmation browsing blocked: %#v %v", result, err)
			}
		})
	}
	if codexCommandNeedsIdle("permissions", "bypassPermissions") {
		t.Fatal("bypass confirmation menu requires idle")
	}
	if !codexCommandNeedsIdle("permissions", "confirm-bypass-permissions") {
		t.Fatal("confirmed bypass lacks idle admission fence")
	}
}

func TestClaudeSettingsSelectionDoesNotFenceUnrelatedActiveSession(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "claude-idle-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	a.sessions["unrelated-running-session"] = &sessionActor{agent: a, runtime: runtime, session: protocol.Session{ThreadID: "unrelated-running-thread", State: "running", ActiveTurnID: "active-other"}}
	installClaudeSelectionCatalog(t, server, actor.session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	for name, args := range map[string]string{"model": "claude-opus-test high", "reasoning": "low", "permissions": "plan"} {
		if _, err := actor.executeCodexCommand(t.Context(), client, name, args); err != nil {
			t.Fatalf("unrelated active thread blocked target %s: %v", name, err)
		}
	}
	if countCall(server.Calls(), "thread/settings/update") != 3 {
		t.Fatal("target settings were not applied")
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/settings/update" && !strings.Contains(string(call.Params), `"threadId":"claude-idle-target"`) {
			t.Fatalf("settings escaped target: %s", call.Params)
		}
	}
	sibling := a.sessions["unrelated-running-session"]
	if sibling.session.ActiveTurnID != "active-other" || sibling.session.State != "running" {
		t.Fatal("target setting changed unrelated active session")
	}
}

func TestClaudeAliasMenusShowResolvedProviderModel(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "resolved-menu-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	if err := server.SetMethodResult("model/list", map[string]any{"data": []any{map[string]any{"id": "sonnet", "model": "sonnet", "resolvedModel": "provider/shared-model", "displayName": "Sonnet"}}}); err != nil {
		t.Fatal(err)
	}
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(t.Context(), client, "model", "")
	if err != nil || !modelOptionPresent(result, "--menu sonnet", "Sonnet → provider/shared-model") {
		t.Fatalf("custom-provider alias destination hidden: %#v %v", result, err)
	}
	result, err = actor.executeCodexCommand(t.Context(), client, "model", "--menu sonnet")
	if err != nil || !strings.Contains(result.Text, "Sonnet → provider/shared-model") {
		t.Fatalf("reasoning step hid alias destination: %#v %v", result, err)
	}
	if hasCall(server.Calls(), "thread/settings/update") {
		t.Fatal("resolved alias menu changed settings")
	}
}

func TestClaudeModelAllowsAdvertisedResolvedIDWithoutReplacingItsVersionPin(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "resolved-pin-target", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	if err := server.SetMethodResult("model/list", map[string]any{"data": []any{map[string]any{
		"id": "opus", "model": "opus", "resolvedModel": "claude-opus-version-pinned", "displayName": "Opus",
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "max"}},
	}}}); err != nil {
		t.Fatal(err)
	}
	client, _, _ := a.manager.Client(runtime.ID)
	if _, err := actor.executeCodexCommand(t.Context(), client, "model", "claude-opus-version-pinned max"); err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, call := range server.Calls() {
		if call.Method == "thread/settings/update" {
			updates++
			var params map[string]string
			if err := json.Unmarshal(call.Params, &params); err != nil || params["model"] != "claude-opus-version-pinned" || params["effort"] != "max" {
				t.Fatalf("version pin replaced or advertised reasoning lost: %s %v", call.Params, err)
			}
		}
	}
	if updates != 1 {
		t.Fatal("selection did not apply exactly once")
	}
}
