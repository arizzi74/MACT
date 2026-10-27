package gateway

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Use actual question callbacks, the manual text-reply helper, registry events,
// Telegram rendering, HTTP bot resolution and delivery checkpoints. The native
// echo deliberately arrives after answer admission and command completion.
func TestAnsweredQuestionDelayedNativeEchoKeepsProgressAtBottom(t *testing.T) {
	for _, async := range []bool{true, false} {
		for _, shared := range []bool{true, false} {
			t.Run(fmt.Sprintf("async=%t/shared-session=%t", async, shared), func(t *testing.T) {
				env := newCrossBotQuestions(t)
				ctx := t.Context()
				if shared {
					selected, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "second", UpdateID: 2, UserID: 10, ChatID: 20, Action: "select", Target: env.first.String()})
					if err != nil || selected.ErrorCode != "" {
						t.Fatalf("select shared session: %+v %v", selected, err)
					}
					env.flush()
				}
				env.emit(env.first, "turn_started", protocol.Result{TurnID: "current-turn"})
				env.emit(env.first, "agent_progress_message", protocol.Result{TurnID: "current-turn", Text: "Waiting for the tunnel"})
				env.emit(env.first, "tool_progress_message", protocol.Result{TurnID: "current-turn", Text: "ssh tunnel-status"})
				env.flush()
				if !shared {
					env.emit(env.second, "turn_started", protocol.Result{TurnID: "other-turn"})
					env.emit(env.second, "agent_progress_message", protocol.Result{TurnID: "other-turn", Text: "Unrelated session commentary"})
					env.emit(env.second, "tool_progress_message", protocol.Result{TurnID: "other-turn", Text: "unrelated-tool"})
					env.flush()
				}
				requestID := "blocking-question"
				if async {
					requestID = "async:tunnel-question"
				}
				approval := protocol.Approval{ID: uuid.NewString(), RequestID: requestID, ThreadID: env.first.String(), TurnID: "current-turn", Type: "user_input", Async: async,
					Questions: []protocol.Question{{ID: "tunnel", Prompt: "Has the tunnel reopened?"}}}
				env.emit(env.first, "user_input_requested", approval)
				env.flush()
				copies := env.questionCopies("Has the tunnel reopened?")
				if len(copies) != 2 {
					t.Fatalf("question was not broadcast to both bots: %+v", copies)
				}
				var answer deliveredQuestionCopy
				for _, copy := range copies {
					if copy.bot == "second" {
						answer = copy
					}
				}
				opened, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "second", UpdateID: 3, UserID: 10, ChatID: 20,
					CallbackToken: questionButton(t, answer.message, "Reply with text"), CallbackMessageID: answer.id})
				if err != nil || !opened.TextReply || opened.SessionID != env.first.String() {
					t.Fatalf("open broadcast question text reply: %+v %v", opened, err)
				}
				env.flush()
				helperID := int64(inputProgressMessageCount(env, "second"))
				if !strings.Contains(env.message("second", helperID).Text, "choose Reply") {
					t.Fatal("text-reply helper was not sent")
				}
				accepted, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "second", UpdateID: 4, UserID: 10, ChatID: 20,
					ReplyToMessageID: helperID, Text: "Tunnel reopened"})
				if err != nil || accepted.CommandID == "" || accepted.SessionID != env.first.String() {
					t.Fatalf("answer broadcast question: %+v %v", accepted, err)
				}
				settleInputProgress(t, env)
				env.emit(env.first, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "current-turn", State: "completed"})
				settleInputProgress(t, env)
				for _, copy := range copies {
					env.assertAnswer(copy.bot, copy.id, "Has the tunnel reopened?", "Tunnel reopened")
				}
				before := map[string]int{"first": inputProgressMessageCount(env, "first"), "second": inputProgressMessageCount(env, "second")}
				echo := env.emit(env.first, "user_message", protocol.Result{TurnID: "current-turn", Text: "> Has the tunnel reopened?\n\nTunnel reopened"})
				if rows, err := env.store.ClaimTelegramDeletions(ctx, 10); err != nil || len(rows) != 0 {
					t.Fatalf("queued echo moved cards before its visible delivery: %+v %v", rows, err)
				}
				env.flush()
				visibleBots := []string{"first"}
				if shared {
					visibleBots = append(visibleBots, "second")
				}
				for _, bot := range visibleBots {
					if inputProgressMessageCount(env, bot) != before[bot]+1 || !strings.Contains(env.message(bot, int64(before[bot]+1)).Text, "👤 You · Codex") {
						t.Fatalf("%s did not send the echo before its replacement cards", bot)
					}
				}
				settleInputProgress(t, env)
				for _, bot := range visibleBots {
					if inputProgressMessageCount(env, bot) != before[bot]+3 {
						t.Fatalf("%s did not repost exactly two cards below the echo", bot)
					}
					commentary := env.message(bot, int64(before[bot]+2))
					tool := env.message(bot, int64(before[bot]+3))
					if !strings.HasPrefix(commentary.Text, "⏳ ") || !strings.Contains(commentary.Text, "Waiting for the tunnel") || !strings.Contains(tool.Text, "ssh tunnel-status") || len(tool.Entities) != 1 || tool.Entities[0].Type != "pre" {
						t.Fatalf("%s restored cards in the wrong order or lost formatting: %+v / %+v", bot, commentary, tool)
					}
				}
				if !shared && inputProgressMessageCount(env, "second") != before["second"] {
					t.Fatal("answer echo posted into or moved cards from another selected session")
				}
				if err := env.store.IngestEvent(ctx, env.worker, env.connection, echo); err != nil {
					t.Fatal(err)
				}
				env.emit(env.first, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "current-turn", State: "completed"})
				settleInputProgress(t, env)
				for _, bot := range visibleBots {
					if inputProgressMessageCount(env, bot) != before[bot]+3 {
						t.Fatal("replayed echo/completion duplicated replacement cards")
					}
				}
				env.emit(env.first, "agent_progress_message", protocol.Result{TurnID: "current-turn", Text: "Tunnel checks resumed"})
				env.emit(env.first, "tool_progress_message", protocol.Result{TurnID: "current-turn", Text: "ssh verify"})
				env.flush()
				for _, bot := range visibleBots {
					if inputProgressMessageCount(env, bot) != before[bot]+3 || !strings.Contains(env.message(bot, int64(before[bot]+2)).Text, "Tunnel checks resumed") || !strings.Contains(env.message(bot, int64(before[bot]+3)).Text, "ssh verify") {
						t.Fatalf("%s did not edit the replacement card IDs", bot)
					}
				}
			})
		}
	}
}

func settleInputProgress(t *testing.T, env *crossBotQuestions) {
	t.Helper()
	env.flush()
	for range 8 {
		if err := env.sender.flushDeletions(t.Context(), env.store, nil); err != nil {
			t.Fatal(err)
		}
	}
	env.flush()
}

func inputProgressMessageCount(env *crossBotQuestions, bot string) int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return len(env.apis[bot].messages)
}
