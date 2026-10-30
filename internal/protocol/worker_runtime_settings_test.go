package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWorkerRuntimeSettingsCredentialSemanticsAndPublicProjection(t *testing.T) {
	for _, value := range []string{
		`{"model":"custom[1m]","base_url":"https://proxy.example.test:8080","auth_mode":"login","permission_mode":"default"}`,
		`{"model":"custom[1m]","base_url":"https://provider.example/v1","auth_mode":"api_key","permission_mode":"acceptEdits","api_key":"private-key"}`,
		`{"model":"","base_url":"","auth_mode":"auth_token","permission_mode":"bypassPermissions","api_key":""}`,
	} {
		var update WorkerRuntimeSettingsUpdate
		if err := json.Unmarshal([]byte(value), &update); err != nil || update.Validate() != nil {
			t.Fatal("valid update rejected")
		}
		reencoded, _ := json.Marshal(update)
		if strings.Contains(value, `"api_key"`) != strings.Contains(string(reencoded), `"api_key"`) {
			t.Fatal("unchanged and clear credential operations were conflated")
		}
	}
	var projection WorkerRuntimeSettings
	if err := json.Unmarshal([]byte(`{"provider":"claude","model":"custom[1m]","base_url":"https://provider.example","auth_mode":"auth_token","permission_mode":"plan","api_key_configured":true,"api_key":"private-key"}`), &projection); err != nil || projection.Validate() != nil {
		t.Fatal("valid public projection rejected")
	}
	encoded, _ := json.Marshal(projection)
	if strings.Contains(string(encoded), "private-key") || strings.Contains(string(encoded), `"api_key":`) {
		t.Fatal("public projection retained credential data")
	}
}

func TestWorkerRuntimeSettingsValidationAndEphemeralEnvelope(t *testing.T) {
	valid := WorkerRuntimeSettingsUpdate{Model: "custom[1m]", BaseURL: "https://provider.example/anthropic", AuthMode: "api_key", PermissionMode: "default"}
	for _, mutate := range []func(*WorkerRuntimeSettingsUpdate){
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "http://provider.example" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "http://127.0.0.1:8080" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "http://[::1]:8080" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "https://user:private-key@provider.example" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "https://provider.example?api_key=private-key" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "https://provider.example#private-key" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "file:///tmp/config" },
		func(s *WorkerRuntimeSettingsUpdate) { s.AuthMode = "shell" },
		func(s *WorkerRuntimeSettingsUpdate) { s.PermissionMode = "--dangerously-skip-permissions" },
		func(s *WorkerRuntimeSettingsUpdate) { s.Model = "model\nsecret" },
		func(s *WorkerRuntimeSettingsUpdate) { s.Model = "--invalid-model" },
		func(s *WorkerRuntimeSettingsUpdate) { key := "key\nsecret"; s.APIKey = &key },
		func(s *WorkerRuntimeSettingsUpdate) { key := strings.Repeat("x", 8193); s.APIKey = &key },
	} {
		update := valid
		mutate(&update)
		if err := update.Validate(); err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal("invalid settings accepted or error quoted a secret")
		}
	}
	request := WorkerRuntimeSettingsRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString(), Action: "put", Settings: &valid}
	if request.Validate() != nil {
		t.Fatal("valid worker request rejected")
	}
	if _, err := NewEnvelope("worker_runtime_settings_request", request); err != nil {
		t.Fatal(err)
	}
	request.Action = "get"
	if request.Validate() == nil {
		t.Fatal("read request accepted a credential payload")
	}
	for _, code := range []string{"unsupported", "invalid_settings", "busy", "save_failed", "unavailable"} {
		result := WorkerRuntimeSettingsResult{RequestID: request.RequestID, ErrorCode: code}
		if result.Validate() != nil {
			t.Fatal("fixed error code rejected")
		}
	}
	if (WorkerRuntimeSettingsResult{RequestID: request.RequestID, ErrorCode: "private-key"}).Validate() == nil {
		t.Fatal("unbounded runtime error accepted")
	}
}

