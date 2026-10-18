package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

var ErrWebUISessionActionNotFound = errors.New("registry: web session action not found")

// WebUISessionAction is a projection of the initiating command's own validated
// events. It exposes neither raw commands nor arbitrary worker error text.
type WebUISessionAction struct {
	CommandID           string                         `json:"command_id"`
	WorkerID            string                         `json:"worker_id"`
	RuntimeID           string                         `json:"runtime_id"`
	RuntimeGeneration   uint64                         `json:"runtime_generation"`
	SessionID           string                         `json:"session_id,omitempty"`
	Status              string                         `json:"status"`
	Pending             bool                           `json:"pending"`
	ErrorCode           string                         `json:"error_code,omitempty"`
	Message             string                         `json:"message"`
	Phase               string                         `json:"phase,omitempty"`
	ArchiveDirectory    string                         `json:"archive_directory,omitempty"`
	Archive             *protocol.SessionArchiveEntry  `json:"archive,omitempty"`
	Archives            []protocol.SessionArchiveEntry `json:"archives,omitempty"`
	NextOffset          *int                           `json:"next_offset,omitempty"`
	Session             *protocol.Session              `json:"session,omitempty"`
	ProjectDeleted      bool                           `json:"project_deleted,omitempty"`
	WarningCode         string                         `json:"warning_code,omitempty"`
	RetainedProjectPath string                         `json:"retained_project_path,omitempty"`
}

func sessionActionOperation(op protocol.Operation) bool {
	return op == protocol.RenameSession || op == protocol.ArchiveSession || op == protocol.ListSessionArchives || op == protocol.RestoreSession
}

func (s *Store) QueueWebUISessionEdit(ctx context.Context, sessionID, requestID uuid.UUID, name string) (WebUISessionAction, error) {
	name, err := protocol.NormalizeSessionTitle(name)
	if err != nil {
		return WebUISessionAction{}, &protocol.Error{Code: "session_name_invalid", Message: err.Error()}
	}
	return s.queueWebUISessionAction(ctx, sessionID, requestID, protocol.RenameSession, protocol.Arguments{SessionName: name}, "")
}

func (s *Store) QueueWebUISessionArchive(ctx context.Context, sessionID, requestID uuid.UUID, cwd string, deleteProject bool) (WebUISessionAction, error) {
	if cwd == "" {
		return WebUISessionAction{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "Refresh the session and confirm its project directory before archiving."}
	}
	return s.queueWebUISessionAction(ctx, sessionID, requestID, protocol.ArchiveSession, protocol.Arguments{SessionArchive: &protocol.SessionArchiveRequest{DeleteProject: deleteProject}}, cwd)
}

