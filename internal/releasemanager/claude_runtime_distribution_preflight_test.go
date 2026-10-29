package releasemanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudePreflightEnvironmentExcludesProductionCredentials(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "ANTHROPIC_BASE_URL", "HTTPS_PROXY", "NODE_OPTIONS", "LD_PRELOAD", "OTEL_EXPORTER_OTLP_HEADERS"} {
		t.Setenv(key, "secret-production-value")
	}
	environment := strings.Join(claudePreflightEnvironment("/isolated"), "\n")
	if strings.Contains(environment, "secret-production-value") {
		t.Fatal("preflight inherited production credentials or executable overrides")
	}
	for _, required := range []string{"HOME=/isolated", "CLAUDE_CONFIG_DIR=/isolated/.claude", "ANTHROPIC_BASE_URL=http://127.0.0.1:1", "DISABLE_UPDATES=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"} {
		if !strings.Contains(environment, required+"\n") {
			t.Errorf("missing isolated setting %q", required)
		}
	}
}

func TestClaudePreflightOnlyInitializesNativeControlProtocol(t *testing.T) {
	for _, kind := range []string{"success", "notification", "wrong-request", "wrong-type", "rejected", "malformed", "empty", "response-null", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "capture")
			// The fixture records arguments and its isolated environment, then
			// accepts precisely one initialize message. There is no user query.
			script := `#!/bin/sh
printf '%s\n' "$@" > '` + capture + `.args'
printf '%s\n' "$HOME" "$CLAUDE_CONFIG_DIR" "$ANTHROPIC_API_KEY" "$ANTHROPIC_BASE_URL" "${ANTHROPIC_AUTH_TOKEN-unset}" "${CLAUDE_CODE_OAUTH_TOKEN-unset}" > '` + capture + `.env'
IFS= read -r message
printf '%s\n' "$message" > '` + capture + `.request'
`
			switch kind {
			case "success", "notification":
				if kind == "notification" {
					script += `printf '%s\n' '{"type":"system","subtype":"ui_invalidate","event":"ui.render"}'` + "\n"
				}
				script += `printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"mact-compatibility-check","response":{"commands":[],"models":[]}}}'` + "\n"
			case "wrong-request":
				script += `printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"wrong","response":{}}}'` + "\n"
			case "wrong-type":
				script += `printf '%s\n' '{"type":"result","response":{"subtype":"success","request_id":"mact-compatibility-check","response":{}}}'` + "\n"
			case "rejected":
				script += `printf '%s\n' '{"type":"control_response","response":{"subtype":"error","request_id":"mact-compatibility-check","response":{"error":"secret-production-value"}}}'` + "\n"
			case "malformed":
				script += "printf '%s\\n' 'secret-production-value'\n"
			case "response-null":
				script += `printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"mact-compatibility-check","response":null}}'` + "\n"
			case "oversize":
				script += "head -c 2097153 /dev/zero | tr '\\000' x\n"
			}
			binary := filepath.Join(dir, "claude-fixture")
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "secret-production-value")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "secret-production-value")
			m := New(nil)
			err := m.preflightClaudeRuntime(t.Context(), &claudeDistribution{Binary: binary})
			if (err == nil) != (kind == "success" || kind == "notification") || (err != nil && strings.Contains(err.Error(), "secret-production-value")) {
				t.Fatalf("preflight=%v", err)
			}
			request, err := os.ReadFile(capture + ".request")
			if err != nil || strings.Contains(string(request), `"type":"user"`) || strings.Count(string(request), "\n") != 1 || !strings.Contains(string(request), `"subtype":"initialize"`) {
				t.Fatalf("preflight request=%q, %v", request, err)
			}
			environment, err := os.ReadFile(capture + ".env")
			if err != nil || strings.Contains(string(environment), "secret-production-value") || !strings.Contains(string(environment), "http://127.0.0.1:1") {
				t.Fatalf("preflight environment=%q, %v", environment, err)
			}
			lines := strings.Split(strings.TrimSpace(string(environment)), "\n")
			if len(lines) == 0 {
				t.Fatal("missing private compatibility home")
			}
			if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
				t.Fatalf("compatibility home was not cleaned: %v", err)
			}
			arguments, err := os.ReadFile(capture + ".args")
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range []string{"--setting-sources=", "--strict-mcp-config", "--mcp-config={\"mcpServers\":{}}", "--no-session-persistence", "--permission-prompt-tool=stdio"} {
				if !strings.Contains(string(arguments), flag+"\n") {
					t.Errorf("missing compatibility isolation flag %q", flag)
				}
			}
		})
	}
}
