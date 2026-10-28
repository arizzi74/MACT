package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestTelegramProviderIdentityUsesSelectedRuntimeAndAuthenticatedWorker(t *testing.T) {
	for _, tc := range []struct{ name, runtime, worker, want string }{
		{"legacy", "", "", "Codex"},
		{"runtime Claude", `{"agent_kind":"claude"}`, "", "Claude Code"},
		{"worker Claude", "", `{"agent_kind":"claude"}`, "Claude Code"},
		{"runtime wins", `{"agent_kind":"codex"}`, `{"agent_kind":"claude"}`, "Codex"},
		{"malformed runtime fallback", `{`, `{"agent_kind":"claude"}`, "Claude Code"},
		{"unrecognized legacy fallback", `{"agent_kind":"untrusted label"}`, `{"agent_kind":"future"}`, "Codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runtimeProviderLabel(protocol.Runtime{Metadata: json.RawMessage(tc.runtime)}, registry.Worker{HeartbeatMetadata: json.RawMessage(tc.worker)})
			if got != tc.want {
				t.Fatalf("provider=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestClaudeTelegramUserEchoAndHistoryLabels(t *testing.T) {
	for _, source := range []string{"runtime", "worker"} {
		t.Run(source, func(t *testing.T) {
			store := renderFixture()
			if source == "runtime" {
				store.runtimes[0].Metadata = json.RawMessage(`{"agent_kind":"claude"}`)
			} else {
				store.workers[0].HeartbeatMetadata = json.RawMessage(`{"agent_kind":"claude"}`)
			}
			sender := testSender(store, nil)
			text, _, err := sender.render(t.Context(), eventRow(t, "user_message", protocol.Result{Text: "Hello Claude"}, testSessionID.String()))
			if err != nil || text != "👤 You · Claude Code\n\nHello Claude" {
				t.Fatalf("user echo=%q err=%v", text, err)
			}
			page := &protocol.HistoryPage{Limit: 1, Prompts: []protocol.HistoryPrompt{{TurnID: "turn", ItemID: "prompt", Text: "Saved prompt"}}}
			row := eventRow(t, "command_completed", protocol.Result{History: page}, testSessionID.String())
			parts, _, err := sender.renderDeliveryParts(t.Context(), row)
			if err != nil || len(parts) != 2 || !strings.Contains(parts[1], "👤 You · Claude Code") || strings.Contains(strings.Join(parts, ""), "Codex") {
				t.Fatalf("prompt history=%q err=%v", parts, err)
			}
			for _, newestFirst := range []bool{false, true} {
				page = &protocol.HistoryPage{Limit: 1, Conversation: true, NewestFirst: newestFirst, Messages: []protocol.HistoryMessage{{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "Saved answer"}}}
				row = eventRow(t, "command_completed", protocol.Result{History: page}, testSessionID.String())
				parts, _, err = sender.renderDeliveryParts(t.Context(), row)
				if err != nil || len(parts) != 1 || !strings.Contains(parts[0], "🤖 Claude Code") || strings.Contains(parts[0], "Codex") {
					t.Fatalf("conversation history newestFirst=%v parts=%q err=%v", newestFirst, parts, err)
				}
			}
		})
	}
}
