package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// WebUIQuestionAnswer contains only confirmed answers, never pending drafts or
// native request credentials. It also supplies a stable identity on reconnect.
type WebUIQuestionAnswer struct {
	ApprovalID string             `json:"approval_id"`
	TurnID     string             `json:"turn_id"`
	ItemID     string             `json:"item_id"`
	AnsweredAt string             `json:"answered_at"`
	Fields     []WebUIAnswerField `json:"fields"`
}
type WebUIAnswerField struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// WebUIQuestionAnswers supplements native conversation history: some agents
// store blocking answers as tool results instead of user messages.
func (s *Store) WebUIQuestionAnswers(ctx context.Context, session uuid.UUID) ([]WebUIQuestionAnswer, error) {
	var visible bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN workers w ON w.worker_id=s.worker_id
  WHERE s.session_id=$1 AND s.archived=FALSE AND w.enabled=TRUE)`, session).Scan(&visible); err != nil {
		return nil, err
	}
	if !visible {
		return nil, ErrEventTarget
	}
	rows, err := s.pool.Query(ctx, `SELECT a.approval_id,COALESCE(a.codex_turn_id,''),COALESCE(a.codex_item_id,''),a.resolved_at,
  (SELECT json_group_array(json_object('question',json(q.question),'answer',q.answer))
   FROM (SELECT question,answer FROM telegram_question_answers WHERE approval_id=a.approval_id AND answer IS NOT NULL ORDER BY rowid) q)
  FROM approvals a JOIN sessions s ON s.session_id=a.session_id JOIN workers w ON w.worker_id=s.worker_id
  WHERE a.session_id=$1 AND a.worker_id=s.worker_id AND a.runtime_id=s.runtime_id
    AND s.archived=FALSE AND w.enabled=TRUE
    AND COALESCE(json_extract(a.request_payload,'$.async'),FALSE)=FALSE
    AND a.state IN ('approved','cleared') AND a.resolved_at IS NOT NULL
    AND EXISTS(SELECT 1 FROM telegram_question_answers q WHERE q.approval_id=a.approval_id AND q.answer IS NOT NULL)
  ORDER BY a.resolved_at DESC,a.approval_id LIMIT 64`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]WebUIQuestionAnswer, 0)
	budget := 512 * 1024
	for rows.Next() {
		var entry WebUIQuestionAnswer
		var answersRaw []byte
		if err := rows.Scan(&entry.ApprovalID, &entry.TurnID, &entry.ItemID, &entry.AnsweredAt, &answersRaw); err != nil {
			return nil, err
		}
		var summaries []struct {
			Question protocol.Question `json:"question"`
			Answer   string            `json:"answer"`
		}
		if err := json.Unmarshal(answersRaw, &summaries); err != nil {
			return nil, err
		}
		for _, summary := range summaries {
			answer := summary.Answer
			if summary.Question.Secret {
				answer = "[REDACTED]"
			}
			entry.Fields = append(entry.Fields, WebUIAnswerField{Question: summary.Question.Prompt, Answer: answer})
		}
		if len(entry.Fields) == 0 {
			continue
		}
		raw, _ := json.Marshal(entry)
		if len(raw) > budget {
			break
		}
		budget -= len(raw)
		out = append(out, entry)
	}
	return out, rows.Err()
}

// RecoverWebUIQuestionAnswer repairs a historical native reply that bypassed
// the worker's durable event path. This is an operator-only method, not an HTTP
// endpoint. The caller must verify the native transcript and supply the exact
// original request identity and complete question set. It never contacts an
// agent or changes the running turn, and cannot overwrite a different answer.
func (s *Store) RecoverWebUIQuestionAnswer(ctx context.Context, workerID, runtimeID, sessionID uuid.UUID, answer protocol.Approval) error {
	id, err := uuid.Parse(answer.ID)
	if err != nil || id == uuid.Nil {
		return ErrEventTarget
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var generation int64
	var requestRaw, answersRaw []byte
	err = tx.QueryRow(ctx, `SELECT runtime_generation,request_payload,input_answers FROM approvals
  WHERE approval_id=$1 AND worker_id=$2 AND runtime_id=$3 AND session_id=$4
   AND codex_request_id=$5 AND codex_thread_id=$6 AND COALESCE(codex_turn_id,'')=$7
   AND COALESCE(codex_item_id,'')=$8 AND approval_type=$9 AND state='cleared' AND resolved_at IS NOT NULL`,
		id, workerID, runtimeID, sessionID, answer.RequestID, answer.ThreadID, answer.TurnID, answer.ItemID, answer.Type).Scan(&generation, &requestRaw, &answersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEventTarget
	}
	if err != nil {
		return err
	}
	var original protocol.Approval
	var existing map[string][]string
	if json.Unmarshal(requestRaw, &original) != nil || json.Unmarshal(answersRaw, &existing) != nil || len(original.Questions) == 0 || len(answer.Answers) != len(original.Questions) {
		return ErrEventTarget
	}
	for _, q := range original.Questions {
		values, ok := answer.Answers[q.ID]
		if !ok || len(values) != 1 || values[0] == "" {
			return ErrEventTarget
		}
		if q.Secret {
			values = []string{"[REDACTED]"}
		}
		if previous := existing[q.ID]; len(previous) > 0 && (len(previous) != 1 || previous[0] != values[0]) {
			return ErrEventTarget
		}
	}
	// Apply the same secret masking as a normal worker acknowledgement.
	safeAnswers := make(map[string][]string, len(answer.Answers))
	for _, q := range original.Questions {
		safeAnswers[q.ID] = answer.Answers[q.ID]
		if q.Secret {
			safeAnswers[q.ID] = []string{"[REDACTED]"}
		}
	}
	answer.Answers = safeAnswers
	if err := applyInputAnswered(ctx, tx, workerID, runtimeID, sessionID, generation, answer); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
