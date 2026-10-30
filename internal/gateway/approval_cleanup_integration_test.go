package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Both native adapters publish the same authoritative resolution when a Web UI
// answer is accepted. Exercise the real outbox and bot HTTP routing, including
// deliberately colliding Telegram message IDs in the two bots.
func TestWebUIResolvedToolApprovalDeletesTelegramCopiesForBothAgents(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			env := newCrossBotQuestions(t)
			metadata, _ := json.Marshal(map[string]string{"agent_kind": agent})
			if err := env.store.RecordHeartbeat(t.Context(), registry.Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Metadata: metadata,
				Runtimes: []registry.Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 1, State: "running", Metadata: metadata}}}); err != nil {
				t.Fatal(err)
			}
			env.emit(env.first, "turn_started", protocol.Result{TurnID: "approval-turn"})
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "native-tool", ThreadID: env.first.String(), TurnID: "approval-turn",
				Type: "command_execution", State: "pending", Summary: "Run the approved command", Decisions: []string{"accept", "decline", "cancel"}}
			env.emit(env.first, "approval_requested", approval)
			env.flush()
			copies := env.questionCopies(approval.Summary)
			if len(copies) != 2 {
				t.Fatalf("approval copies=%+v", copies)
			}
			question := protocol.Approval{ID: uuid.NewString(), RequestID: "unrelated-question", ThreadID: env.first.String(), Type: "user_input", Async: true,
				Questions: []protocol.Question{{ID: "keep", Prompt: "Keep this unanswered question?"}}}
			env.emit(env.first, "user_input_requested", question)
			env.flush()
			questions := env.questionCopies("Keep this unanswered question?")
			if len(questions) != 2 {
				t.Fatalf("question copies=%+v", questions)
			}
			// Resolution comes from the native Web UI path, without a Telegram
			// callback. A restarted sender must still remove every original card.
			approval.State = "cleared"
			env.emit(env.first, "approval_resolved", approval)
			env.sender = NewSender(env.store, nil, nil, env.sender.options)
			env.flush()
			for _, copy := range copies {
				if _, exists := env.apis[copy.bot].live[copy.id]; exists {
					t.Fatalf("resolved approval survived in %s: %d", copy.bot, copy.id)
				}
				deleted := 0
				for _, identity := range env.apis[copy.bot].deleted {
					if identity == [2]int64{copy.message.ChatID, copy.id} {
						deleted++
					}
				}
				if deleted != 1 {
					t.Fatalf("approval deletion count=%d in %s", deleted, copy.bot)
				}
			}
			for _, copy := range questions {
				if !strings.Contains(env.apis[copy.bot].live[copy.id].Text, "Keep this unanswered question?") {
					t.Fatal("tool approval cleanup removed an unrelated question")
				}
			}
			env.emit(env.first, "approval_resolved", approval)
			env.flush()
			for _, api := range env.apis {
				if len(api.deleted) != 1 {
					t.Fatalf("resolution replay repeated deletion: %+v", api.deleted)
				}
			}
			env.assertSelections()
		})
	}
}