func (s *Store) queueWebUISessionAction(ctx context.Context, sessionID, requestID uuid.UUID, op protocol.Operation, args protocol.Arguments, expectedCWD string) (WebUISessionAction, error) {
	if sessionID == uuid.Nil || requestID == uuid.Nil || (op != protocol.RenameSession && op != protocol.ArchiveSession) {
		return WebUISessionAction{}, ErrWebUISessionActionNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebUISessionAction{}, err
	}
	defer tx.Rollback(ctx)
	if prior, e := readWebUISessionAction(ctx, tx, uuid.Nil, sessionID, requestID, op); e == nil {
		if err = matchingSessionAction(ctx, tx, requestID, args, expectedCWD); err != nil {
			return prior, err
		}
		return prior, tx.Commit(ctx)
	} else if !errors.Is(e, ErrWebUISessionActionNotFound) {
		return prior, e
	}
	if err = unusedSessionActionID(ctx, tx, requestID); err != nil {
		return WebUISessionAction{}, err
	}
	var workerID, runtimeID uuid.UUID
	var generation uint64
	var threadID, cwd, state, connectivity, runtimeState string
	var active string
	var enabled, supported bool
	capability := "$.supports_session_archives"
	if op == protocol.RenameSession {
		capability = "$.supports_session_editing"
	}
	err = tx.QueryRow(ctx, `SELECT s.worker_id,s.runtime_id,r.generation,s.codex_thread_id,COALESCE(s.cwd,''),s.state,COALESCE(s.active_turn_id,''),w.enabled,w.connectivity,r.state,
        COALESCE(json_extract(w.heartbeat_metadata,$2)=1,FALSE)
        FROM sessions s JOIN workers w ON w.worker_id=s.worker_id JOIN runtimes r ON r.runtime_id=s.runtime_id AND r.worker_id=s.worker_id
        WHERE s.session_id=$1 AND s.archived=FALSE`, sessionID, capability).Scan(&workerID, &runtimeID, &generation, &threadID, &cwd, &state, &active, &enabled, &connectivity, &runtimeState, &supported)
	if errors.Is(err, sql.ErrNoRows) {
		return WebUISessionAction{}, ErrWebUISessionActionNotFound
	}
	if err != nil {
		return WebUISessionAction{}, err
	}
	if !enabled || connectivity != "online" || runtimeState != "running" {
		return WebUISessionAction{}, &protocol.Error{Code: "worker_unavailable", Message: "The worker must be connected and running for this action."}
	}
	if !supported {
		return WebUISessionAction{}, &protocol.Error{Code: protocol.UnsupportedOperation, Message: "Update this worker to enable session editing and archives."}
	}
	if expectedCWD != "" && cwd != expectedCWD {
		return WebUISessionAction{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The project directory changed. Reopen the session editor before archiving."}
	}
	if active != "" || (state != "idle" && state != "not_loaded" && state != "failed" && state != "") {
		return WebUISessionAction{}, sessionActionBusy()
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE session_id=$1 AND status IN ('pending','dispatched','acknowledged') AND operation<>'read_history')
        OR EXISTS(SELECT 1 FROM approvals WHERE session_id=$1 AND state='pending')`, sessionID).Scan(&busy)
	if err != nil {
		return WebUISessionAction{}, err
	}
	if busy {
		return WebUISessionAction{}, sessionActionBusy()
	}
	args.CWD = cwd
	command := protocol.Command{ID: requestID.String(), WorkerID: workerID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: generation, SessionID: sessionID.String(), ThreadID: threadID, Operation: op, Arguments: args, CreatedAt: time.Now().UTC()}
	command.ExpiresAt = command.CreatedAt.Add(2 * time.Minute)
	if err = insertWebUISessionAction(ctx, tx, command); err != nil {
		return WebUISessionAction{}, err
	}
	result, err := readWebUISessionAction(ctx, tx, uuid.Nil, sessionID, requestID, op)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (s *Store) QueueWebUIArchiveOperation(ctx context.Context, target WebUICreationTarget, op protocol.Operation, request protocol.SessionArchiveRequest) (WebUISessionAction, error) {
	if (op != protocol.ListSessionArchives && op != protocol.RestoreSession) || target.WorkerID == uuid.Nil || target.RuntimeID == uuid.Nil || target.RequestID == uuid.Nil || target.RuntimeGeneration == 0 || target.RuntimeGeneration > math.MaxInt64 {
		return WebUISessionAction{}, ErrWebUISessionActionNotFound
	}
	command := protocol.Command{ID: target.RequestID.String(), WorkerID: target.WorkerID.String(), RuntimeID: target.RuntimeID.String(), RuntimeGeneration: target.RuntimeGeneration, Operation: op, Arguments: protocol.Arguments{SessionArchive: &request}, CreatedAt: time.Now().UTC()}
	command.ExpiresAt = command.CreatedAt.Add(2 * time.Minute)
	if err := command.Validate(); err != nil {
		return WebUISessionAction{}, &protocol.Error{Code: "invalid_archive_request", Message: "Select a valid archive on this worker."}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebUISessionAction{}, err
	}
	defer tx.Rollback(ctx)
	if prior, e := readWebUISessionAction(ctx, tx, target.WorkerID, uuid.Nil, target.RequestID, op); e == nil {
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT payload FROM commands WHERE command_id=$1`, target.RequestID).Scan(&raw); err != nil {
			return prior, err
		}
		var old protocol.Command
		if json.Unmarshal(raw, &old) != nil || old.RuntimeID != command.RuntimeID || old.RuntimeGeneration != command.RuntimeGeneration || !sameSessionActionArguments(old.Arguments, command.Arguments) {
			return prior, webUICreationConflict()
		}
		return prior, tx.Commit(ctx)
	} else if !errors.Is(e, ErrWebUISessionActionNotFound) {
		return prior, e
	}
	if err = unusedSessionActionID(ctx, tx, target.RequestID); err != nil {
		return WebUISessionAction{}, err
	}
	var generation uint64
	var enabled, supported bool
	var connectivity, state string
	err = tx.QueryRow(ctx, `SELECT r.generation,w.enabled,w.connectivity,r.state,COALESCE(json_extract(w.heartbeat_metadata,'$.supports_session_archives')=1,FALSE)
        FROM runtimes r JOIN workers w ON w.worker_id=r.worker_id WHERE r.runtime_id=$1 AND r.worker_id=$2`, target.RuntimeID, target.WorkerID).Scan(&generation, &enabled, &connectivity, &state, &supported)
	if errors.Is(err, sql.ErrNoRows) {
		return WebUISessionAction{}, ErrWebUISessionActionNotFound
	}
	if err != nil {
		return WebUISessionAction{}, err
	}
	if generation != target.RuntimeGeneration {
		return WebUISessionAction{}, &protocol.Error{Code: protocol.StaleRuntime, Message: "The worker runtime changed. Refresh the session list."}
	}
	if !enabled || connectivity != "online" || state != "running" {
		return WebUISessionAction{}, &protocol.Error{Code: "worker_unavailable", Message: "The worker must be connected and running to list or restore archives."}
	}
	if !supported {
		return WebUISessionAction{}, &protocol.Error{Code: protocol.UnsupportedOperation, Message: "Update this worker to enable session archives."}
	}
	if err = insertWebUISessionAction(ctx, tx, command); err != nil {
		return WebUISessionAction{}, err
	}
	result, err := readWebUISessionAction(ctx, tx, target.WorkerID, uuid.Nil, target.RequestID, op)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func insertWebUISessionAction(ctx context.Context, tx *dbTx, c protocol.Command) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if _, err := protocol.NewEnvelope("command", c); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO commands(command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status,expires_at)
        VALUES($1,'webui',$2,$3,$4,$5,$6,$7,'pending',$8)`, uuid.MustParse(c.ID), uuid.MustParse(c.WorkerID), uuid.MustParse(c.RuntimeID), int64(c.RuntimeGeneration), nullUUID(c.SessionID), string(c.Operation), string(raw), c.ExpiresAt)
	return err
}

func unusedSessionActionID(ctx context.Context, tx *dbTx, id uuid.UUID) error {
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE command_id=$1)`, id).Scan(&found); err != nil {
		return err
	}
	if found {
		return webUICreationConflict()
	}
	return nil
}
func matchingSessionAction(ctx context.Context, tx *dbTx, id uuid.UUID, args protocol.Arguments, cwd string) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM commands WHERE command_id=$1`, id).Scan(&raw); err != nil {
		return err
	}
	var old protocol.Command
	if json.Unmarshal(raw, &old) != nil {
		return webUICreationConflict()
	}
	args.CWD = old.Arguments.CWD
	if cwd != "" && cwd != args.CWD {
		return webUICreationConflict()
	}
	if !sameSessionActionArguments(old.Arguments, args) {
		return webUICreationConflict()
	}
	return nil
}
func sameSessionActionArguments(a, b protocol.Arguments) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func sessionActionBusy() *protocol.Error {
	return &protocol.Error{Code: protocol.SessionBusy, Message: "Wait for this session, its subagents and pending work to finish before changing or archiving it."}
}

func (s *Store) WebUISessionAction(ctx context.Context, workerID, sessionID, requestID uuid.UUID, op protocol.Operation) (WebUISessionAction, error) {
	return readWebUISessionAction(ctx, s.pool, workerID, sessionID, requestID, op)
}

func readWebUISessionAction(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, workerID, sessionID, requestID uuid.UUID, op protocol.Operation) (WebUISessionAction, error) {
	var result WebUISessionAction
	var code string
	if !sessionActionOperation(op) || requestID == uuid.Nil || ((sessionID == uuid.Nil) == (workerID == uuid.Nil)) {
		return result, ErrWebUISessionActionNotFound
	}
	err := db.QueryRow(ctx, `SELECT command_id,worker_id,runtime_id,runtime_generation,COALESCE(session_id,''),status,COALESCE(error_code,'')
        FROM commands WHERE command_id=$1 AND operation=$2 AND source='webui'
          AND (($3 IS NOT NULL AND session_id=$3) OR ($3 IS NULL AND session_id IS NULL AND worker_id=$4))`, requestID, string(op), sessionActionUUID(sessionID), sessionActionUUID(workerID)).Scan(&result.CommandID, &result.WorkerID, &result.RuntimeID, &result.RuntimeGeneration, &result.SessionID, &result.Status, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrWebUISessionActionNotFound
	}
	if err != nil {
		return result, err
	}
	result.Pending = result.Status == "pending" || result.Status == "dispatched" || result.Status == "acknowledged"
	if result.Pending {
		result.Message = "Waiting for the worker to complete the request."
		result.Phase = "preparing"
	} else if result.Status == "completed" {
		result.Message = "Session action completed."
	} else {
		result.ErrorCode, result.Message = sessionActionFailure(code, result.Status)
		return result, nil
	}
	var raw []byte
	kind := "command_completed"
	if result.Pending {
		kind = "session_archive_progress"
	}
	err = db.QueryRow(ctx, `SELECT payload FROM events WHERE worker_id=$1 AND runtime_id=$2 AND runtime_generation=$3 AND kind=$4
        AND json_extract(payload,'$.command_id')=$5 ORDER BY event_seq DESC LIMIT 1`, uuid.MustParse(result.WorkerID), uuid.MustParse(result.RuntimeID), int64(result.RuntimeGeneration), kind, result.CommandID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) && result.Pending {
		return result, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	var completed protocol.Result
	if err != nil || json.Unmarshal(raw, &completed) != nil || completed.CommandID != result.CommandID || completed.Error != nil {
		result.Status, result.Pending, result.ErrorCode, result.Message = "outcome_unknown", false, "command_outcome_unknown", "The result could not be confirmed. Check the session and archives before retrying."
		return result, nil
	}
	if completed.SessionArchive != nil {
		if err := completed.SessionArchive.Validate(); err != nil {
			return result, ErrEventTarget
		}
		a := completed.SessionArchive
		result.Phase, result.ArchiveDirectory, result.Archive, result.Archives, result.NextOffset = a.Phase, a.ArchiveDirectory, a.Archive, a.Archives, a.NextOffset
		result.ProjectDeleted, result.WarningCode = a.ProjectDeleted, a.WarningCode
		result.RetainedProjectPath = a.RetainedProjectPath
	}
	if !result.Pending {
		result.Session = completed.Session
		switch op {
		case protocol.RenameSession:
			result.Message = "Session name saved."
		case protocol.ArchiveSession:
			result.Message = "Session archived."
			if result.WarningCode == "project_cleanup_failed" {
				result.Message = "Session archived. Project folder cleanup could not finish; the verified archive was kept."
				if result.RetainedProjectPath != "" {
					result.Message += " Retained project files: " + result.RetainedProjectPath
				}
			}
		case protocol.ListSessionArchives:
			result.Message = "Archives ready."
		case protocol.RestoreSession:
			result.Message = "Session restored."
		}
	}
	return result, nil
}

func sessionActionUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func sessionActionFailure(code, status string) (string, string) {
	if status == "expired" {
		return "expired", "The request expired. Check the session and archives before retrying."
	}
	if status == "outcome_unknown" {
		return "command_outcome_unknown", "The outcome is unknown. Check the session and archives before retrying."
	}
	switch code {
	case protocol.SessionBusy, "archive_busy":
		return code, "The session, project or runtime has pending work. Wait for it to finish and retry."
	case "shared_directory", "shared_workspace":
		return code, "Another session uses this project directory. Keep the project folder or close the other sessions first."
	case "archive_conflict", "path_exists":
		return code, "The original project path or session already exists. Resolve the conflict before restoring; nothing was overwritten."
	case "archive_incompatible", "archive_unsupported", protocol.CodexMethodUnsupported:
		return code, "This archive or Codex version cannot be restored by this worker."
	case "archive_invalid", "archive_corrupt":
		return code, "The archive failed verification. The original data has been kept."
	case "archive_not_found":
		return code, "The archive is no longer available. Refresh the archive list."
	case protocol.InvalidWorkspace:
		return code, "The project directory changed, is protected or is outside the worker's allowed roots. Reopen the session editor."
	case protocol.UnsupportedOperation:
		return code, "Update this worker to enable this action."
	case protocol.StaleRuntime, protocol.UnknownSession, protocol.CodexUnavailable:
		return code, "The session or runtime changed. Refresh the session list."
	case "session_name_invalid":
		return code, "Enter a session name of 1–120 characters without control characters."
	default:
		return "command_failed", "The action failed. Check the session and archive list before retrying."
	}
}
