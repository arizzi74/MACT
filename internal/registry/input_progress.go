package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Async answers can produce a native user-message echo after the accepted
// answer and its command completion have already moved progress. Repost only
// after the echo's final chunk is durably sent, using its actual session and
// destination rather than the question broadcast or the command's origin.
func repositionDeliveredTelegramInputProgress(ctx context.Context, tx *dbTx, deliveryID uuid.UUID) error {
	var in IncomingUpdate
	var sessionID string
	err := tx.QueryRow(ctx, `SELECT delivery.bot_id,delivery.chat_id,delivery.message_thread_id,event.session_id
        FROM telegram_deliveries delivery
        JOIN events event ON event.event_id=delivery.event_id
        JOIN sessions session ON session.session_id=event.session_id
          AND session.worker_id=event.worker_id AND session.runtime_id=event.runtime_id
        JOIN runtimes runtime ON runtime.runtime_id=event.runtime_id
          AND runtime.worker_id=event.worker_id AND runtime.generation=event.runtime_generation
        WHERE delivery.delivery_id=$1 AND delivery.kind='user_message' AND delivery.status='sent'
          AND delivery.visibility_revoked=0
          AND COALESCE(json_extract(delivery.payload,'$.progress_input_repositioned'),0)<>1
          AND session.archived=FALSE AND session.state IN ('running','waiting_input','waiting_approval')
          AND session.active_turn_id=json_extract(event.payload,'$.turn_id')
          AND COALESCE(session.active_turn_id,'')<>''
          AND `+telegramChatAllowedSQL("delivery.bot_id", "delivery.chat_id")+`
          AND NOT EXISTS(SELECT 1 FROM telegram_bots bot WHERE bot.bot_id=delivery.bot_id AND bot.enabled=0)
          AND NOT EXISTS(SELECT 1 FROM telegram_bot_retirements retired WHERE retired.bot_id=delivery.bot_id)
          AND `+sessionVisibleSQL("delivery.bot_id", "delivery.chat_id", "delivery.message_thread_id", "event.session_id"), deliveryID).
		Scan(&in.BotID, &in.ChatID, &in.TopicID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registry: resolve delivered input progress: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries
        SET payload=json_set(payload,'$.progress_input_repositioned',json('true'))
        WHERE delivery_id=$1`, deliveryID); err != nil {
		return err
	}
	return repositionTelegramProgressScope(ctx, tx, in, sessionID)
}
