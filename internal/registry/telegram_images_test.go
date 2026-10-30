package registry

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestTelegramImagesRemainBoundToLeasedFinalWorkerAndSession(t *testing.T) {
	for _, cause := range []string{"valid", "other bot", "other chat", "other topic", "stale claim", "new generation", "archive", "revoke", "expiry"} {
		t.Run(cause, func(t *testing.T) {
			env := progressEnv(t)
			progressEvent(t, env, 2, "turn_started", "image-turn", "")
			progressEvent(t, env, 3, "turn_completed", "image-turn", "")
			rows, err := env.store.ClaimDeliveries(t.Context(), 100)
			if err != nil {
				t.Fatal(err)
			}
			var row Delivery
			for _, candidate := range rows {
				if candidate.Kind == "turn_completed" {
					row = candidate
				}
			}
			if row.ID == "" {
				t.Fatal("no final delivery", rows)
			}
			switch cause {
			case "other bot":
				row.BotID = "other"
			case "other chat":
				row.ChatID++
			case "other topic":
				row.TopicID++
			case "stale claim":
				row.Attempt++
			case "new generation":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime)
			case "archive":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE sessions SET archived=TRUE WHERE session_id=$1`, env.session)
			case "revoke":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE workers SET enabled=FALSE WHERE worker_id=$1`, env.worker)
			case "expiry":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE delivery_id=$1`, uuid.MustParse(row.ID))
			}
			if err != nil {
				t.Fatal(err)
			}
			target, err := env.store.TelegramImageTarget(t.Context(), row)
			if cause == "valid" {
				if err != nil || target.ID != env.session.String() || target.WorkerID != env.worker.String() || target.RuntimeID != env.runtime.String() || target.CWD != "/work" {
					t.Fatal(target, err)
				}
			} else if !errors.Is(err, ErrWebUIFileTarget) {
				t.Fatal("invalid image target accepted", target, err)
			}
		})
	}
}
