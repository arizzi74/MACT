package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestToolApprovalCleanupHandlesLateCheckpointsAndPreservesQuestions(t *testing.T) {
	for _, kind := range []string{"command_execution", "file_change", "permissions", "user_input"} {
		t.Run(kind, func(t *testing.T) {
			env := progressEnv(t)
			progressEvent(t, env, 2, "turn_started", "turn", "")
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "request", ThreadID: "thread-1", TurnID: "turn", Type: kind, State: "pending"}
			asyncQuestionEvent(t, env, 3, 1, "approval_requested", approval)
			var delivery string
			if err := env.store.pool.QueryRow(t.Context(), `SELECT delivery_id FROM telegram_deliveries WHERE kind='approval_requested'`).Scan(&delivery); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET status='sending',next_attempt_at=$2 WHERE delivery_id=$1`, delivery, time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.PrepareDeliveryChunks(t.Context(), delivery, []json.RawMessage{json.RawMessage(`{"text":"approval"}`)}); err != nil {
				t.Fatal(err)
			}
			// The Web UI resolves the native request while sendMessage is in
			// flight, before its eventual message ID can be checkpointed.
			approval.State = "approved"
			asyncQuestionEvent(t, env, 4, 1, "approval_resolved", approval)
			if err := env.store.MarkDeliveryChunkSent(t.Context(), delivery, 0, 901, env.session.String(), "turn", approval.ID); err != nil {
				t.Fatal(err)
			}
			cleanups := inputReplyCleanupMessages(t, env.store)
			if kind == "user_input" {
				if len(cleanups) != 0 {
					t.Fatalf("question history deleted: %+v", cleanups)
				}
			} else if len(cleanups) != 1 || cleanups[901] == "" {
				t.Fatalf("late approval message was not queued for deletion: %+v", cleanups)
			}
		})
	}
}

func TestToolApprovalCleanupKeepsPendingAndOldMessages(t *testing.T) {
	env := progressEnv(t)
	progressEvent(t, env, 2, "turn_started", "turn", "")
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "tool", ThreadID: "thread-1", TurnID: "turn", Type: "command_execution", State: "pending"}
	asyncQuestionEvent(t, env, 3, 1, "approval_requested", approval)
	id := uuid.MustParse(approval.ID)
	for _, message := range []int64{910, 911} {
		if err := env.store.RecordBotMessageRoute(t.Context(), "bot", 20, message, env.session, "turn", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE bot_message_routes SET created_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-49 hours') || '000000Z') WHERE message_id=911`); err != nil {
		t.Fatal(err)
	}
	if got := inputReplyCleanupMessages(t, env.store); len(got) != 0 {
		t.Fatalf("pending approval deleted: %+v", got)
	}
	approval.State = "approved"
	asyncQuestionEvent(t, env, 4, 1, "approval_resolved", approval)
	if got := inputReplyCleanupMessages(t, env.store); len(got) != 1 || got[910] == "" {
		t.Fatalf("cleanup did not respect Telegram deletion window: %+v", got)
	}
	// A legacy sender can record its route after resolution as well.
	if err := env.store.RecordBotMessageRoute(t.Context(), "bot", 20, 912, env.session, "turn", id); err != nil {
		t.Fatal(err)
	}
	if got := inputReplyCleanupMessages(t, env.store); len(got) != 2 || got[912] == "" {
		t.Fatalf("late route was not retired: %+v", got)
	}
}
