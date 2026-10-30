package registry

import (
	"context"

	"github.com/google/uuid"
)

// A resolved tool approval is no longer actionable in any bot. Reuse the
// durable request-message deletion operation, addressed to each original
// message rather than the session's current bot or Telegram selection.
// User-input questions retain their question/answer history instead.
func enqueueResolvedApprovalCleanup(ctx context.Context, tx *dbTx, approval uuid.UUID, bot string, chat, message int64) error {
	rows, err := tx.Query(ctx, `SELECT route.bot_id,route.chat_id,route.message_id
 FROM bot_message_routes route JOIN approvals approval ON approval.approval_id=route.approval_id
 WHERE approval.approval_id=$1 AND approval.session_id=route.session_id
   AND approval.state IN ('approved','declined','cancelled','expired','cleared')
   AND approval.approval_type IN ('command_execution','file_change','permissions')
   AND COALESCE(route.question_id,'')=''
   AND route.created_at>(strftime('%Y-%m-%dT%H:%M:%f','now','-48 hours') || '000000Z')
   AND ($2='' OR (route.bot_id=$2 AND route.chat_id=$3 AND route.message_id=$4))`, approval, bot, chat, message)
	if err != nil {
		return err
	}
	type target struct {
		bot           string
		chat, message int64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.bot, &t.chat, &t.message); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, t := range targets {
		// Telegram message IDs are unique within a bot/chat, including topics.
		// Deleting never requires looking up a new session or topic binding.
		if err := enqueueInputReplyCleanup(ctx, tx, t.bot, t.chat, 0, t.message); err != nil {
			return err
		}
	}
	return nil
}
