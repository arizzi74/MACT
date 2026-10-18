package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Archive directory listings and progress are private worker-control results,
// never Telegram messages or transcript items. Validate their immutable command
// before allowing a completion to change session inventory.
func applySessionArchiveEvent(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event, target eventTarget) (bool, error) {
	var result protocol.Result
	if json.Unmarshal(event.Data, &result) != nil {
		return false, nil
	}
	if result.CommandID == "" {
		if result.SessionArchive != nil {
			return true, ErrEventTarget
		}
		return false, nil
	}
	id, err := uuid.Parse(result.CommandID)
	if err != nil {
		return true, ErrEventTarget
	}
	var operation string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT operation,payload FROM commands WHERE command_id=$1 AND worker_id=$2 AND runtime_id IS $3 AND runtime_generation=$4 AND session_id IS $5`, id, workerID, target.runtimeID, int64(event.RuntimeGeneration), target.sessionID).Scan(&operation, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		if result.SessionArchive != nil || event.Kind == "session_archive_progress" {
			return true, ErrEventTarget
		}
		return false, nil
	}
	if err != nil {
		return true, err
	}
	op := protocol.Operation(operation)
	if !sessionActionOperation(op) {
		if result.SessionArchive != nil || event.Kind == "session_archive_progress" {
			return true, ErrEventTarget
		}
		return false, nil
	}
	var command protocol.Command
	if json.Unmarshal(raw, &command) != nil || command.Validate() != nil || target.runtimeID == nil {
		return true, ErrEventTarget
	}
	if result.TurnID != "" || result.History != nil || result.Workspace != nil || result.ModelMenu != nil || result.UsageMenu != nil || result.Permissions != nil {
		return true, ErrEventTarget
	}
	progress := event.Kind == "session_archive_progress"
	if event.Kind != "command_completed" && event.Kind != "command_failed" && event.Kind != "command_result_unknown" && !progress {
		return true, ErrEventTarget
	}
	if progress {
		if op == protocol.RenameSession || result.SessionArchive == nil || result.Session != nil || result.Error != nil || result.SessionArchive.Archive != nil || len(result.SessionArchive.Archives) != 0 {
			return true, ErrEventTarget
		}
		if result.SessionArchive.ProjectDeleted || result.SessionArchive.WarningCode != "" || result.SessionArchive.NextOffset != nil {
			return true, ErrEventTarget
		}
		if result.SessionArchive.Validate() != nil {
			return true, ErrEventTarget
		}
		return true, updateCommandOutcome(ctx, tx, id, workerID, event, target, result)
	}
	success := event.Kind == "command_completed"
	if success && result.Error != nil {
		return true, ErrEventTarget
	}
	if !success {
		if result.Session != nil || result.SessionArchive != nil {
			return true, ErrEventTarget
		}
		return true, updateCommandOutcome(ctx, tx, id, workerID, event, target, result)
	}
	if op == protocol.RenameSession {
		if result.SessionArchive != nil || result.Session == nil || result.Session.Name == "" || result.Session.Archived || result.Session.Deleted {
			return true, ErrEventTarget
		}
		if _, err := protocol.NormalizeSessionTitle(result.Session.Name); err != nil {
			return true, ErrEventTarget
		}
	} else if result.SessionArchive == nil || result.SessionArchive.Validate() != nil {
		return true, ErrEventTarget
	}
	if a := result.SessionArchive; a != nil {
		if (a.ProjectDeleted || a.WarningCode != "") && (op != protocol.ArchiveSession || !command.Arguments.SessionArchive.DeleteProject) {
			return true, ErrEventTarget
		}
		if a.ProjectDeleted && a.WarningCode != "" || (a.NextOffset != nil && op != protocol.ListSessionArchives) {
			return true, ErrEventTarget
		}
	}
	switch op {
	case protocol.ArchiveSession:
		if result.Session == nil || !result.Session.Archived || result.Session.Deleted || result.Session.Loaded || result.Session.ActiveTurnID != "" || result.SessionArchive.Archive == nil || len(result.SessionArchive.Archives) != 0 {
			return true, ErrEventTarget
		}
	case protocol.ListSessionArchives:
		if result.Session != nil || result.SessionArchive.Archive != nil {
			return true, ErrEventTarget
		}
	case protocol.RestoreSession:
		if result.Session == nil || result.Session.Archived || result.Session.Deleted || result.Session.ActiveTurnID != "" || result.SessionArchive.Archive == nil || len(result.SessionArchive.Archives) != 0 || result.SessionArchive.Archive.ID != command.Arguments.SessionArchive.ArchiveID {
			return true, ErrEventTarget
		}
	}
	if result.Session != nil {
		p := result.Session
		if p.WorkerID != command.WorkerID || p.RuntimeID != command.RuntimeID || p.ThreadID == "" || p.ID == "" {
			return true, ErrEventTarget
		}
		if target.sessionID != nil && (p.ID != target.sessionID.String() || p.ThreadID != command.ThreadID || p.CWD != command.Arguments.CWD) {
			return true, ErrEventTarget
		}
		if target.runtimeCurrent {
			if err := upsertProtocolSession(ctx, tx, workerID, *target.runtimeID, target.sessionID, *p, event.RuntimeGeneration); err != nil {
				return true, err
			}
		}
	}
	return true, updateCommandOutcome(ctx, tx, id, workerID, event, target, result)
}
