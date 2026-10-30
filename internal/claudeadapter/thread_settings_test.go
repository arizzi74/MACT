package claudeadapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

func settingsString(value string) *string { return &value }

func TestNativeCatalogProbeAdvertisesExactCapabilitiesWithoutTurns(t *testing.T) {
	cfg := testConfig(t)
	cfg.LoadSettings = func() (LaunchSettings, error) {
		return LaunchSettings{Model: "opus", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "PRIVATE_CATALOG_TOKEN"}}, nil
	}
	r := startTest(t, cfg)
	models, err := r.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sonnet, opus bool
	for _, model := range models {
		switch model.Model {
		case "sonnet":
			sonnet = reflect.DeepEqual(model.ReasoningEfforts, []string{"low", "medium", "high", "max"})
		case "opus":
			opus = model.Default && reflect.DeepEqual(model.ReasoningEfforts, []string{"low", "medium", "high", "xhigh", "max"})
		}
	}
	if !sonnet || !opus {
		t.Fatalf("incorrect runtime effort catalog: %+v", models)
	}
	modes, err := r.ReadClaudePermissionModes(context.Background(), "")
	if err != nil || !reflect.DeepEqual(modes, nativePermissionModes) {
		t.Fatalf("modes=%v err=%v", modes, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkingDirectory, "actions.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("metadata discovery launched a user session: %v", err)
	}
	if len(r.server.threads) != 0 {
		t.Fatal("metadata discovery created an adapter session")
	}
	wire, _ := json.Marshal(r.server.modelList(LaunchSettings{Model: "opus"}))
	if strings.Contains(string(wire), "PRIVATE_CATALOG_TOKEN") {
		t.Fatal("native catalog exposed account credentials")
	}
}

