package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// SkillRequester preserves the requesting user's scope at both menu steps.
func (s *Store) SkillRequester(ctx context.Context, commandID uuid.UUID) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx, `SELECT telegram_user_id FROM commands
        WHERE command_id=$1 AND source='telegram' AND operation='codex_command'
          AND json_extract(payload, '$.arguments.codex.name') IN ('skill','skills')
          AND telegram_user_id IS NOT NULL`, commandID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && userID == 0) {
		return 0, ErrTelegramTarget
	}
	if err != nil {
		return 0, fmt.Errorf("registry: read skill requester: %w", err)
	}
	return userID, nil
}

func consumeSkillCallback(ctx context.Context, tx *dbTx, in IncomingUpdate, callback callbackPayload, sessionID *uuid.UUID) (AcceptResult, error) {
	if sessionID == nil || *sessionID == uuid.Nil || callback.RuntimeID == "" || callback.Generation <= 0 || !validSkillMenuArgs(callback.Decision) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	target, err := sessionRoute(ctx, tx, *sessionID)
	if err != nil {
		return AcceptResult{}, callbackTargetError(err)
	}
	if !callbackMatchesTarget(callback, target) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err := validateSkillCallbackOrigin(ctx, tx, callback.QuestionID, *sessionID, callback.RuntimeID, callback.Generation, in.BotID, in.UserID, in.ChatID, in.TopicID); err != nil {
		return AcceptResult{}, err
	}
	if err := validateSkillMenuChoice(ctx, tx, callback.QuestionID, callback.Decision); err != nil {
		return AcceptResult{}, err
	}
	result, err := acceptRoutedCodexCommand(ctx, tx, in, target, "skill", callback.Decision)
	if err != nil {
		return AcceptResult{}, err
	}
	// Claim every button in this step atomically. The next step is tied to the
	// newly queued command, so two taps cannot create competing settings.
	used, err := tx.Exec(ctx, `UPDATE telegram_callbacks SET used_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE action='skill' AND used_at IS NULL
          AND json_extract(payload, '$.question_id')=$1`, callback.QuestionID)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: consume skill menu: %w", err)
	}
	if used.RowsAffected() == 0 {
		return AcceptResult{}, ErrCallbackInvalid
	}
	return result, nil
}

// Only a skill or page actually offered by this menu may be selected.
func validateSkillMenuChoice(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, commandID, args string) error {
	var valid bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands c
        JOIN events e ON e.worker_id=c.worker_id AND e.runtime_id=c.runtime_id
          AND e.runtime_generation=c.runtime_generation AND e.session_id=c.session_id
        JOIN json_each(e.payload, '$.skill_menu.options') choice
        WHERE c.command_id=$1 AND c.status='completed' AND e.kind='command_completed'
          AND json_extract(e.payload, '$.command_id')=c.command_id
          AND json_extract(choice.value, '$.args')=$2)`, commandID, args).Scan(&valid)
	if err != nil {
		return fmt.Errorf("registry: validate offered skill choice: %w", err)
	}
	if !valid {
		return ErrCallbackInvalid
	}
	return nil
}

func validSkillMenuArgs(args string) bool {
	_, page := protocol.SkillPage(args)
	return args != "" && (page || protocol.ValidSkillName(args))
}

func validateSkillCallbackOrigin(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, commandID string, sessionID uuid.UUID, runtimeID string, generation int64, botID string, userID, chatID, topicID int64) error {
	if err := validateSettingsCallbackOrigin(ctx, db, "skill", commandID, sessionID, runtimeID, generation, botID, userID, chatID, topicID); err == nil {
		return nil
	}
	return validateSettingsCallbackOrigin(ctx, db, "skills", commandID, sessionID, runtimeID, generation, botID, userID, chatID, topicID)
}
