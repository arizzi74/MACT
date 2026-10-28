package codexadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This opt-in check uses an offline provider and an isolated Codex home. It
// creates empty threads only; no model turn or authenticated API call runs.
func TestNativeNewThreadDefaultsAreAtomic(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_DEFAULTS_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_DEFAULTS_TEST=1 to verify native thread defaults")
	}
	codexHome, project, outside := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	configuration := fmt.Sprintf(`model_provider = "isolated"
model = "native-default-model"
model_reasoning_effort = "low"
default_permissions = ":read-only"
approval_policy = "never"
check_for_update_on_startup = false
[sandbox_workspace_write]
network_access = true
writable_roots = [%q]
[model_providers.isolated]
name = "Offline defaults test"
base_url = "http://127.0.0.1:1"
wire_api = "responses"
requires_openai_auth = false
[projects.%q]
trust_level = "trusted"
`, outside, project)
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{WorkingDirectory: project, ClientInfo: ClientInfo{Name: "telegramgw-native-defaults-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, tc := range []struct{ profile, policy, sandbox string }{
		{":workspace", "on-request", "workspaceWrite"},
		{":read-only", "on-request", "readOnly"},
		{":danger-full-access", "never", "dangerFullAccess"},
	} {
		options := ThreadOptions{
			CWD: project, HistoryMode: "legacy", Model: "gpt-5.4", ReasoningEffort: "high",
			PermissionProfile: tc.profile, ApprovalPolicy: tc.policy, ApprovalsReviewer: "user",
		}
		params, err := options.startFields()
		if err != nil {
			t.Fatal(err)
		}
		type defaultsReply struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Model             string `json:"model"`
			ReasoningEffort   string `json:"reasoningEffort"`
			ApprovalPolicy    string `json:"approvalPolicy"`
			ApprovalsReviewer string `json:"approvalsReviewer"`
			Profile           struct {
				ID string `json:"id"`
			} `json:"activePermissionProfile"`
			Sandbox struct {
				Type          string   `json:"type"`
				NetworkAccess bool     `json:"networkAccess"`
				WritableRoots []string `json:"writableRoots"`
			} `json:"sandbox"`
		}
		var reply defaultsReply
		if err := client.request(ctx, "thread/start", params, &reply, false); err != nil {
			t.Fatalf("start %s: %v", tc.profile, err)
		}
		if reply.Thread.ID == "" || reply.Model != "gpt-5.4" || reply.ReasoningEffort != "high" || reply.ApprovalPolicy != tc.policy || reply.ApprovalsReviewer != "user" || reply.Profile.ID != tc.profile || reply.Sandbox.Type != tc.sandbox {
			t.Fatalf("native defaults changed or inherited global config: %#v", reply)
		}
		if tc.profile == ":workspace" && (reply.Sandbox.NetworkAccess || len(reply.Sandbox.WritableRoots) != 0) {
			t.Fatalf("workspace preset inherited global sandbox extras: %#v", reply.Sandbox)
		}
		// Worker-created sessions are named immediately. Naming materializes
		// the empty rollout, so its defaults can be reopened before any prompt.
		if err := client.RenameThread(ctx, reply.Thread.ID, "Saved worker defaults"); err != nil {
			t.Fatalf("name %s: %v", tc.profile, err)
		}
		var resumed defaultsReply
		if err := client.request(ctx, "thread/resume", map[string]any{"threadId": reply.Thread.ID, "excludeTurns": true}, &resumed, false); err != nil {
			t.Fatalf("resume named %s: %v", tc.profile, err)
		}
		if resumed.Thread.ID != reply.Thread.ID || resumed.Model != reply.Model || resumed.ReasoningEffort != reply.ReasoningEffort || resumed.ApprovalPolicy != reply.ApprovalPolicy || resumed.ApprovalsReviewer != reply.ApprovalsReviewer || resumed.Profile.ID != reply.Profile.ID || resumed.Sandbox.Type != reply.Sandbox.Type {
			t.Fatalf("named thread did not preserve defaults across resume: %#v", resumed)
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != configuration {
		t.Fatalf("thread defaults changed native config: %s %v", after, err)
	}
}
