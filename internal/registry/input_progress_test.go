package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDeliveredInputProgressWaitsForAllEchoChunksAcrossRetryAndRestart(t *testing.T) {
	env := answerProgressEnv(t)
	ctx := t.Context()
	echo := progressEvent(t, env, 5, "user_message", "turn-a", "")
	row := claimProgress(t, env.store, 1)[0]
	if row.Kind != "user_message" {
		t.Fatalf("expected native echo: %+v", row)
	}
	if _, err := env.store.PrepareDeliveryChunks(ctx, row.ID, []json.RawMessage{json.RawMessage(`{"text":"Quoted question"}`), json.RawMessage(`{"text":"Answer"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, row.ID, 0, 210, "", "", ""); err != nil {
		t.Fatal(err)
	}
	deleteAnswerProgress(t, env.store, 0)
	if err := env.store.RetryDelivery(ctx, row.ID, time.Hour, "Telegram rate limit"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimProgress(t, reopened, 0)
	if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=`+sqliteNow+` WHERE delivery_id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if retried := claimProgress(t, reopened, 1)[0]; retried.ID != row.ID {
		t.Fatalf("echo retry changed identity: %+v", retried)
	}
	if err := reopened.MarkDeliveryChunkSent(ctx, row.ID, 1, 211, "", "", ""); err != nil {
		t.Fatal(err)
	}
	deleteAnswerProgress(t, reopened, 2)
	controlAnswerCheckpointPair(t, reopened, 300)
	if err := reopened.IngestEvent(ctx, env.worker, env.connection, echo); err != nil {
		t.Fatal(err)
	}
	// A repeated successful checkpoint must retain the same replacement pair.
	tx, err := reopened.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := repositionDeliveredTelegramInputProgress(ctx, tx, uuid.MustParse(row.ID)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	deleteAnswerProgress(t, reopened, 0)
	claimProgress(t, reopened, 0)
}

func TestDeliveredInputProgressRequiresCurrentVisibleTurn(t *testing.T) {
	for _, scenario := range []string{"current", "finished turn", "different turn", "old generation", "hidden session", "archived session"} {
		t.Run(scenario, func(t *testing.T) {
			env := answerProgressEnv(t)
			ctx := t.Context()
			progressEvent(t, env, 5, "user_message", "turn-a", "")
			echo := claimProgress(t, env.store, 1)[0]
			var query string
			var args []any
			switch scenario {
			case "finished turn":
				query, args = `UPDATE sessions SET state='idle',active_turn_id=NULL WHERE session_id=$1`, []any{env.session}
			case "different turn":
				query, args = `UPDATE sessions SET active_turn_id='new-turn' WHERE session_id=$1`, []any{env.session}
			case "old generation":
				query, args = `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, []any{env.runtime}
			case "hidden session":
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "")
				query, args = `UPDATE telegram_bindings SET session_id=$1 WHERE bot_id='bot' AND chat_id=20`, []any{other}
			case "archived session":
				query, args = `UPDATE sessions SET archived=1 WHERE session_id=$1`, []any{env.session}
			}
			if query != "" {
				if _, err := env.store.pool.Exec(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			// Single-message checkpoint covers the non-chunked sender path too.
			if err := env.store.MarkDeliverySent(ctx, echo.ID, 200, "", "", ""); err != nil {
				t.Fatal(err)
			}
			var retired int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_progress_messages WHERE retire_requested=1`).Scan(&retired); err != nil {
				t.Fatal(err)
			}
			want := 0
			if scenario == "current" {
				want = 2
			}
			if retired != want {
				t.Fatalf("%s input retired %d cards, want %d", scenario, retired, want)
			}
			var progressDeliveries int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries
                WHERE kind IN ('agent_progress_message','tool_progress_message')`).Scan(&progressDeliveries); err != nil {
				t.Fatal(err)
			}
			if progressDeliveries != 2+want {
				t.Fatalf("%s input restored stale/hidden progress: %d deliveries", scenario, progressDeliveries)
			}
		})
	}
}
