package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func controlAnswerResult(t *testing.T, env eventTestEnv, kind string, result protocol.Result) protocol.Event {
	t.Helper()
	sequence, err := env.store.EventWatermark(t.Context(), env.worker)
	if err != nil {
		t.Fatal(err)
	}
	event := historyTestEvent(t, env, kind, result)
	event.Seq = uint64(sequence + 1)
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func controlAnswerAccept(t *testing.T, env eventTestEnv, mode string, async bool) AcceptResult {
	t.Helper()
	approval := insertPendingQuestion(t, env, env.session, "turn-a", async,
		protocol.Question{ID: "first", Prompt: "Which computer?", Options: []string{"Mac", "Linux"}})
	if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, 900, env.session, "turn-a", approval, "first"); err != nil {
		t.Fatal(err)
	}
	answer := telegramUpdate(env, 2)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Mac"
	if mode == "button" {
		answer.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "input", "first", "Mac")
	}
	var accepted AcceptResult
	var err error
	if mode == "voice" {
		queueVoiceTest(t, env, answer)
		job := claimVoiceTest(t, env.store)
		accepted, err = env.store.CompleteTelegramVoice(t.Context(), job.ID, job.LeaseID, "Mac")
	} else {
		accepted, err = env.store.AcceptTelegram(t.Context(), answer)
	}
	if err != nil || accepted.CommandID == "" || !accepted.ProgressReposition {
		t.Fatalf("accept complete %s answer: %+v %v", mode, accepted, err)
	}
	return accepted
}

