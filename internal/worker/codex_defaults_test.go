package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func putCodexDefaults(t *testing.T, a *Agent, defaults protocol.CodexSessionDefaults) {
	t.Helper()
	result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", &protocol.WorkerRuntimeSettingsUpdate{CodexDefaults: &defaults}))
	if result.ErrorCode != "" || result.Settings == nil || result.Settings.Validate() != nil || result.Settings.CodexDefaults == nil || *result.Settings.CodexDefaults != defaults {
		t.Fatalf("save defaults: %#v", result)
	}
}

func TestCodexDefaultsPersistAcrossAgentRestartWithoutChangingActiveSessions(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.StateFile = filepath.Join(t.TempDir(), "worker.db")
	nativeHome := t.TempDir()
	t.Setenv("CODEX_HOME", nativeHome)
	nativeConfig := []byte("model = \"native-model\"\n")
	if err := os.WriteFile(filepath.Join(nativeHome, "config.toml"), nativeConfig, 0600); err != nil {
		t.Fatal(err)
	}
	session := installSession(a, runtime, "running-existing", "active-turn")
	before, err := a.store.ListSessions(runtime.ID)
	if err != nil {
		t.Fatal(err)
	}
	defaults := protocol.CodexSessionDefaults{Model: "default-model", ReasoningEffort: "high", PermissionLevel: "read-only"}
	putCodexDefaults(t, a, defaults)
	after, err := a.store.ListSessions(runtime.ID)
	if err != nil || !reflect.DeepEqual(before, after) || session.ActiveTurnID == "" || hasCall(server.Calls(), "thread/settings/update") {
		t.Fatalf("saving future defaults changed a running session: %#v %v", after, err)
	}
	info, err := os.Stat(codexDefaultsPath(a.cfg))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private defaults file: %v, %v", info, err)
	}
	restarted, err := NewAgent(a.cfg, a.store, a.log)
	if err != nil {
		t.Fatal(err)
	}
	options, err := restarted.newThreadOptions(runtime.DefaultCWD, "legacy")
	if err != nil || options.Model != defaults.Model || options.ReasoningEffort != defaults.ReasoningEffort || options.PermissionProfile != ":read-only" || options.Sandbox != "" {
		t.Fatalf("restart did not recover session defaults: %#v %v", options, err)
	}
	result := restarted.runtimeSettings(t.Context(), claudeSettingsRequest(restarted, "get", nil))
	if result.ErrorCode != "" || result.Settings == nil || result.Settings.Provider != "codex" || result.Settings.CodexDefaults == nil || *result.Settings.CodexDefaults != defaults {
		t.Fatalf("restart defaults projection: %#v", result)
	}
	nativeAfter, err := os.ReadFile(filepath.Join(nativeHome, "config.toml"))
	if err != nil || string(nativeAfter) != string(nativeConfig) {
		t.Fatalf("worker defaults changed native config: %s %v", nativeAfter, err)
	}
	other := a.cfg
	other.StateFile = filepath.Join(filepath.Dir(a.cfg.StateFile), "other-worker.db")
	if got, err := readCodexDefaults(codexDefaultsPath(other)); err != nil || got != (protocol.CodexSessionDefaults{}) {
		t.Fatalf("another worker inherited these defaults: %#v %v", got, err)
	}
}

