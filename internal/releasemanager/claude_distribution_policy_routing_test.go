package releasemanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeClaudeRoutingEnvFixture(t *testing.T, home, key string, value any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"env": map[string]any{key: value}})
	if err != nil {
		t.Fatal(err)
	}
	writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "settings.json"), string(data))
}

func TestClaudeUpdatePolicyCachedRemoteRequiresUnambiguousProvider(t *testing.T) {
	for _, key := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
	} {
		for _, source := range []string{"process", "user"} {
			for _, value := range []string{"1", "true", "YES", "On", "0", "false", "", "private-invalid-value"} {
				t.Run(key+"/"+source+"/"+value, func(t *testing.T) {
					home, sources := claudePolicyFixture(t)
					path := filepath.Join(home, ".claude", "remote-settings.json")
					writeClaudePolicyFixture(t, path, unrelatedClaudeRemotePolicy)
					if source == "process" {
						t.Setenv(key, value)
					} else {
						writeClaudeRoutingEnvFixture(t, home, key, value)
					}
					before := snapshotClaudePolicyFiles(t, path)
					policy, err := readClaudeUpdatePolicyWithSources(home, sources)
					wantError := value != "" && value != "0" && value != "false"
					if (err != nil) != wantError || (!wantError && policy != (claudeUpdatePolicy{Channel: "latest"})) {
						t.Fatalf("cached policy with provider flag %q: %+v, %v", value, policy, err)
					}
					if err != nil && strings.Contains(err.Error(), "private-invalid-value") {
						t.Fatalf("provider routing error exposed payload: %v", err)
					}
					assertClaudePolicyFilesUnchanged(t, before)
				})
			}
		}
	}
}

func TestClaudeUpdatePolicyCachedRemoteAcceptsOnlyDefaultAnthropicEndpoint(t *testing.T) {
	for _, source := range []string{"process", "user"} {
		for _, tc := range []struct {
			endpoint string
			allowed  bool
		}{
			{"", true},
			{"https://api.anthropic.com", true},
			{"https://api.anthropic.com/", true},
			{"https://API.ANTHROPIC.COM:443/", true},
			{"https://api.anthropic.com:443", true},
			{"http://api.anthropic.com", false},
			{"https://private-gateway.example", false},
			{"https://api.anthropic.com:8443", false},
			{"https://private-token@api.anthropic.com", false},
			{"https://api.anthropic.com/private-path", false},
			{"https://api.anthropic.com/?token=private-token", false},
			{"https://api.anthropic.com/#private-token", false},
			{"https://[private-invalid-host", false},
		} {
			t.Run(source+"/"+tc.endpoint, func(t *testing.T) {
				home, sources := claudePolicyFixture(t)
				writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), unrelatedClaudeRemotePolicy)
				if source == "process" {
					t.Setenv("ANTHROPIC_BASE_URL", tc.endpoint)
				} else {
					writeClaudeRoutingEnvFixture(t, home, "ANTHROPIC_BASE_URL", tc.endpoint)
				}
				if _, err := readClaudeUpdatePolicyWithSources(home, sources); (err == nil) != tc.allowed || (err != nil && strings.Contains(err.Error(), "private-")) {
					t.Fatalf("cached policy endpoint routing error = %v; allowed %v", err, tc.allowed)
				}
			})
		}
	}
}

func TestClaudeUpdatePolicyRoutingUsesUserOverridesBeforeManagedEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, key, inherited, userValue string
		userOverrides                   bool
		managed                         string
		wantError                       bool
	}{
		{"user-disables-inherited-provider", "CLAUDE_CODE_USE_BEDROCK", "1", "0", true, "", false},
		{"user-clears-inherited-provider", "CLAUDE_CODE_USE_VERTEX", "1", "", true, "", false},
		{"user-restores-default-endpoint", "ANTHROPIC_BASE_URL", "https://private-gateway.example", "https://api.anthropic.com:443/", true, "", false},
		{"user-clears-inherited-endpoint", "ANTHROPIC_BASE_URL", "https://private-gateway.example", "", true, "", false},
		{"managed-env-does-not-cancel-explicit-provider", "CLAUDE_CODE_USE_FOUNDRY", "1", "", false, `{"env":{"CLAUDE_CODE_USE_FOUNDRY":"0"}}`, true},
		{"managed-env-does-not-cancel-explicit-endpoint", "ANTHROPIC_BASE_URL", "https://private-gateway.example", "", false, `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			t.Setenv(tc.key, tc.inherited)
			writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), unrelatedClaudeRemotePolicy)
			if tc.userOverrides {
				writeClaudeRoutingEnvFixture(t, home, tc.key, tc.userValue)
			}
			if tc.managed != "" {
				writeClaudePolicyFixture(t, filepath.Join(sources.ManagedRoot, "managed-settings.json"), tc.managed)
			}
			if _, err := readClaudeUpdatePolicyWithSources(home, sources); (err != nil) != tc.wantError || (err != nil && strings.Contains(err.Error(), "private-")) {
				t.Fatalf("routing precedence error = %v; want error %v", err, tc.wantError)
			}
		})
	}
}

func TestClaudeUpdatePolicyCachedRemoteRejectsMalformedRoutingTypes(t *testing.T) {
	for _, key := range []string{"CLAUDE_CODE_USE_MANTLE", "ANTHROPIC_BASE_URL"} {
		for _, value := range []any{true, 1, nil, []string{"private-routing-payload"}} {
			t.Run(key+"/"+string(mustClaudeRoutingJSON(t, value)), func(t *testing.T) {
				home, sources := claudePolicyFixture(t)
				writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), unrelatedClaudeRemotePolicy)
				writeClaudeRoutingEnvFixture(t, home, key, value)
				if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil || strings.Contains(err.Error(), "private-routing-payload") {
					t.Fatalf("malformed provider routing error = %v", err)
				}
			})
		}
	}
}

func mustClaudeRoutingJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestClaudeUpdatePolicyAlternateProviderWithoutRemoteCacheKeepsLocalBounds(t *testing.T) {
	for _, cache := range []string{"absent", "empty"} {
		t.Run(cache, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
			t.Setenv("ANTHROPIC_BASE_URL", "https://private-gateway.example")
			if cache == "empty" {
				writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), `{}`)
			}
			path := filepath.Join(sources.ManagedRoot, "managed-settings.json")
			writeClaudePolicyFixture(t, path, `{"autoUpdatesChannel":"stable","requiredMinimumVersion":"2.1.312","requiredMaximumVersion":"2.1.314"}`)
			policy, err := readClaudeUpdatePolicyWithSources(home, sources)
			want := claudeUpdatePolicy{Channel: "stable", MinimumVersion: "2.1.312", MaximumVersion: "2.1.314"}
			if err != nil || policy != want {
				t.Fatalf("local policy with no remote cache = %+v, %v; want %+v", policy, err, want)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			writeClaudePolicyFixture(t, path, `{"env":{"DISABLE_UPDATES":"1"}}`)
			if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil {
				t.Fatal("alternate provider bypassed an explicit local update prohibition")
			}
		})
	}
}
