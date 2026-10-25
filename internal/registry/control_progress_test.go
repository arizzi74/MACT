package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func controlProgressResult(t *testing.T, env eventTestEnv, sequence uint64, kind string, result protocol.Result) protocol.Event {
	t.Helper()
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: sequence, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: payload}
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestSteerProgressMovesAfterProcessingAndAcknowledgement(t *testing.T) {
	for _, view := range []string{"queued", "voice_transcript"} {
		t.Run(view, func(t *testing.T) {
			env := answerProgressEnv(t)
			ctx := t.Context()
			in := telegramUpdate(env, 2)
			in.Text = "Also inspect the retry case"
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" || accepted.ProgressReposition {
				t.Fatalf("accept steer: %+v %v", accepted, err)
			}
			ack := claimProgress(t, env.store, 1)[0]
			if view == "voice_transcript" {
				// Voice admission uses this response shape with the same frozen
				// command ID. The processed hook must not assume View=queued.
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET payload=json_set(payload,'$.view','voice_transcript','$.transcript','Also inspect the retry case') WHERE delivery_id=$1`, ack.ID); err != nil {
					t.Fatal(err)
				}
			}
			chunks := []json.RawMessage{json.RawMessage(`{"text":"Acknowledgement one"}`), json.RawMessage(`{"text":"Acknowledgement two"}`)}
			if _, err := env.store.PrepareDeliveryChunks(ctx, ack.ID, chunks); err != nil {
				t.Fatal(err)
			}
			if err := env.store.MarkDeliveryChunkSent(ctx, ack.ID, 0, 110, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := env.store.RetryDelivery(ctx, ack.ID, time.Hour, "Telegram rate limit"); err != nil {
				t.Fatal(err)
			}
			if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: accepted.CommandID, Status: "accepted"}); err != nil {
				t.Fatal(err)
			}
			var retired int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_progress_messages WHERE retire_requested=1`).Scan(&retired); err != nil || retired != 0 {
				t.Fatalf("admission or worker acknowledgement moved progress: %d %v", retired, err)
			}
			result := protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"}
			completed := controlProgressResult(t, env, 5, "command_completed", result)
			var payload []byte
			if err := env.store.pool.QueryRow(ctx, `SELECT payload FROM telegram_deliveries WHERE delivery_id=$1`, ack.ID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var response AcceptResult
			var marked struct {
				Completed bool `json:"progress_command_completed"`
			}
			if json.Unmarshal(payload, &response) != nil || json.Unmarshal(payload, &marked) != nil || !response.ProgressReposition || !marked.Completed || response.View != view {
				t.Fatalf("completion marker broke acknowledgement rendering: %s", payload)
			}
			deleteAnswerProgress(t, env.store, 2)
			claimProgress(t, env.store, 0) // Backed-off acknowledgement still precedes progress.
			reopened, err := Open(ctx, env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.IngestEvent(ctx, env.worker, env.connection, completed); err != nil {
				t.Fatalf("replay after restart: %v", err)
			}
			claimProgress(t, reopened, 0)
			if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=`+sqliteNow+` WHERE delivery_id=$1`, ack.ID); err != nil {
				t.Fatal(err)
			}
			retried := claimProgress(t, reopened, 1)[0]
			if retried.ID != ack.ID {
				t.Fatalf("progress overtook acknowledgement: %+v", retried)
			}
			prepared, err := reopened.DeliveryChunks(ctx, ack.ID)
			if err != nil || len(prepared) != 2 || !prepared[0].Sent || prepared[1].Sent {
				t.Fatalf("prepared acknowledgement chunks changed: %+v %v", prepared, err)
			}
			if err := reopened.MarkDeliveryChunkSent(ctx, ack.ID, 1, 111, "", "", ""); err != nil {
				t.Fatal(err)
			}
			agent := claimProgress(t, reopened, 1)[0]
			if agent.Kind != "agent_progress_message" {
				t.Fatalf("tool overtook agent: %+v", agent)
			}
			checkpointProgress(t, reopened, agent, 200)
			tool := claimProgress(t, reopened, 1)[0]
			if tool.Kind != "tool_progress_message" {
				t.Fatalf("tool missing after agent: %+v", tool)
			}
			checkpointProgress(t, reopened, tool, 201)
			// A second durable event for the same command is distinct from an
			// exact replay and must also preserve the newly posted slots.
			controlProgressResult(t, env, 6, "command_completed", result)
			claimProgress(t, reopened, 0)
			var deliveries, repositioned int
			if err := reopened.pool.QueryRow(ctx, `SELECT count(*),sum(progress_repositioned) FROM telegram_deliveries WHERE kind IN ('agent_progress_message','tool_progress_message')`).Scan(&deliveries, &repositioned); err != nil || deliveries != 4 || repositioned != 2 {
				t.Fatalf("duplicate completion moved cards again: deliveries=%d repositioned=%d %v", deliveries, repositioned, err)
			}
			progressEvent(t, env, 7, "agent_progress_message", "turn-a", "")
			latest := claimProgress(t, reopened, 1)[0]
			assertProgressTarget(t, reopened, latest, 200)
			checkpointProgress(t, reopened, latest, 200)
			progressEvent(t, env, 8, "tool_progress_message", "turn-a", "")
			latest = claimProgress(t, reopened, 1)[0]
			assertProgressTarget(t, reopened, latest, 201)
		})
	}
}

func TestSteerProgressIgnoresUnsuccessfulAndStaleControls(t *testing.T) {
	for _, scenario := range []string{"failed", "unknown", "result error", "missing turn", "different result turn", "different expected turn", "ended turn", "old generation", "other source", "hidden session", "disabled bot", "changed owner", "retired bot", "revoked chat"} {
		t.Run(scenario, func(t *testing.T) {
			env := answerProgressEnv(t)
			ctx := t.Context()
			in := telegramUpdate(env, 2)
			in.Action, in.Text = "steer", "Please check this too"
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" {
				t.Fatalf("accept steer: %+v %v", accepted, err)
			}
			kind := "command_completed"
			result := protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"}
			var query string
			var args []any
			switch scenario {
			case "failed":
				kind, result.Error = "command_failed", &protocol.Error{Code: protocol.StaleTurn}
			case "unknown":
				kind = "command_result_unknown"
			case "result error":
				result.Error = &protocol.Error{Code: protocol.InternalError}
			case "missing turn":
				result.TurnID = ""
			case "different result turn":
				result.TurnID = "other-turn"
			case "different expected turn":
				result.TurnID = "other-turn"
				query, args = `UPDATE sessions SET active_turn_id='other-turn' WHERE session_id=$1`, []any{env.session}
			case "ended turn":
				query, args = `UPDATE sessions SET active_turn_id=NULL,state='idle' WHERE session_id=$1`, []any{env.session}
			case "old generation":
				query, args = `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, []any{env.runtime}
			case "other source":
				// Give the unrelated command the same apparent Telegram
				// destination and acknowledgement so only its source excludes it.
				otherID := uuid.NewString()
				if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
                    (command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,
                     expected_turn_id,payload,status,telegram_bot_id,telegram_user_id,telegram_chat_id,telegram_message_thread_id)
                    SELECT $1,'webui',worker_id,runtime_id,runtime_generation,session_id,operation,
                      expected_turn_id,json_set(payload,'$.command_id',$1),status,telegram_bot_id,
                      telegram_user_id,telegram_chat_id,telegram_message_thread_id
                    FROM commands WHERE command_id=$2`, otherID, accepted.CommandID); err != nil {
					t.Fatal(err)
				}
				if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,message_thread_id,kind,payload)
                    SELECT $1,bot_id,chat_id,message_thread_id,kind,json_set(payload,'$.command_id',$2)
                    FROM telegram_deliveries WHERE kind='ui_response' AND json_extract(payload,'$.command_id')=$3`, uuid.New(), otherID, accepted.CommandID); err != nil {
					t.Fatal(err)
				}
				result.CommandID = otherID
			case "hidden session":
				query = `DELETE FROM telegram_bindings WHERE bot_id='bot' AND chat_id=20`
			case "disabled bot", "changed owner":
				bot := telegramBotRecordForTest("bot", scenario != "disabled bot")
				if scenario == "changed owner" {
					bot.OwnerID = 99
				}
				if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
					t.Fatal(err)
				}
			case "retired bot":
				query = `INSERT INTO telegram_bot_retirements(bot_id) VALUES('bot')`
			case "revoked chat":
				if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{999}); err != nil {
					t.Fatal(err)
				}
			}
			if query != "" {
				if _, err := env.store.pool.Exec(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			controlProgressResult(t, env, 5, kind, result)
			var marked, moved int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE COALESCE(json_extract(payload,'$.progress_command_completed'),0)=1`).Scan(&marked); err != nil || marked != 0 {
				t.Fatalf("ineligible completion marked an acknowledgement: %d %v", marked, err)
			}
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE progress_repositioned=1`).Scan(&moved); err != nil || moved != 0 {
				t.Fatalf("ineligible completion repositioned progress: %d %v", moved, err)
			}
		})
	}
}