func TestCodexNewAndWebUIClearReadFreshDefaultSnapshot(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.StateFile = filepath.Join(t.TempDir(), "worker.db")
	defaults := protocol.CodexSessionDefaults{Model: "worker-model", ReasoningEffort: "high", PermissionLevel: "full-access"}
	putCodexDefaults(t, a, defaults)
	create := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	if ack, err := a.HandleCommand(t.Context(), create); err != nil || ack.Status != "accepted" {
		t.Fatalf("Telegram new: %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(create.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	assertStart := func(model, effort, permission string) {
		t.Helper()
		var params map[string]any
		for _, call := range server.Calls() {
			if call.Method == "thread/start" {
				if err := json.Unmarshal(call.Params, &params); err != nil {
					t.Fatal(err)
				}
			}
		}
		config, _ := params["config"].(map[string]any)
		if params["model"] != model || config["model_reasoning_effort"] != effort || params["permissions"] != permission || params["approvalsReviewer"] != "user" || params["sandbox"] != nil {
			t.Fatalf("new thread defaults = %#v", params)
		}
	}
	assertStart("worker-model", "high", ":danger-full-access")
	parent := installSession(a, runtime, "custom-parent", "")
	actor := a.actorForThread(runtime.ID, parent.ThreadID)
	customModel, customEffort := "session-override", "low"
	client, _, _ := a.manager.Client(runtime.ID)
	if err := client.UpdateThreadSettings(t.Context(), parent.ThreadID, codexadapter.ThreadSettingsUpdate{Model: &customModel, Effort: &customEffort}); err != nil {
		t.Fatal(err)
	}
	for i, defaults := range []protocol.CodexSessionDefaults{
		{Model: "next-worker-model", ReasoningEffort: "medium", PermissionLevel: "read-only"},
		{Model: "worker-model", ReasoningEffort: "high", PermissionLevel: "ask-for-approval"},
	} {
		putCodexDefaults(t, a, defaults)
		threadID := []string{"clear-first", "clear-second"}[i]
		if err := server.SetMethodResult("thread/start", map[string]any{"thread": map[string]any{"id": threadID, "cwd": parent.CWD, "status": "idle"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := actor.webUINewSession(t.Context(), client, `{"name":"Fresh after clear"}`); err != nil {
			t.Fatal(err)
		}
		permission := []string{":read-only", ":workspace"}[i]
		assertStart(defaults.Model, defaults.ReasoningEffort, permission)
	}
	if countCall(server.Calls(), "thread/start") != 3 || countCall(server.Calls(), "thread/settings/update") != 1 {
		t.Fatal("creation applied defaults through a later settings mutation")
	}
}

func TestCodexDefaultPermissionPresetsAndNativeModelDefaults(t *testing.T) {
	for _, tc := range []struct{ level, profile, sandbox, policy string }{
		{"", "", "workspace-write", "on-request"},
		{"ask-for-approval", ":workspace", "", "on-request"},
		{"read-only", ":read-only", "", "on-request"},
		{"full-access", ":danger-full-access", "", "never"},
	} {
		options := codexDefaultThreadOptions(protocol.CodexSessionDefaults{PermissionLevel: tc.level}, "/work", "legacy")
		if options.PermissionProfile != tc.profile || options.Sandbox != tc.sandbox || options.ApprovalPolicy != tc.policy || options.ApprovalsReviewer != "user" || options.Model != "" || options.ReasoningEffort != "" || options.CWD != "/work" || options.HistoryMode != "legacy" {
			t.Fatalf("permission %q = %#v", tc.level, options)
		}
	}
}

func TestCodexDefaultsRejectInvalidOrUnsafeFilesAndAllowPrivateRepair(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.StateFile = filepath.Join(t.TempDir(), "worker.db")
	path := codexDefaultsPath(a.cfg)
	for _, raw := range []string{`null`, `{`, `{"permission_level":"danger-full-access"}`, `{"model":"ok","unknown":true}`, `{} {}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.newThreadOptions(runtime.DefaultCWD, ""); err == nil {
			t.Fatalf("invalid defaults allowed creation: %s", raw)
		}
		if _, err := NewAgent(a.cfg, a.store, a.log); err != nil {
			t.Fatalf("corrupt defaults blocked worker recovery: %v", err)
		}
		if got := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "get", nil)); got.ErrorCode != "unavailable" {
			t.Fatalf("invalid defaults read: %#v", got)
		}
	}
	putCodexDefaults(t, a, protocol.CodexSessionDefaults{PermissionLevel: "read-only"})
	before, _ := os.ReadFile(path)
	invalid := protocol.CodexSessionDefaults{ReasoningEffort: "not-effort"}
	if got := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", &protocol.WorkerRuntimeSettingsUpdate{CodexDefaults: &invalid})); got.ErrorCode != "invalid_settings" {
		t.Fatalf("invalid update accepted: %#v", got)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || hasCall(server.Calls(), "thread/start") {
		t.Fatal("invalid defaults caused partial creation or persistence")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCodexDefaults(path); err == nil {
		t.Fatal("public defaults file accepted")
	}
	if err := saveCodexDefaults(path, protocol.CodexSessionDefaults{}); err == nil {
		t.Fatal("public defaults file overwritten")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readCodexDefaults(path); err == nil {
		t.Fatal("symlink defaults accepted")
	}
	if err := saveCodexDefaults(path, protocol.CodexSessionDefaults{}); err == nil {
		t.Fatal("symlink defaults overwritten")
	}
}

func TestNativeSessionResetReplacesCarriedDefaultsAndPreservesResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db.codex-defaults.json")
	start := []byte(`{"id":1,"method":"thread/start","params":{"cwd":"/work","historyMode":"legacy","model":"old-session-model","sandbox":"danger-full-access","permissions":":danger-full-access","approvalPolicy":"never","approvalsReviewer":"auto_review","config":{"model":"older-model","model_reasoning_effort":"low","sandbox_mode":"danger-full-access","personality":"none"}}}`)
	if forwarded, err := applySavedNativeCodexDefaults(start, path); err != nil || string(forwarded) != string(start) {
		t.Fatalf("worker without configured defaults changed native start: %s %v", forwarded, err)
	}
	for _, defaults := range []protocol.CodexSessionDefaults{
		{Model: "worker-model", ReasoningEffort: "high", PermissionLevel: "read-only"},
		{},
	} {
		if err := saveCodexDefaults(path, defaults); err != nil {
			t.Fatal(err)
		}
		forwarded, err := applySavedNativeCodexDefaults(start, path)
		if err != nil {
			t.Fatal(err)
		}
		var msg struct {
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(forwarded, &msg); err != nil {
			t.Fatal(err)
		}
		params := msg.Params
		config, _ := params["config"].(map[string]any)
		if params["cwd"] != "/work" || params["historyMode"] != "legacy" || params["approvalsReviewer"] != "user" || params["approvalPolicy"] != "on-request" || config["personality"] != "none" || config["model"] != nil || config["sandbox_mode"] != nil {
			t.Fatalf("native reset lost routing or retained inherited permissions: %#v", params)
		}
		if defaults.Model != "" {
			if params["model"] != defaults.Model || config["model_reasoning_effort"] != defaults.ReasoningEffort || params["permissions"] != ":read-only" || params["sandbox"] != nil {
				t.Fatalf("native reset ignored saved defaults: %#v", params)
			}
		} else if params["model"] != nil || config["model_reasoning_effort"] != nil || params["permissions"] != nil || params["sandbox"] != "workspace-write" {
			t.Fatalf("empty saved defaults did not restore fresh native defaults: %#v", params)
		}
	}
	resume := []byte(`{"id":2,"method":"thread/resume","params":{"threadId":"existing","model":"session-override"}}`)
	if got, err := applySavedNativeCodexDefaults(resume, path); err != nil || string(got) != string(resume) {
		t.Fatalf("resuming existing session reapplied defaults: %s %v", got, err)
	}
}

func TestNativeProxyRejectsCorruptDefaultsBeforeCreatingThread(t *testing.T) {
	proxy, path, requests := newNativeProxyTest(t)
	defaultsPath := filepath.Join(t.TempDir(), "worker.db.codex-defaults.json")
	if err := os.WriteFile(defaultsPath, []byte(`{"permission_level":"unknown"}`), 0600); err != nil {
		t.Fatal(err)
	}
	proxy.mu.Lock()
	proxy.newThreadDefaults = func(payload []byte) ([]byte, error) { return applySavedNativeCodexDefaults(payload, defaultsPath) }
	proxy.mu.Unlock()
	client := dialNativeProxy(t, path)
	nativeWrite(t, client, `{"id":1,"method":"thread/start","params":{"cwd":"/work"}}`)
	if got := nativeRead(t, client); !strings.Contains(got, "Codex session defaults are unavailable") {
		t.Fatalf("native invalid defaults error: %s", got)
	}
	select {
	case request := <-requests:
		t.Fatalf("invalid defaults created a partial thread: %s", request.payload)
	default:
	}
	if err := saveCodexDefaults(defaultsPath, protocol.CodexSessionDefaults{Model: "repaired", ReasoningEffort: "high", PermissionLevel: "full-access"}); err != nil {
		t.Fatal(err)
	}
	nativeWrite(t, client, `{"id":2,"method":"thread/start","params":{"cwd":"/work","permissions":":read-only","sandbox":"read-only"}}`)
	request := nextNativeRequest(t, requests)
	var msg struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(request.payload, &msg); err != nil || msg.Params["model"] != "repaired" || msg.Params["permissions"] != ":danger-full-access" || msg.Params["sandbox"] != nil || msg.Params["approvalPolicy"] != "never" {
		t.Fatalf("native proxy did not apply repaired defaults: %s %v", request.payload, err)
	}
	nativeWrite(t, request.connection, `{"id":2,"result":{"thread":{"id":"created"}}}`)
	_ = nativeRead(t, client)
}

func TestClaudeRuntimeSettingsRejectCodexDefaults(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	defaults := protocol.CodexSessionDefaults{}
	got := a.runtimeSettings(context.Background(), claudeSettingsRequest(a, "put", &protocol.WorkerRuntimeSettingsUpdate{CodexDefaults: &defaults}))
	if got.ErrorCode != "invalid_settings" {
		t.Fatalf("Claude worker accepted Codex defaults: %#v", got)
	}
	if _, err := os.Stat(codexDefaultsPath(a.cfg)); !os.IsNotExist(err) {
		t.Fatalf("Claude wrote Codex defaults: %v", err)
	}
}
