package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIQuestionAnswerRecoveryAndHistory(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	id := insertPendingQuestion(t, env, env.session, "turn", false,
		protocol.Question{ID: "second-sort", Prompt: "First question?"}, protocol.Question{ID: "first-sort", Prompt: "Private question?", Secret: true})
	raw := []byte{}
	if err := env.store.pool.QueryRow(ctx, `SELECT request_payload FROM approvals WHERE approval_id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var answer protocol.Approval
	_ = json.Unmarshal(raw, &answer)
	answer.ItemID = "native-question"
	answer.Answers = map[string][]string{"second-sort": {"First answer"}, "first-sort": {"private answer"}}
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET codex_item_id=$2,state='cleared',resolved_at=`+sqliteNow+` WHERE approval_id=$1`, id, answer.ItemID); err != nil {
		t.Fatal(err)
	}
	for i, q := range answer.Questions {
		if err := env.store.RecordBotInputRoute(ctx, "bot", 20, int64(501+i), env.session, "turn", id, q.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []string{"session", "thread", "turn", "item", "request", "question", "missing"} {
		bad := answer
		session := env.session
		switch mutation {
		case "session":
			session = uuid.New()
		case "thread":
			bad.ThreadID = "other"
		case "turn":
			bad.TurnID = "other"
		case "item":
			bad.ItemID = "other"
		case "request":
			bad.RequestID = "other"
		case "question":
			bad.Answers = map[string][]string{"unknown": {"bad"}, "first-sort": {"private answer"}}
		case "missing":
			bad.Answers = map[string][]string{"second-sort": {"First answer"}}
		}
		if err := env.store.RecoverWebUIQuestionAnswer(ctx, env.worker, env.runtime, session, bad); !errors.Is(err, ErrEventTarget) {
			t.Fatalf("%s: %v", mutation, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := env.store.RecoverWebUIQuestionAnswer(ctx, env.worker, env.runtime, env.session, answer); err != nil {
			t.Fatal(err)
		}
	}
	got, err := env.store.WebUIQuestionAnswers(ctx, env.session)
	if err != nil || len(got) != 1 {
		t.Fatalf("history: %+v %v", got, err)
	}
	if got[0].ApprovalID != id.String() || got[0].TurnID != "turn" || got[0].ItemID != "native-question" || len(got[0].Fields) != 2 || got[0].Fields[0].Answer != "First answer" || got[0].Fields[1].Answer != "[REDACTED]" {
		t.Fatalf("history: %+v", got)
	}
	// Later partial snapshots can omit earlier fields. The preserved original
	// question rows must still supply the complete answer history in order.
	reduced := answer
	reduced.Questions = reduced.Questions[1:]
	reducedRaw, _ := json.Marshal(reduced)
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET request_payload=$2 WHERE approval_id=$1`, id, string(reducedRaw)); err != nil {
		t.Fatal(err)
	}
	if history, err := env.store.WebUIQuestionAnswers(ctx, env.session); err != nil || len(history) != 1 || len(history[0].Fields) != 2 {
		t.Fatalf("reduced snapshot lost answers: %+v %v", history, err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET request_payload=$2 WHERE approval_id=$1`, id, string(raw)); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(got)
	if strings.Contains(string(data), "private answer") {
		t.Fatal("secret exposed")
	}
	edits := questionAnswerEdits(t, env.store)
	if len(edits) != 2 || edits[501].Answer != "First answer" || edits[502].Answer != "[REDACTED]" {
		t.Fatalf("Telegram edits: %+v", edits)
	}
	answer.Answers["second-sort"] = []string{"conflicting"}
	if err := env.store.RecoverWebUIQuestionAnswer(ctx, env.worker, env.runtime, env.session, answer); !errors.Is(err, ErrEventTarget) {
		t.Fatal("overwrote an existing answer", err)
	}
	var state string
	if err := env.store.pool.QueryRow(ctx, `SELECT state FROM approvals WHERE approval_id=$1`, id).Scan(&state); err != nil || state != "cleared" {
		t.Fatal("recovery changed approval state", state, err)
	}
	if _, err := env.store.WebUIQuestionAnswers(ctx, uuid.New()); !errors.Is(err, ErrEventTarget) {
		t.Fatal("unknown session exposed history", err)
	}
	for _, statement := range []string{`UPDATE sessions SET archived=TRUE WHERE session_id=$1`, `UPDATE sessions SET archived=FALSE WHERE session_id=$1`} {
		if _, err := env.store.pool.Exec(ctx, statement, env.session); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(statement, "=TRUE") {
			if _, err := env.store.WebUIQuestionAnswers(ctx, env.session); !errors.Is(err, ErrEventTarget) {
				t.Fatal("archived history exposed", err)
			}
		}
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE workers SET enabled=FALSE WHERE worker_id=$1`, env.worker); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.WebUIQuestionAnswers(ctx, env.session); !errors.Is(err, ErrEventTarget) {
		t.Fatal("revoked worker history exposed", err)
	}
}

func TestWebUIQuestionAnswersIgnoreUnconfirmedDrafts(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	id := insertPendingQuestion(t, env, env.session, "turn", false, protocol.Question{ID: "field", Prompt: "Question?"})
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET input_answers='{"field":["unconfirmed draft"]}' WHERE approval_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if got, err := env.store.WebUIQuestionAnswers(ctx, env.session); err != nil || len(got) != 0 {
		t.Fatalf("draft leaked: %+v %v", got, err)
	}
}

func TestWebUIQuestionAnswerHistoryDoesNotDuplicateAsyncNativeEcho(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	id := insertPendingQuestion(t, env, env.session, "async-turn", true, protocol.Question{ID: "field", Prompt: "Question already answered by an ordinary native prompt?"})
	var raw []byte
	if err := env.store.pool.QueryRow(ctx, `SELECT request_payload FROM approvals WHERE approval_id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var answer protocol.Approval
	_ = json.Unmarshal(raw, &answer)
	answer.Answers = map[string][]string{"field": {"Already visible in native history"}}
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET state='cleared',resolved_at=`+sqliteNow+` WHERE approval_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecoverWebUIQuestionAnswer(ctx, env.worker, env.runtime, env.session, answer); err != nil {
		t.Fatal(err)
	}
	got, err := env.store.WebUIQuestionAnswers(ctx, env.session)
	if err != nil || len(got) != 0 {
		t.Fatalf("Duplicated async native answer: %+v %v", got, err)
	}
}