func TestCodexRuntimeSettingsDefaultsAndProviderIsolation(t *testing.T) {
	for _, defaults := range []CodexSessionDefaults{
		{},
		{Model: "gpt-example", ReasoningEffort: "xhigh", PermissionLevel: "full-access"},
		{Model: "provider/model:tag", ReasoningEffort: "none", PermissionLevel: "read-only"},
		{ReasoningEffort: "ultra", PermissionLevel: "ask-for-approval"},
	} {
		update := WorkerRuntimeSettingsUpdate{CodexDefaults: &defaults}
		settings := WorkerRuntimeSettings{Provider: "codex", CodexDefaults: &defaults}
		if err := update.Validate(); err != nil {
			t.Fatalf("valid defaults rejected: %v", err)
		}
		if err := settings.Validate(); err != nil {
			t.Fatalf("valid defaults projection rejected: %v", err)
		}
		data, err := json.Marshal(update)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip WorkerRuntimeSettingsUpdate
		if json.Unmarshal(data, &roundtrip) != nil || roundtrip.Validate() != nil || *roundtrip.CodexDefaults != defaults {
			t.Fatal("defaults did not survive a wire round trip")
		}
		request := WorkerRuntimeSettingsRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString(), Action: "put", Settings: &update}
		if request.Validate() != nil {
			t.Fatal("valid Codex defaults request rejected")
		}
		result := WorkerRuntimeSettingsResult{RequestID: request.RequestID, Settings: &settings}
		if result.Validate() != nil {
			t.Fatal("valid Codex defaults result rejected")
		}
	}
	for _, defaults := range []CodexSessionDefaults{
		{Model: " leading-space"}, {Model: "model\nprivate"}, {Model: "--option"}, {Model: "-option"},
		{Model: strings.Repeat("x", 257)}, {Model: string([]byte{0xff})},
		{ReasoningEffort: "invented"}, {ReasoningEffort: " HIGH"}, {PermissionLevel: "bypassPermissions"},
		{PermissionLevel: "danger-full-access"},
	} {
		if (WorkerRuntimeSettingsUpdate{CodexDefaults: &defaults}).Validate() == nil ||
			(WorkerRuntimeSettings{Provider: "codex", CodexDefaults: &defaults}).Validate() == nil {
			t.Fatalf("invalid Codex defaults accepted: %+v", defaults)
		}
	}
	for _, mutate := range []func(*WorkerRuntimeSettingsUpdate){
		func(s *WorkerRuntimeSettingsUpdate) { s.Model = "claude" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "https://provider.example" },
		func(s *WorkerRuntimeSettingsUpdate) { s.AuthMode = "login" },
		func(s *WorkerRuntimeSettingsUpdate) { s.PermissionMode = "default" },
		func(s *WorkerRuntimeSettingsUpdate) { key := ""; s.APIKey = &key },
	} {
		update := WorkerRuntimeSettingsUpdate{CodexDefaults: &CodexSessionDefaults{}}
		mutate(&update)
		if update.Validate() == nil {
			t.Fatal("mixed provider settings accepted")
		}
	}
	for _, settings := range []WorkerRuntimeSettings{
		{Provider: "codex"},
		{Provider: "codex", CodexDefaults: &CodexSessionDefaults{}, APIKeyConfigured: true},
		{Provider: "claude", CodexDefaults: &CodexSessionDefaults{}, AuthMode: "login", PermissionMode: "default"},
	} {
		if settings.Validate() == nil {
			t.Fatal("inconsistent provider projection accepted")
		}
	}
}

func TestClaudeEffortAndNativePermissionModes(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		settings := WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: "default", ReasoningEffort: effort}
		if err := settings.Validate(); err != nil {
			t.Fatalf("native effort %q: %v", effort, err)
		}
	}
	for _, effort := range []string{"none", "minimal", "ultra", "HIGH", "high\n"} {
		settings := WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: "default", ReasoningEffort: effort}
		if settings.Validate() == nil {
			t.Fatalf("non-native effort accepted: %q", effort)
		}
	}
	for _, mode := range []string{"default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions"} {
		settings := WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: mode}
		if settings.Validate() != nil {
			t.Fatalf("native mode %q rejected", mode)
		}
	}
	for _, mode := range []string{"read-only", "ask-for-approval", "full-access", "accept-edits"} {
		if (WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: mode}).Validate() == nil {
			t.Fatalf("Codex/invalid mode %q accepted", mode)
		}
	}
	if (WorkerRuntimeSettingsUpdate{ReasoningEffort: "high", CodexDefaults: &CodexSessionDefaults{}}).Validate() == nil {
		t.Fatal("Claude effort mixed into Codex defaults")
	}
}

func TestClaudeEnterpriseBudgetCredentialsStayPrivateAndSeparate(t *testing.T) {
	key := "synthetic-Enterprise-Admin-key"
	valid := WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: "default", EnterpriseBudget: &ClaudeEnterpriseBudgetUpdate{Enabled: true, UserID: "user_Ab123", APIKey: &key}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*WorkerRuntimeSettingsUpdate){
		func(s *WorkerRuntimeSettingsUpdate) { s.AuthMode = "api_key" },
		func(s *WorkerRuntimeSettingsUpdate) { s.BaseURL = "https://proxy.example.test" },
		func(s *WorkerRuntimeSettingsUpdate) { s.EnterpriseBudget.UserID = "user_bad/name" },
		func(s *WorkerRuntimeSettingsUpdate) { s.EnterpriseBudget.UserID = "" },
		func(s *WorkerRuntimeSettingsUpdate) { bad := "secret key"; s.EnterpriseBudget.APIKey = &bad },
	} {
		update := valid
		budget := *valid.EnterpriseBudget
		update.EnterpriseBudget = &budget
		mutate(&update)
		if err := update.Validate(); err == nil || strings.Contains(err.Error(), key) {
			t.Fatal("invalid Enterprise enrollment accepted or credential exposed")
		}
	}
	public := WorkerRuntimeSettings{Provider: "claude", AuthMode: "login", PermissionMode: "default", EnterpriseBudget: &ClaudeEnterpriseBudgetSettings{Enabled: true, UserID: "user_Ab123", APIKeyConfigured: true}}
	if err := public.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(public)
	if err != nil || strings.Contains(string(data), key) || strings.Contains(string(data), `"api_key":`) {
		t.Fatalf("private Enterprise credential entered projection: %s %v", data, err)
	}
	public.EnterpriseBudget.APIKeyConfigured = false
	if public.Validate() == nil {
		t.Fatal("enabled Enterprise budget without an Admin key accepted")
	}
}

func TestClaudePermissionCapabilitiesRejectUnknownAndDuplicateModes(t *testing.T) {
	for _, modes := range [][]string{{"read-only"}, {"default", "default"}, {"default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions", "default"}} {
		settings := WorkerRuntimeSettings{Provider: "claude", AuthMode: "login", PermissionMode: "default", SupportedPermissionModes: modes}
		if settings.Validate() == nil {
			t.Fatalf("invalid native mode advertisement accepted: %#v", modes)
		}
	}
}