func controlAnswerSendResponses(t *testing.T, store *Store) {
	t.Helper()
	rows, err := store.ClaimDeliveries(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		message := int64(910 + i)
		switch row.Kind {
		case "question_answered":
			var edit QuestionAnswerEdit
			if err := json.Unmarshal(row.Payload, &edit); err != nil {
				t.Fatal(err)
			}
			message = edit.MessageID
		case "ui_response":
		default:
			t.Fatalf("progress bypassed answer response: %+v", row)
		}
		if err := store.MarkDeliverySent(t.Context(), row.ID, message, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func controlAnswerCheckpointPair(t *testing.T, store *Store, firstMessage int64) []Delivery {
	t.Helper()
	var pair []Delivery
	for index, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		rows, err := store.ClaimDeliveries(t.Context(), 100)
		if err != nil || len(rows) != 1 || rows[0].Kind != kind {
			t.Fatalf("restored %s must have its own ordered send: %+v %v", kind, rows, err)
		}
		row := rows[0]
		assertProgressTarget(t, store, row, 0)
		checkpointProgress(t, store, row, firstMessage+int64(index))
		pair = append(pair, row)
	}
	return pair
}

func controlAnswerSettleAdmission(t *testing.T, store *Store) []Delivery {
	t.Helper()
	controlAnswerSendResponses(t, store)
	deleteAnswerProgress(t, store, 2)
	return controlAnswerCheckpointPair(t, store, 200)
}

func controlAnswerAssertNotRepositioned(t *testing.T, store *Store, command string, pair []Delivery) {
	t.Helper()
	for _, row := range pair {
		var retired bool
		if err := store.pool.QueryRow(t.Context(), `SELECT progress_repositioned FROM telegram_deliveries WHERE delivery_id=$1`, row.ID).Scan(&retired); err != nil || retired {
			t.Fatalf("nonqualifying answer result retired progress %s: %v %v", row.ID, retired, err)
		}
	}
	var marked int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_deliveries WHERE kind='ui_response'
        AND json_extract(payload,'$.command_id')=$1 AND json_extract(payload,'$.progress_command_completed')=1`, command).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("nonqualifying answer result was marked complete: %d %v", marked, err)
	}
}

func TestControlAnswerProgressMovesAgainAfterConfirmedCompletion(t *testing.T) {
	for _, mode := range []string{"typed", "button", "voice", "async_active"} {
		t.Run(mode, func(t *testing.T) {
			env := answerProgressEnv(t)
			accepted := controlAnswerAccept(t, env, mode, mode == "async_active")
			controlAnswerSettleAdmission(t, env.store)
			confirmed := controlAnswerResult(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"})
			claimProgress(t, env.store, 0)
			deletions, err := env.store.ClaimTelegramDeletions(t.Context(), 100)
			if err != nil || len(deletions) != 2 {
				t.Fatalf("confirmed answer did not retire both admitted progress cards: %+v %v", deletions, err)
			}
			for i, deletion := range deletions {
				if deletion.MessageID != 200 && deletion.MessageID != 201 {
					t.Fatalf("confirmed answer deleted a question or acknowledgement: %+v", deletion)
				}
				if err := env.store.MarkTelegramDeletionDone(t.Context(), deletion.ID); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					claimProgress(t, env.store, 0)
				}
			}
			controlAnswerCheckpointPair(t, env.store, 300)
			if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, confirmed); err != nil {
				t.Fatal(err)
			}
			deleteAnswerProgress(t, env.store, 0)
			claimProgress(t, env.store, 0)
			// A different completion event after reopening must retain the same
			// once-per-command decision as an exact durable event replay.
			reopened, err := Open(t.Context(), env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			env.store = reopened
			controlAnswerResult(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"})
			deleteAnswerProgress(t, reopened, 0)
			claimProgress(t, reopened, 0)
		})
	}
}

func TestControlAnswerProgressWaitsForPendingAcknowledgement(t *testing.T) {
	env := answerProgressEnv(t)
	accepted := controlAnswerAccept(t, env, "typed", false)
	ack := answerProgressUI(t, env.store)
	deleteAnswerProgress(t, env.store, 2)
	controlAnswerResult(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"})
	claimProgress(t, env.store, 0)
	if err := env.store.RetryDelivery(t.Context(), ack.ID, time.Hour, "Telegram retry"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimProgress(t, reopened, 0)
	if _, err := reopened.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET next_attempt_at=$2 WHERE delivery_id=$1`, ack.ID, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	ack = answerProgressUI(t, reopened)
	if _, err := reopened.PrepareDeliveryChunks(t.Context(), ack.ID, []json.RawMessage{json.RawMessage(`{"text":"Answer queued"}`), json.RawMessage(`{"text":"Acknowledged"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkDeliveryChunkSent(t.Context(), ack.ID, 0, 910, "", "", ""); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, reopened, 0)
	if err := reopened.MarkDeliveryChunkSent(t.Context(), ack.ID, 1, 911, "", "", ""); err != nil {
		t.Fatal(err)
	}
	controlAnswerCheckpointPair(t, reopened, 300)
}

func TestControlAnswerProgressWaitsForVoiceTranscript(t *testing.T) {
	env := answerProgressEnv(t)
	accepted := controlAnswerAccept(t, env, "voice", false)
	rows, err := env.store.ClaimDeliveries(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var transcript Delivery
	for i, row := range rows {
		var result AcceptResult
		if row.Kind == "ui_response" && json.Unmarshal(row.Payload, &result) == nil && result.View == "voice_transcript" {
			transcript = row
			continue
		}
		if err := env.store.MarkDeliverySent(t.Context(), row.ID, 920+int64(i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if transcript.ID == "" {
		t.Fatal("no pending voice transcript")
	}
	deleteAnswerProgress(t, env.store, 2)
	controlAnswerResult(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"})
	claimProgress(t, env.store, 0)
	if err := env.store.MarkDeliverySent(t.Context(), transcript.ID, 930, "", "", ""); err != nil {
		t.Fatal(err)
	}
	controlAnswerCheckpointPair(t, env.store, 300)
}

func TestControlAnswerProgressMovesVisibleCardsWhenAsyncAnswerStartsTurn(t *testing.T) {
	env := answerProgressEnv(t)
	answered := env
	answered.session = uuid.New()
	insertRouteSession(t, env, answered.session, "idle-question-thread", "")
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_chat_modes SET multi_session=1 WHERE bot_id='bot' AND chat_id=20 AND message_thread_id=0`); err != nil {
		t.Fatal(err)
	}
	accepted := controlAnswerAccept(t, answered, "typed", true)
	controlAnswerSettleAdmission(t, env.store)
	controlAnswerResult(t, answered, "turn_started", protocol.Result{CommandID: accepted.CommandID, TurnID: "new-turn", State: "running"})
	deleteAnswerProgress(t, env.store, 2)
	controlAnswerCheckpointPair(t, env.store, 300)
	var active, selected string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT active_turn_id FROM sessions WHERE session_id=$1`, answered.session).Scan(&active); err != nil || active != "new-turn" {
		t.Fatalf("async answer did not preserve started turn: %s %v", active, err)
	}
	if err := env.store.pool.QueryRow(t.Context(), `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND chat_id=20 AND message_thread_id=0`).Scan(&selected); err != nil || selected != env.session.String() {
		t.Fatalf("async answer changed selection: %s %v", selected, err)
	}
}

func TestControlAnswerProgressIgnoresUnconfirmedAndStaleResults(t *testing.T) {
	for _, scenario := range []string{"failed", "unknown", "missing_turn", "different_turn", "old_generation", "finished_turn", "selection_changed"} {
		t.Run(scenario, func(t *testing.T) {
			env := answerProgressEnv(t)
			accepted := controlAnswerAccept(t, env, "typed", false)
			pair := controlAnswerSettleAdmission(t, env.store)
			kind := "command_completed"
			result := protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"}
			switch scenario {
			case "failed":
				kind = "command_failed"
				result.Error = &protocol.Error{Code: "invalid_argument", Message: "Answer rejected"}
			case "unknown":
				kind = "command_result_unknown"
			case "missing_turn":
				result.TurnID = ""
			case "different_turn":
				result.TurnID = "other-turn"
			case "old_generation":
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime); err != nil {
					t.Fatal(err)
				}
			case "finished_turn":
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET state='idle',active_turn_id=NULL WHERE session_id=$1`, env.session); err != nil {
					t.Fatal(err)
				}
			case "selection_changed":
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "")
				selection := telegramUpdate(env, 3)
				selection.Action, selection.Target = "select", other.String()
				if selected, err := env.store.AcceptTelegram(t.Context(), selection); err != nil || selected.ErrorCode != "" {
					t.Fatalf("switch selection: %+v %v", selected, err)
				}
			}
			controlAnswerResult(t, env, kind, result)
			controlAnswerAssertNotRepositioned(t, env.store, accepted.CommandID, pair)
		})
	}
}

