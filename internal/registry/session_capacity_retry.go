package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const CapacityRetryDelay = 30 * time.Second

var ErrCapacityRetryUnavailable = errors.New("registry: capacity retry is unavailable or superseded")

// CapacityRetry binds one countdown and one immutable command to one failure.
// Stored alongside the terminal error, so multiple views and gateway restarts
// cannot reset the deadline or submit the continuation more than once.
type CapacityRetry struct {
	ErrorRevision    string `json:"error_revision"`
	TurnID           string `json:"turn_id,omitempty"`
	PromptID         string `json:"prompt_id,omitempty"`
	FailureCommandID string `json:"failure_command_id,omitempty"`
	State            string `json:"state"`
	DueAtMs          int64  `json:"due_at_ms"`
	CommandID        string `json:"command_id"`
	Generation       uint64 `json:"generation"`
	WorkerID         string `json:"worker_id"`
	RuntimeID        string `json:"runtime_id"`
	ThreadID         string `json:"thread_id"`
}

type capacityTarget struct {
	target       routeTarget
	runtimeState string
	state        string
	enabled      bool
	metadata     struct {
		Error *struct {
			Revision  string `json:"revision"`
			Message   string `json:"message"`
			Code      string `json:"code"`
			TurnID    string `json:"turn_id"`
			CommandID string `json:"command_id"`
		} `json:"terminal_error"`
		PromptID string         `json:"terminal_error_prompt_id"`
		Retry    *CapacityRetry `json:"capacity_retry"`
	}
}

func isModelCapacityError(code, message string) bool {
	text := strings.ToLower(message)
	for _, other := range []string{"context window", "context length", "context_length", "rate limit", "rate_limit", "usage limit", "quota", "budget", "credit balance", "cybersecurity", "authentication", "permission denied"} {
		if strings.Contains(text, other) {
			return false
		}
	}
	switch strings.ToLower(code) {
	case "model_capacity", "modelcapacity", "serveroverloaded", "server_overloaded", "overloaded_error":
		return true
	}
	return strings.Contains(text, "at capacity") || strings.Contains(text, "model capacity") || strings.Contains(text, "overloaded")
}

