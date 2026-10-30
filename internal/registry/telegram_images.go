package registry

import (
	"context"
	"database/sql"
	"errors"
)

// TelegramImageTarget binds an attachment to the leased final delivery's own
// worker and session. A different bot selection cannot redirect a file read.
func (s *Store) TelegramImageTarget(ctx context.Context, delivery Delivery) (AdminSession, error) {
	var target AdminSession
	err := s.pool.QueryRow(ctx, `SELECT session.session_id,session.worker_id,session.runtime_id,
        session.codex_thread_id,session.cwd,runtime.generation
        FROM telegram_deliveries delivery JOIN events event ON event.event_id=delivery.event_id
        JOIN sessions session ON session.session_id=event.session_id
          AND session.worker_id=event.worker_id AND session.runtime_id=event.runtime_id
        JOIN workers worker ON worker.worker_id=session.worker_id
        JOIN runtimes runtime ON runtime.runtime_id=session.runtime_id AND runtime.worker_id=session.worker_id
        WHERE delivery.delivery_id=$1 AND delivery.attempt_count=$2 AND delivery.status='sending'
          AND delivery.next_attempt_at>`+sqliteNow+` AND delivery.visibility_revoked=0 AND delivery.bot_id=$3 AND delivery.chat_id=$4 AND delivery.message_thread_id=$5
          AND delivery.kind IN ('final_agent_message','turn_completed')
          AND session.archived=FALSE AND worker.enabled=TRUE AND runtime.state='running'
          AND runtime.generation=event.runtime_generation`, delivery.ID, delivery.Attempt, delivery.BotID, delivery.ChatID, delivery.TopicID).
		Scan(&target.ID, &target.WorkerID, &target.RuntimeID, &target.ThreadID, &target.CWD, &target.RuntimeGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminSession{}, ErrWebUIFileTarget
	}
	return target, err
}