func TestSessionSettingsOverrideWorkerDefaultsAndSurviveResume(t *testing.T) {
	cfg := testConfig(t)
	defaults := LaunchSettings{Model: "opus", ReasoningEffort: "high", PermissionMode: "acceptEdits", Env: map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": "max"}}
	cfg.LoadSettings = func() (LaunchSettings, error) { return defaults, nil }
	r := startTest(t, cfg)
	thread, err := r.StartThread(context.Background(), codexadapter.ThreadOptions{Model: "sonnet", ReasoningEffort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	runTurn(t, r, thread.ID, "first")
	r.server.mu.Lock()
	first := r.server.runners[thread.ID]
	status := r.server.threads[thread.ID].statusSnapshot(nil, nil)
	r.server.mu.Unlock()
	if status.Model == nil || *status.Model != "sonnet" || status.ReasoningEffort == nil || *status.ReasoningEffort != "medium" {
		t.Fatalf("worker default replaced session preference: %+v", status)
	}
	// Another session may be running while this idle session is updated.
	sibling := startThread(t, r)
	if _, err := r.StartTurn(context.Background(), sibling.ID, "block"); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, settingsString("opus"), settingsString("max"), settingsString("plan")); err != nil {
		t.Fatal(err)
	}
	r.server.mu.Lock()
	if r.server.runners[thread.ID] != nil || r.server.runners[sibling.ID] == nil || r.server.threads[sibling.ID].active() == nil {
		t.Fatal("session update affected the wrong subprocess")
	}
	r.server.mu.Unlock()
	<-first.done
	runTurn(t, r, thread.ID, "second")
	log, err := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var launches []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		var item map[string]any
		_ = json.Unmarshal([]byte(line), &item)
		if item["launch"] == true && item["session"] == thread.ID {
			launches = append(launches, item)
		}
	}
	if len(launches) != 2 || launches[0]["model"] != "sonnet" || launches[0]["effortEnv"] != "medium" || launches[1]["resume"] != true || launches[1]["model"] != "opus" || launches[1]["effort"] != "max" || launches[1]["permissionMode"] != "plan" {
		t.Fatalf("session subprocess settings=%+v", launches)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	defaults.Model, defaults.ReasoningEffort, defaults.PermissionMode = "haiku", "low", "dontAsk"
	r2 := startTest(t, cfg)
	resumed, err := r2.ResumeThread(context.Background(), thread.ID, codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Settings struct {
			Model      string `json:"model"`
			Effort     string `json:"effort"`
			Permission string `json:"permissionMode"`
		} `json:"threadSettings"`
		Status ClaudeStatus `json:"claudeStatus"`
	}
	if err := json.Unmarshal(resumed.Raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.Model != "opus" || snapshot.Settings.Effort != "max" || snapshot.Settings.Permission != "plan" || snapshot.Status.ReasoningEffort == nil || *snapshot.Status.ReasoningEffort != "max" {
		t.Fatalf("persisted session override lost: %s", resumed.Raw)
	}
	if err := r2.UpdateClaudeThreadSettings(context.Background(), thread.ID, settingsString(""), settingsString(""), settingsString("")); err != nil {
		t.Fatal(err)
	}
	runTurn(t, r2, thread.ID, "after default reset")
	r2.server.mu.Lock()
	saved := r2.server.threads[thread.ID]
	if saved.RequestedModel != "" || saved.RequestedEffort == nil || *saved.RequestedEffort != "" || saved.RequestedPermissionMode != "" || saved.StatusConfigModel != "haiku" || saved.StatusConfigEffort != "" || saved.StatusConfigPermission != "dontAsk" || saved.ClaudeStatus.ReasoningEffort == nil || *saved.ClaudeStatus.ReasoningEffort != "" {
		t.Fatalf("reset failed: %+v", saved)
	}
	r2.server.mu.Unlock()
	log, _ = os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if !strings.Contains(string(log), `"effortEnv":"auto"`) {
		t.Fatal("empty effort restored inherited environment effort")
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	r3 := startTest(t, cfg)
	resumedDefault, err := r3.ResumeThread(context.Background(), thread.ID, codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(resumedDefault.Raw, &snapshot) != nil || snapshot.Status.ReasoningEffort == nil || *snapshot.Status.ReasoningEffort != "" {
		t.Fatalf("explicit default lost on restart: %s", resumedDefault.Raw)
	}
}

func TestSessionSettingsBroadcastAndResumeContract(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	peer := r.server.newPeer(nil)
	defer peer.close()
	r.server.receive(peer, []byte(`{"id":"init","method":"initialize"}`))
	<-peer.queue
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, settingsString("sonnet"), settingsString("high"), settingsString("dontAsk")); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"thread/settings/updated", "thread/claudeStatus/updated"} {
		var event struct {
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
				Settings struct {
					Model  string `json:"model"`
					Effort string `json:"effort"`
					Mode   string `json:"permissionMode"`
				} `json:"threadSettings"`
				Status ClaudeStatus `json:"claudeStatus"`
			} `json:"params"`
		}
		if err := json.Unmarshal(<-peer.queue, &event); err != nil {
			t.Fatal(err)
		}
		if event.Method != method || event.Params.ThreadID != thread.ID {
			t.Fatalf("wrong settings event: %+v", event)
		}
		if method == "thread/settings/updated" && (event.Params.Settings.Model != "sonnet" || event.Params.Settings.Effort != "high" || event.Params.Settings.Mode != "dontAsk") {
			t.Fatalf("settings notification=%+v", event)
		}
		if method == "thread/claudeStatus/updated" && (event.Params.Status.ReasoningEffort == nil || *event.Params.Status.ReasoningEffort != "high") {
			t.Fatalf("status notification=%+v", event)
		}
	}
	request, _ := json.Marshal(map[string]any{"id": "resume", "method": "thread/resume", "params": map[string]any{"threadId": thread.ID}})
	r.server.receive(peer, request)
	var result struct {
		Result struct {
			Model  string `json:"model"`
			Effort string `json:"reasoningEffort"`
			Mode   string `json:"permissionMode"`
		} `json:"result"`
	}
	if err := json.Unmarshal(<-peer.queue, &result); err != nil {
		t.Fatal(err)
	}
	if result.Result.Model != "sonnet" || result.Result.Effort != "high" || result.Result.Mode != "dontAsk" {
		t.Fatalf("normalized resume=%+v", result)
	}
}

func TestSessionSettingsIdleFenceIncludesOnlyTargetActivity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*server, *savedThread)
	}{
		{"active", func(s *server, thread *savedThread) {
			thread.Turns = []*savedTurn{{ID: uuid.NewString(), Status: "inProgress"}}
		}},
		{"queued", func(s *server, thread *savedThread) { thread.Queue = []*queuedInput{{ID: uuid.NewString()}} }},
		{"approval", func(s *server, thread *savedThread) {
			s.pending["pending"] = &permission{Process: &process{threadID: thread.ID}}
		}},
		{"background task", func(s *server, thread *savedThread) {
			s.runners[thread.ID] = &process{tasks: map[string]*backgroundTask{"task": {active: true, background: true}}}
		}},
		{"background snapshot", func(s *server, thread *savedThread) {
			s.runners[thread.ID] = &process{backgroundSet: map[string]string{"task": "run"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := startTest(t, testConfig(t))
			thread := startThread(t, r)
			r.server.mu.Lock()
			saved := r.server.threads[thread.ID]
			test.mutate(r.server, saved)
			before, _ := json.Marshal(saved)
			r.server.mu.Unlock()
			if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, settingsString("sonnet"), nil, nil); err == nil {
				t.Fatal("busy session settings changed")
			}
			r.server.mu.Lock()
			after, _ := json.Marshal(saved)
			delete(r.server.runners, thread.ID)
			delete(r.server.pending, "pending")
			r.server.mu.Unlock()
			if string(before) != string(after) {
				t.Fatal("busy settings rejection changed persisted preferences")
			}
		})
	}
}