func readCapacityTarget(ctx context.Context, tx *dbTx, id uuid.UUID) (capacityTarget, error) {
	var t capacityTarget
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT session.worker_id,session.runtime_id,runtime.generation,session.codex_thread_id,
        COALESCE(session.active_turn_id,''),COALESCE(session.cwd,''),session.state,worker.enabled,runtime.state,session.metadata
        FROM sessions session JOIN workers worker ON worker.worker_id=session.worker_id
        JOIN runtimes runtime ON runtime.runtime_id=session.runtime_id AND runtime.worker_id=session.worker_id
        WHERE session.session_id=$1 AND session.archived=FALSE`, id).Scan(&t.target.workerID, &t.target.runtimeID, &t.target.generation, &t.target.threadID, &t.target.activeTurnID, &t.target.cwd, &t.state, &t.enabled, &t.runtimeState, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrCapacityRetryUnavailable
	}
	if err != nil {
		return t, err
	}
	t.target.sessionID = id
	if json.Unmarshal(raw, &t.metadata) != nil {
		return t, ErrCapacityRetryUnavailable
	}
	return t, nil
}

func writeCapacityRetry(ctx context.Context, tx *dbTx, id uuid.UUID, retry *CapacityRetry, clearError bool) error {
	raw, err := json.Marshal(retry)
	if err != nil {
		return err
	}
	query := `UPDATE sessions SET metadata=json_set(metadata,'$.capacity_retry',json($2)) WHERE session_id=$1`
	if clearError {
		query = `UPDATE sessions SET metadata=json_set(metadata,'$.capacity_retry',json($2),'$.terminal_error',NULL) WHERE session_id=$1`
	}
	_, err = tx.Exec(ctx, query, id, string(raw))
	return err
}

// SetCapacityRetry schedules a countdown or cancels and acknowledges its error.
// Repeated calls for the same revision never re-arm a cancelled/submitted retry.
func (s *Store) SetCapacityRetry(ctx context.Context, id uuid.UUID, revision, action string, now time.Time) (CapacityRetry, error) {
	if id == uuid.Nil || revision == "" || len(revision) > 256 || (action != "schedule" && action != "cancel") {
		return CapacityRetry{}, ErrCapacityRetryUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CapacityRetry{}, err
	}
	defer tx.Rollback(ctx)
	t, err := readCapacityTarget(ctx, tx, id)
	if err != nil {
		return CapacityRetry{}, err
	}
	before, err := s.activityRowTx(ctx, tx, id.String())
	if err != nil {
		return CapacityRetry{}, err
	}
	retry := t.metadata.Retry
	if retry != nil && retry.ErrorRevision == revision && (retry.State == "cancelled" || retry.State == "submitted" || action == "schedule" && retry.State == "superseded") {
		if action == "cancel" && retry.State == "submitted" {
			return CapacityRetry{}, ErrCapacityRetryUnavailable
		}
		return *retry, tx.Commit(ctx)
	}
	failure := t.metadata.Error
	if !t.enabled || failure == nil || failure.Revision != revision || !isModelCapacityError(failure.Code, failure.Message) {
		return CapacityRetry{}, ErrCapacityRetryUnavailable
	}
	if retry == nil || retry.ErrorRevision != revision {
		retry = &CapacityRetry{ErrorRevision: revision, TurnID: failure.TurnID, PromptID: t.metadata.PromptID, FailureCommandID: failure.CommandID, State: "waiting", DueAtMs: now.Add(CapacityRetryDelay).UnixMilli(),
			CommandID: uuid.NewString(), Generation: uint64(t.target.generation), WorkerID: t.target.workerID.String(), RuntimeID: t.target.runtimeID.String(), ThreadID: t.target.threadID}
	}
	if action == "schedule" && (t.target.activeTurnID != "" || t.runtimeState != "running" || !(t.state == "idle" || t.state == "not_loaded" || t.state == "failed" || t.state == "") || (before != nil && before.PendingQuestions > 0)) {
		return CapacityRetry{}, ErrCapacityRetryUnavailable
	}
	if action == "cancel" {
		retry.State = "cancelled"
	}
	if err := writeCapacityRetry(ctx, tx, id, retry, action == "cancel"); err != nil {
		return CapacityRetry{}, err
	}
	after, err := s.activityRowTx(ctx, tx, id.String())
	if err != nil {
		return CapacityRetry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CapacityRetry{}, err
	}
	s.publishActivityTransition("", id.String(), before, after)
	return *retry, nil
}

// QueueDueCapacityRetries runs from the gateway command dispatcher. Claiming a
// deadline and inserting its frozen command share one SQLite write transaction.
func (s *Store) QueueDueCapacityRetries(ctx context.Context, now time.Time) error {
	rows, err := s.pool.Query(ctx, `SELECT session_id FROM sessions WHERE archived=FALSE AND json_extract(metadata,'$.capacity_retry.state')='waiting'
        AND json_extract(metadata,'$.capacity_retry.due_at_ms')<=$1 LIMIT 100`, now.UnixMilli())
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.queueCapacityRetry(ctx, id, now); err != nil && !errors.Is(err, ErrCapacityRetryUnavailable) {
			return err
		}
	}
	return nil
}

func (s *Store) queueCapacityRetry(ctx context.Context, id uuid.UUID, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	t, err := readCapacityTarget(ctx, tx, id)
	if err != nil {
		return err
	}
	retry := t.metadata.Retry
	if retry == nil || retry.State != "waiting" || retry.DueAtMs > now.UnixMilli() {
		return nil
	}
	before, err := s.activityRowTx(ctx, tx, id.String())
	if err != nil {
		return err
	}
	failure := t.metadata.Error
	valid := now.UnixMilli() <= retry.DueAtMs+(2*time.Minute).Milliseconds() && t.runtimeState == "running" && t.enabled && failure != nil && failure.Revision == retry.ErrorRevision && isModelCapacityError(failure.Code, failure.Message) &&
		t.target.activeTurnID == "" && (t.state == "idle" || t.state == "not_loaded" || t.state == "failed" || t.state == "") && before != nil && before.PendingQuestions == 0 &&
		t.target.workerID.String() == retry.WorkerID && t.target.runtimeID.String() == retry.RuntimeID && t.target.threadID == retry.ThreadID && uint64(t.target.generation) == retry.Generation
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE session_id=$1 AND status IN ('pending','dispatched','acknowledged') AND operation IN ('start_turn','steer','delete_session','archive_session','restore_session'))`, id).Scan(&busy); err != nil {
		return err
	}
	if busy {
		valid = false
	}
	retry.State = "superseded"
	if valid {
		command := protocol.Command{ID: retry.CommandID, WorkerID: retry.WorkerID, RuntimeID: retry.RuntimeID, RuntimeGeneration: retry.Generation,
			SessionID: id.String(), ThreadID: retry.ThreadID, Operation: protocol.StartTurn, Arguments: protocol.Arguments{Text: "Continue", CWD: t.target.cwd}, CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
		if err := command.Validate(); err != nil {
			return err
		}
		payload, err := json.Marshal(command)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO commands(command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status,expires_at)
            VALUES($1,'webui',$2,$3,$4,$5,'start_turn',$6,'pending',$7)`, command.ID, t.target.workerID, t.target.runtimeID, t.target.generation, id, string(payload), command.ExpiresAt); err != nil {
			return err
		}
		retry.State = "submitted"
	}
	if err := writeCapacityRetry(ctx, tx, id, retry, false); err != nil {
		return err
	}
	after, err := s.activityRowTx(ctx, tx, id.String())
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.publishActivityTransition("", id.String(), before, after)
	return nil
}