func TestControlAnswerProgressIgnoresDismissedQuestion(t *testing.T) {
	env := answerProgressEnv(t)
	approval := insertPendingQuestion(t, env, env.session, "turn-a", true, protocol.Question{ID: "first", Prompt: "Continue?"})
	answer := telegramUpdate(env, 2)
	answer.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "dismiss_input", "first", "")
	accepted, err := env.store.AcceptTelegram(t.Context(), answer)
	if err != nil || accepted.CommandID == "" || accepted.ProgressReposition {
		t.Fatalf("dismiss question: %+v %v", accepted, err)
	}
	controlAnswerSendResponses(t, env.store)
	controlAnswerResult(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"})
	controlAnswerAssertNotRepositioned(t, env.store, accepted.CommandID, nil)
	deleteAnswerProgress(t, env.store, 0)
	claimProgress(t, env.store, 0)
}

func TestControlAnswerProgressKeepsOtherDestinations(t *testing.T) {
	env := answerProgressEnv(t)
	if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO telegram_bindings(bot_id,user_id,chat_id,message_thread_id,session_id)
        VALUES('bot',10,20,4,$1),('other-bot',10,20,0,$1),('bot',10,21,0,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		progressEvent(t, env, uint64(5+i), kind, "turn-a", "")
		for j, row := range claimProgress(t, env.store, 4) {
			message := int64(400 + i*10 + j)
			if row.BotID == "bot" && row.ChatID == 20 && row.TopicID == 0 {
				message = int64(100 + i)
			}
			checkpointProgress(t, env.store, row, message)
		}
	}
	accepted := controlAnswerAccept(t, env, "typed", false)
	controlAnswerSettleAdmission(t, env.store)
	controlAnswerResult(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-a", State: "completed"})
	for _, deletion := range deleteAnswerProgress(t, env.store, 2) {
		if deletion.BotID != "bot" || deletion.ChatID != 20 || deletion.TopicID != 0 || (deletion.MessageID != 200 && deletion.MessageID != 201) {
			t.Fatalf("confirmed answer retired another destination: %+v", deletion)
		}
	}
	controlAnswerCheckpointPair(t, env.store, 300)
	var untouched int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_progress_messages
        WHERE (bot_id<>'bot' OR chat_id<>20 OR message_thread_id<>0) AND status='pending' AND retire_requested=0`).Scan(&untouched); err != nil || untouched != 6 {
		t.Fatalf("confirmed answer changed another bot/chat/topic: %d %v", untouched, err)
	}
}
