package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Control acknowledgements confirm worker admission before its Codex RPC has
// finished. Only a successful durable result may move progress below the input
// and its acknowledgement. Keep this in the event transaction, after applying
// turn_started so an idle async answer has acquired its current turn identity.
func repositionCompletedTelegramControlProgress(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event, target eventTarget, commandID *uuid.UUID) error {
	if commandID == nil || !target.runtimeCurrent || target.runtimeID == nil || target.sessionID == nil ||
		(event.Kind != "command_completed" && event.Kind != "turn_started") {
		return nil
	}
	var result protocol.Result
	if err := json.Unmarshal(event.Data, &result); err != nil {
		return fmt.Errorf("registry: decode completed progress control: %w", err)
	}
	if result.Error != nil || strings.TrimSpace(result.TurnID) == "" {
		return nil
	}
	var in IncomingUpdate
	var payload []byte
	var operation, expected, active string
	err := tx.QueryRow(ctx, `SELECT command.operation,command.payload,COALESCE(command.expected_turn_id,''),
        session.active_turn_id,command.telegram_bot_id,command.telegram_user_id,
        command.telegram_chat_id,COALESCE(command.telegram_message_thread_id,0)
        FROM commands command
        JOIN sessions session ON session.session_id=command.session_id
          AND session.worker_id=command.worker_id AND session.runtime_id=command.runtime_id
        JOIN runtimes runtime ON runtime.runtime_id=command.runtime_id
          AND runtime.worker_id=command.worker_id AND runtime.generation=command.runtime_generation
        WHERE command.command_id=$1 AND command.worker_id=$2 AND command.runtime_id=$3
          AND command.runtime_generation=$4 AND command.session_id=$5
          AND command.source='telegram' AND command.operation IN ('steer','input_response')
          AND command.telegram_bot_id IS NOT NULL AND command.telegram_user_id IS NOT NULL
          AND command.telegram_chat_id IS NOT NULL
          AND session.archived=FALSE AND session.state IN ('running','waiting_input','waiting_approval')
          AND session.active_turn_id=$6
          AND NOT EXISTS(SELECT 1 FROM telegram_bots bot WHERE bot.bot_id=command.telegram_bot_id
            AND (bot.enabled=0 OR bot.owner_id<>command.telegram_user_id))
          AND NOT EXISTS(SELECT 1 FROM telegram_bot_retirements retired WHERE retired.bot_id=command.telegram_bot_id)
          AND `+telegramChatAllowedSQL("command.telegram_bot_id", "command.telegram_chat_id")+`
          AND `+sessionVisibleSQL("command.telegram_bot_id", "command.telegram_chat_id", "COALESCE(command.telegram_message_thread_id,0)", "command.session_id"),
		*commandID, workerID, *target.runtimeID, int64(event.RuntimeGeneration), *target.sessionID, result.TurnID).
		Scan(&operation, &payload, &expected, &active, &in.BotID, &in.UserID, &in.ChatID, &in.TopicID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registry: resolve completed progress control: %w", err)
	}
	var command protocol.Command
	if err := json.Unmarshal(payload, &command); err != nil {
		return fmt.Errorf("registry: decode completed progress command: %w", err)
	}
	switch protocol.Operation(operation) {
	case protocol.Steer:
		if event.Kind != "command_completed" || expected == "" || expected != active {
			return nil
		}
	case protocol.InputResponse:
		if len(command.Arguments.Answers) == 0 || command.Arguments.Decision == "dismiss" || (expected != "" && expected != active) {
			return nil
		}
		if event.Kind == "turn_started" && (expected != "" || !strings.HasPrefix(command.Arguments.RequestID, "async:")) {
			return nil
		}
	}
	// The original response is also the durable once-only marker, including
	// when admission already moved progress for a question answer. This internal
	// JSON member is ignored when rendering AcceptResult. Tag even a response
	// with prepared chunks: the queue payload still gates progress until all of
	// its chunks have been sent, without changing their content or adding an ACK.
	ct, err := tx.Exec(ctx, `UPDATE telegram_deliveries SET
        payload=json_set(payload,'$.progress_reposition',json('true'),'$.progress_command_completed',json('true'))
        WHERE kind='ui_response' AND bot_id=$1 AND chat_id=$2 AND message_thread_id=$3
          AND json_extract(payload,'$.command_id')=$4
          AND COALESCE(json_extract(payload,'$.progress_command_completed'),0)<>1
          AND visibility_revoked=0 AND status<>'cancelled'`, in.BotID, in.ChatID, in.TopicID, commandID.String())
	if err != nil {
		return fmt.Errorf("registry: mark completed progress response: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return nil
	}
	return repositionTelegramProgress(ctx, tx, in)
}