func TestSessionSettingsRejectInvalidOrUnadvertisedValuesAtomically(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	for _, raw := range []string{
		`{"model":"sonnet","effort":"xhigh"}`, `{"model":"haiku","effort":"low"}`, `{"model":"gateway/custom","effort":"high"}`,
		`{"effort":"ultra"}`, `{"permissionMode":"full-access"}`, `{"permissionMode":"manual"}`, `{"model":"two words"}`, `{"model":"--print"}`, `{"effort":null}`, `{"sandboxPolicy":{"type":"dangerFullAccess"}}`, `{}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var fields map[string]any
			_ = json.Unmarshal([]byte(raw), &fields)
			fields["threadId"] = thread.ID
			encoded, _ := json.Marshal(fields)
			r.server.mu.Lock()
			saved := r.server.threads[thread.ID]
			before, _ := json.Marshal(saved)
			err := r.server.updateThreadSettings(saved, encoded)
			after, _ := json.Marshal(saved)
			r.server.mu.Unlock()
			if err == nil || string(before) != string(after) {
				t.Fatalf("invalid settings mutated session: %s err=%v", raw, err)
			}
		})
	}
	// Full provider IDs remain valid without guessed effort capabilities.
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, settingsString("arn:aws:bedrock:eu:test/model/claude-opus-4-7"), settingsString(""), nil); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityFallbackAndNativeManualModeNormalization(t *testing.T) {
	for _, help := range []string{
		`  --permission-mode <mode> (choices: "manual", "acceptEdits", "auto", "bypassPermissions", "dontAsk", "plan")`,
		`  --permission-mode <mode> (choices: "default", "acceptEdits", "auto", "bypassPermissions", "dontAsk", "plan")`,
	} {
		if got := permissionModesFromHelp([]byte(help)); !reflect.DeepEqual(got, nativePermissionModes) {
			t.Fatalf("mode normalization=%v", got)
		}
	}
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_NO_CAPABILITIES=1")
	r := startTest(t, cfg)
	modes, err := r.ReadClaudePermissionModes(context.Background(), "")
	if err != nil || !reflect.DeepEqual(modes, []string{"default"}) {
		t.Fatalf("legacy permission fallback=%v %v", modes, err)
	}
	models, err := r.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) < 7 {
		t.Fatal("documented aliases missing in legacy catalog")
	}
	for _, model := range models {
		if len(model.ReasoningEfforts) > 0 {
			t.Fatal("legacy alias invented effort levels")
		}
	}
	thread := startThread(t, r)
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, nil, nil, settingsString("auto")); err == nil {
		t.Fatal("unadvertised permission mode accepted")
	}
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, nil, nil, settingsString("default")); err != nil {
		t.Fatal(err)
	}
}

func TestNativeModelMetadataSanitizesCapabilities(t *testing.T) {
	models := decodeNativeModels(json.RawMessage(`{"models":[{"value":"sonnet","supportsEffort":true,"supportedEffortLevels":["xhigh","low","low","none","max"]},{"value":"legacy","supportsEffort":true},{"value":"sonnet"},{"value":"model with spaces"}]}`))
	if len(models) != 2 || !reflect.DeepEqual(models[0].SupportedEffortLevels, []string{"low", "xhigh", "max"}) || !reflect.DeepEqual(models[1].SupportedEffortLevels, []string{"low", "medium", "high"}) {
		t.Fatalf("native metadata=%+v", models)
	}
}

func TestSessionPermissionLaunchUsesNativeManualAndBypassOptIn(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_MANUAL_MODE=1")
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, nil, nil, settingsString("default")); err != nil {
		t.Fatal(err)
	}
	runTurn(t, r, thread.ID, "manual turn")
	if err := r.UpdateClaudeThreadSettings(context.Background(), thread.ID, nil, nil, settingsString("bypassPermissions")); err != nil {
		t.Fatal(err)
	}
	runTurn(t, r, thread.ID, "confirmed bypass turn")
	log, err := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), `"permissionMode":"manual"`) || !strings.Contains(string(log), `"permissionMode":"bypassPermissions"`) || !strings.Contains(string(log), `"allowBypass":true`) {
		t.Fatalf("native permission startup=%s", log)
	}
	p := nativeStatusProcess(r, thread.ID)
	feedStatus(p, `{"type":"system","subtype":"init","permissionMode":"manual"}`)
	status := readStatus(t, r, thread.ID)
	if status.PermissionMode == nil || *status.PermissionMode != "default" {
		t.Fatalf("manual status not normalized: %+v", status)
	}
}

func TestSessionSettingsPersistenceFailureKeepsIdleProcessAndPreferences(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	runTurn(t, r, thread.ID, "before failed save")
	r.server.mu.Lock()
	saved := r.server.threads[thread.ID]
	before, _ := json.Marshal(saved)
	proc := r.server.runners[thread.ID]
	state := r.server.cfg.StateDirectory
	r.server.cfg.StateDirectory = filepath.Join(cfg.WorkingDirectory, "missing", "state")
	err := r.server.updateThreadSettings(saved, json.RawMessage(`{"model":"opus","effort":"max"}`))
	r.server.cfg.StateDirectory = state
	after, _ := json.Marshal(saved)
	same := r.server.runners[thread.ID] == proc
	r.server.mu.Unlock()
	if err == nil || !same || string(before) != string(after) {
		t.Fatalf("failed settings persistence changed runtime: err=%v same=%t", err, same)
	}
	runTurn(t, r, thread.ID, "after failed save")
}

func TestAdminDefaultsReloadPreservesExplicitSessionOverrides(t *testing.T) {
	cfg := testConfig(t)
	defaults := LaunchSettings{Model: "sonnet", ReasoningEffort: "medium", PermissionMode: "default"}
	cfg.LoadSettings = func() (LaunchSettings, error) { return defaults, nil }
	r := startTest(t, cfg)
	override := startThread(t, r)
	inherited := startThread(t, r)
	if err := r.UpdateClaudeThreadSettings(context.Background(), override.ID, settingsString("opus"), settingsString("max"), settingsString("plan")); err != nil {
		t.Fatal(err)
	}
	if err := r.WithIdleSettingsUpdate(func() error { defaults = LaunchSettings{Model: "haiku", PermissionMode: "dontAsk"}; return nil }); err != nil {
		t.Fatal(err)
	}
	r.server.mu.Lock()
	defer r.server.mu.Unlock()
	fixed, changed := r.server.threads[override.ID], r.server.threads[inherited.ID]
	if fixed.StatusConfigModel != "opus" || fixed.StatusConfigEffort != "max" || fixed.StatusConfigPermission != "plan" {
		t.Fatal("admin update overwrote session preferences")
	}
	if changed.StatusConfigModel != "haiku" || changed.StatusConfigEffort != "" || changed.StatusConfigPermission != "dontAsk" {
		t.Fatal("new defaults did not refresh inherited session settings")
	}
}
