package worker

import (
	"os"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestClaudeRuntimeDefaultEffortRequiresSelectedModelAdvertisement(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	for _, tc := range []struct {
		model, effort string
		valid         bool
	}{
		{"worker-next", "high", true},
		{"worker-next", "xhigh", false},
		{"no-effort-model", "low", false},
		{"provider/unadvertised-model", "high", false},
		{"provider/unadvertised-model", "", true},
		{"claude-opus-test", "max", true},
		{"claude-opus-resolved-test", "xhigh", true},
		{"", "high", true},
		{"", "max", false},
	} {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			path := claudeSettingsPath(a.cfg)
			before, readErr := os.ReadFile(path)
			update := &protocol.WorkerRuntimeSettingsUpdate{Model: tc.model, ReasoningEffort: tc.effort, AuthMode: "login", PermissionMode: "default"}
			result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
			if tc.valid {
				if result.ErrorCode != "" || result.Settings == nil || result.Settings.Model != tc.model || result.Settings.ReasoningEffort != tc.effort {
					t.Fatalf("advertised default choice rejected: %#v", result)
				}
				return
			}
			if result.ErrorCode != "invalid_settings" || result.Settings != nil {
				t.Fatalf("unadvertised default effort accepted: %#v", result)
			}
			after, afterErr := os.ReadFile(path)
			if (readErr == nil) != (afterErr == nil) || string(after) != string(before) {
				t.Fatal("invalid default effort changed saved settings")
			}
		})
	}
}

func TestClaudeRuntimeAccountSwitchRequiresNativeDefaultBeforeSelectingEffort(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	initial := &protocol.WorkerRuntimeSettingsUpdate{Model: "claude-opus-test", ReasoningEffort: "high", AuthMode: "login", PermissionMode: "default"}
	if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", initial)); result.ErrorCode != "" {
		t.Fatal(result.ErrorCode)
	}
	path := claudeSettingsPath(a.cfg)
	credential := "synthetic-new-provider-credential"
	next := &protocol.WorkerRuntimeSettingsUpdate{Model: "claude-opus-test", ReasoningEffort: "high", BaseURL: "https://new-provider.example.test", AuthMode: "api_key", PermissionMode: "default", APIKey: &credential}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The old account advertised this exact row and effort; that evidence is
	// insufficient after endpoint, authentication or active credential changes.
	result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", next))
	if result.ErrorCode != "invalid_settings" {
		t.Fatalf("new provider reused the previous account's catalog: %#v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("rejected provider change partially saved settings", err)
	}
	next.ReasoningEffort = ""
	if result = a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", next)); result.ErrorCode != "" {
		t.Fatalf("initial custom provider setup with native default failed: %#v", result)
	}
	next.ReasoningEffort = "high"
	next.APIKey = nil
	if result = a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", next)); result.ErrorCode != "" {
		t.Fatalf("current account advertised effort rejected after setup: %#v", result)
	}
	for _, change := range []string{"active credential", "authentication mode", "endpoint"} {
		t.Run(change, func(t *testing.T) {
			update := *next
			switch change {
			case "active credential":
				rotated := "synthetic-rotated-provider-key"
				update.APIKey = &rotated
			case "authentication mode":
				update.AuthMode = "auth_token"
			case "endpoint":
				update.BaseURL = "https://another-provider.example.test"
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", &update)); result.ErrorCode != "invalid_settings" {
				t.Fatalf("account change reused stale effort metadata: %#v", result)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("rejected account change partially saved settings", err)
			}
		})
	}
}

func TestClaudeRuntimeBudgetUpdatesAndInactiveLoginKeyDoNotChangeInferenceAccount(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	inactive := "synthetic-unused-login-credential"
	update := &protocol.WorkerRuntimeSettingsUpdate{Model: "claude-opus-test", ReasoningEffort: "high", AuthMode: "login", PermissionMode: "default", APIKey: &inactive}
	if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "" {
		t.Fatalf("inactive login key was treated as an account change: %#v", result)
	}
	nextInactive := "synthetic-next-unused-login-credential"
	update.APIKey = &nextInactive
	if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "" {
		t.Fatalf("inactive saved key rotation invalidated native catalog: %#v", result)
	}
	adminKey := "synthetic-read-only-admin-key"
	update.APIKey = nil
	update.EnterpriseBudget = &protocol.ClaudeEnterpriseBudgetUpdate{Enabled: true, UserID: "user_budgetMember", APIKey: &adminKey}
	result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	if result.ErrorCode != "" || result.Settings == nil || result.Settings.ReasoningEffort != "high" || result.Settings.EnterpriseBudget == nil || !result.Settings.EnterpriseBudget.Enabled {
		t.Fatalf("budget-only update was treated as an inference account switch: %#v", result)
	}
}
