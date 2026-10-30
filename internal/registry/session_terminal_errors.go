package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const maxSessionTerminalErrorRunes = 8000

// The worker has already redacted public error messages at its output boundary.
// Keep a bounded copy in session metadata so inventory and reconnect snapshots
// can report a failure without reading conversation history or the event ledger.
func applySessionTerminalError(ctx context.Context, tx *dbTx, event protocol.Event, sessionID uuid.UUID, result protocol.Result) error {
	switch event.Kind {
	case "turn_started", "user_message":
		_, err := tx.Exec(ctx, `UPDATE sessions SET metadata=json_set(metadata,
            '$.terminal_error',NULL,'$.terminal_error_turn',$2,'$.terminal_error_generation',$3,
            '$.terminal_error_input_received_at',(SELECT received_at FROM events WHERE event_id=$4),
            '$.terminal_error_input_seq',$5,'$.terminal_error_prompt_id','',
            '$.terminal_error_input_command_rowid',COALESCE((SELECT MAX(rowid) FROM commands WHERE commands.session_id=sessions.session_id),0))
            WHERE session_id=$1`, sessionID, result.TurnID, int64(event.RuntimeGeneration), event.ID, int64(event.Seq))
		return err
	case "session_prompt_started", "session_prompt_failed":
		var prompt protocol.SessionPromptEvent
		if json.Unmarshal(event.Data, &prompt) != nil || prompt.StartedAt.IsZero() {
			return ErrEventTarget
		}
		if id, err := uuid.Parse(prompt.RequestID); err != nil || id == uuid.Nil {
			return ErrEventTarget
		}
		if event.Kind == "session_prompt_started" {
			_, err := tx.Exec(ctx, `UPDATE sessions SET metadata=json_set(metadata,
                '$.terminal_error',NULL,'$.terminal_error_prompt_id',$2,'$.terminal_error_prompt_started_at',$3,
                '$.terminal_error_prompt_seq',$4,'$.terminal_error_input_command_rowid',
                COALESCE((SELECT MAX(rowid) FROM commands WHERE commands.session_id=sessions.session_id),0))
                WHERE session_id=$1`, sessionID, prompt.RequestID, prompt.StartedAt.UTC().Format(time.RFC3339Nano), int64(event.Seq))
			return err
		}
		if prompt.AfterEventSeq == 0 || prompt.AfterEventSeq >= event.Seq || prompt.Error == nil {
			return ErrEventTarget
		}
		result.Error = prompt.Error
	case "command_failed":
		if result.CommandID == "" {
			return nil
		}
	case "turn_failed":
		// Transient provider errors are not terminal. New workers omit these
		// events altogether; tolerate the flag from an older/custom producer.
		var retry struct {
			WillRetry      bool `json:"will_retry"`
			WillRetryCamel bool `json:"willRetry"`
		}
		_ = json.Unmarshal(event.Data, &retry)
		if retry.WillRetry || retry.WillRetryCamel {
			return nil
		}
	default:
		return nil
	}
	message, code := "Turn failed.", ""
	if result.Error != nil {
		if value := boundedTerminalErrorText(result.Error.Message, maxSessionTerminalErrorRunes); value != "" {
			message = value
		}
		code = boundedTerminalErrorText(result.Error.Code, 256)
	}
	revision := event.ID
	if isModelCapacityError(code, message) {
		var saved []byte
		if err := tx.QueryRow(ctx, `SELECT COALESCE(json_extract(metadata,'$.capacity_retry'),'{}') FROM sessions WHERE session_id=$1`, sessionID).Scan(&saved); err != nil {
			return err
		}
		var retry CapacityRetry
		if json.Unmarshal(saved, &retry) == nil && retry.Generation == event.RuntimeGeneration {
			same := result.TurnID != "" && retry.TurnID == result.TurnID
			if event.Kind == "session_prompt_failed" {
				var prompt protocol.SessionPromptEvent
				_ = json.Unmarshal(event.Data, &prompt)
				same = retry.PromptID != "" && retry.PromptID == prompt.RequestID
			}
			if event.Kind == "command_failed" {
				same = retry.FailureCommandID != "" && retry.FailureCommandID == result.CommandID
			}
			if same {
				if retry.State == "cancelled" || retry.State == "submitted" {
					return nil
				}
				if retry.State == "waiting" {
					revision = retry.ErrorRevision
				}
			}
		}
	}
	raw, err := json.Marshal(struct {
		TurnID    string `json:"turn_id"`
		Message   string `json:"message"`
		Code      string `json:"code"`
		Revision  string `json:"revision"`
		CommandID string `json:"command_id,omitempty"`
	}{result.TurnID, message, code, revision, result.CommandID})
	if err != nil {
		return err
	}
	if event.Kind == "session_prompt_failed" {
		var prompt protocol.SessionPromptEvent
		_ = json.Unmarshal(event.Data, &prompt)
		_, err = tx.Exec(ctx, `UPDATE sessions SET metadata=json_set(metadata,'$.terminal_error',json($2))
            WHERE session_id=$1 AND COALESCE(active_turn_id,'')=''
              AND json_extract(metadata,'$.terminal_error_prompt_id')=$3
              AND json_extract(metadata,'$.terminal_error_prompt_started_at')=$4
              AND json_extract(metadata,'$.terminal_error_prompt_seq')=$5
              AND COALESCE(json_extract(metadata,'$.terminal_error_input_seq'),0)<=$5`,
			sessionID, string(raw), prompt.RequestID, prompt.StartedAt.UTC().Format(time.RFC3339Nano), int64(prompt.AfterEventSeq))
		return err
	}
	if event.Kind == "command_failed" {
		// Only a real prompt command can create a session-level failure before
		// a turn exists. A failed history/settings/maintenance command stays a
		// command result. Compare timestamps recorded by this database, never
		// a worker's clock, and reject an older prompt's late failure once a
		// newer native input or prompt command has been accepted.
		_, err = tx.Exec(ctx, `UPDATE sessions SET metadata=json_set(metadata,'$.terminal_error',json($2))
            WHERE session_id=$1 AND COALESCE(active_turn_id,'')=''
              AND EXISTS(SELECT 1 FROM commands command WHERE command.command_id=$3
                AND command.session_id=sessions.session_id AND command.operation='start_turn'
                AND command.rowid>COALESCE(json_extract(sessions.metadata,'$.terminal_error_input_command_rowid'),0)
                AND (COALESCE(json_extract(sessions.metadata,'$.terminal_error_input_received_at'),'')=''
                  OR julianday(command.created_at)>=julianday(json_extract(sessions.metadata,'$.terminal_error_input_received_at')))
                AND NOT EXISTS(SELECT 1 FROM commands newer WHERE newer.session_id=command.session_id
                  AND newer.operation IN ('start_turn','steer') AND newer.rowid>command.rowid))`,
			sessionID, string(raw), result.CommandID)
		return err
	}
	// active_turn_id alone is insufficient: an old failure can arrive after
	// the newer turn has completed. Retain the last accepted input's turn id
	// through terminal events and inventory refreshes to fence that race too.
	_, err = tx.Exec(ctx, `UPDATE sessions SET metadata=json_set(metadata,'$.terminal_error',json($2))
        WHERE session_id=$1 AND (COALESCE(active_turn_id,'')='' OR active_turn_id=$3)
          AND COALESCE(json_extract(metadata,'$.terminal_error_prompt_id'),'')=''
          AND (COALESCE(json_extract(metadata,'$.terminal_error_generation'),0)<>$4
            OR json_type(metadata,'$.terminal_error_turn') IS NULL
            OR COALESCE(json_extract(metadata,'$.terminal_error_turn'),'')=$3)`,
		sessionID, string(raw), result.TurnID, int64(event.RuntimeGeneration))
	if err != nil {
		return fmt.Errorf("registry: project terminal session error: %w", err)
	}
	return nil
}

func boundedTerminalErrorText(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(value, "\uFFFD"), "\x00", "\uFFFD"))
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return value
}
