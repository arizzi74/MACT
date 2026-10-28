package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func terminalErrorEvent(t *testing.T, env eventTestEnv, seq uint64, turn, message string) protocol.Event {
	t.Helper()
	data, err := json.Marshal(protocol.Result{TurnID: turn, State: "failed", Text: "DO NOT COPY CONVERSATION TEXT",
		Error: &protocol.Error{Code: "model_capacity", Message: message}})
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "turn_failed", OccurredAt: time.Now().UTC(), Data: data}
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func terminalActivity(t *testing.T, env eventTestEnv) SessionActivity {
	t.Helper()
	snapshot, err := env.store.LiveSessionActivity(context.Background())
	if err != nil || len(snapshot.Sessions) != 1 {
		t.Fatalf("activity snapshot: %+v %v", snapshot, err)
	}
	return snapshot.Sessions[0]
}

func TestSessionTerminalErrorPersistsUntilNewInputAndFencesOldTurns(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "failed-turn", "")
	failure := terminalErrorEvent(t, env, 3, "failed-turn", "The model is at capacity. Credential [REDACTED].")
	// The failure occurred while no browser was subscribed or viewing this
	// session. A later snapshot must still contain its public explanation.
	failed := terminalActivity(t, env)
	if failed.ErrorTurnID != "failed-turn" || failed.ErrorMessage != "The model is at capacity. Credential [REDACTED]." ||
		failed.ErrorCode != "model_capacity" || failed.ErrorRevision != failure.ID {
		t.Fatalf("missing durable terminal error: %+v", failed)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	update := terminalErrorEvent(t, env, 4, "failed-turn", "Updated provider explanation")
	activityChanged(t, changes)
	updated := terminalActivity(t, env)
	if updated.ErrorMessage != "Updated provider explanation" || updated.ErrorRevision != update.ID {
		t.Fatalf("same-turn error update lost: %+v", updated)
	}
	idle := env.discovery(t)
	idle.ID, idle.Seq, idle.Kind = uuid.NewString(), 5, "session_state_changed"
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, idle); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if got := terminalActivity(t, env); got.ErrorRevision != update.ID || got.ErrorMessage != updated.ErrorMessage {
		t.Fatalf("idle inventory erased error: %+v", got)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection,
		Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	activityUnchanged(t, changes)
	progressEvent(t, env, 6, "turn_started", "new-turn", "")
	activityChanged(t, changes)
	if got := terminalActivity(t, env); got.ErrorMessage != "" || got.ErrorRevision != "" {
		t.Fatalf("new prompt retained old error: %+v", got)
	}
	progressEvent(t, env, 7, "turn_completed", "new-turn", "")
	activityChanged(t, changes)
	terminalErrorEvent(t, env, 8, "failed-turn", "Late old failure must not replace new turn")
	activityChanged(t, changes)
	if got := terminalActivity(t, env); got.ErrorMessage != "" || got.State != "idle" {
		t.Fatalf("old failure appeared after newer turn completed: %+v", got)
	}
}

func promptLifecycleEvent(t *testing.T, env eventTestEnv, seq uint64, kind string, payload any) protocol.Event {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestSessionPromptErrorFencesOtherSocketsAndLaterAcceptedInput(t *testing.T) {
	env := newEventTestEnv(t)
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	first := protocol.SessionPromptEvent{RequestID: uuid.NewString(), StartedAt: time.Now().UTC()}
	promptLifecycleEvent(t, env, 2, "session_prompt_started", first)
	first.AfterEventSeq, first.Error = 2, &protocol.Error{Code: "capacity", Message: "Model capacity unavailable"}
	failure := promptLifecycleEvent(t, env, 3, "session_prompt_failed", first)
	if got := terminalActivity(t, env); got.ErrorMessage != first.Error.Message || got.ErrorRevision != failure.ID || got.ErrorTurnID != "" {
		t.Fatalf("pre-turn rejection not durable: %+v", got)
	}
	second := protocol.SessionPromptEvent{RequestID: uuid.NewString(), StartedAt: first.StartedAt.Add(time.Nanosecond)}
	promptLifecycleEvent(t, env, 4, "session_prompt_started", second)
	if got := terminalActivity(t, env); got.ErrorMessage != "" {
		t.Fatalf("explicit new prompt did not clear error: %+v", got)
	}
	promptLifecycleEvent(t, env, 5, "session_prompt_failed", first)
	if got := terminalActivity(t, env); got.ErrorMessage != "" {
		t.Fatalf("old socket rejection replaced newer prompt: %+v", got)
	}
	progressEvent(t, env, 6, "turn_started", "accepted-native-prompt", "")
	progressEvent(t, env, 7, "turn_completed", "accepted-native-prompt", "")
	third := protocol.SessionPromptEvent{RequestID: uuid.NewString(), StartedAt: second.StartedAt.Add(time.Nanosecond)}
	promptLifecycleEvent(t, env, 8, "session_prompt_started", third)
	terminalErrorEvent(t, env, 9, "accepted-native-prompt", "Previous turn's failure arrived after next prompt dispatch")
	second.AfterEventSeq, second.Error = 4, first.Error
	promptLifecycleEvent(t, env, 10, "session_prompt_failed", second)
	if got := terminalActivity(t, env); got.ErrorMessage != "" || got.State != "idle" {
		t.Fatalf("old rejection appeared after native prompt completed: %+v", got)
	}
}

func TestSessionPromptCommandFailureRequiresLatestImmutablePromptCommand(t *testing.T) {
	for _, scenario := range []string{"prompt", "maintenance", "newer_prompt", "newer_native_input"} {
		t.Run(scenario, func(t *testing.T) {
			env := newEventTestEnv(t)
			if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			insert := func(operation string) string {
				id := uuid.NewString()
				if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO commands
                    (command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status)
                    VALUES($1,'telegram',$2,$3,1,$4,$5,'{}','pending')`, id, env.worker, env.runtime, env.session, operation); err != nil {
					t.Fatal(err)
				}
				return id
			}
			operation := "start_turn"
			if scenario == "maintenance" {
				operation = "codex_command"
			}
			id := insert(operation)
			seq := uint64(2)
			if scenario == "newer_prompt" {
				insert("start_turn")
			}
			if scenario == "newer_native_input" {
				progressEvent(t, env, seq, "turn_started", "newer", "")
				progressEvent(t, env, seq+1, "turn_completed", "newer", "")
				seq += 2
			}
			failure := promptLifecycleEvent(t, env, seq, "command_failed", protocol.Result{CommandID: id,
				State: "failed", Error: &protocol.Error{Message: "Model capacity rejected prompt"}})
			got := terminalActivity(t, env)
			if scenario == "prompt" {
				if got.ErrorMessage != "Model capacity rejected prompt" || got.ErrorRevision != failure.ID {
					t.Fatalf("prompt command rejection not projected: %+v", got)
				}
			} else if got.ErrorMessage != "" {
				t.Fatalf("unrelated/stale command failure leaked into session: %+v", got)
			}
		})
	}
}

func TestSessionTerminalErrorGenericBoundedAndTransient(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "turn", "")
	terminalErrorEvent(t, env, 3, "turn", " \n ")
	if got := terminalActivity(t, env); got.ErrorMessage != "Turn failed." || strings.Contains(got.ErrorMessage, "CONVERSATION") {
		t.Fatalf("missing generic fallback: %+v", got)
	}
	terminalErrorEvent(t, env, 4, "turn", strings.Repeat("世", 9000))
	if got := terminalActivity(t, env); len([]rune(got.ErrorMessage)) != maxSessionTerminalErrorRunes || !strings.HasSuffix(got.ErrorMessage, "…") {
		t.Fatalf("error not bounded: %d", len([]rune(got.ErrorMessage)))
	}
	progressEvent(t, env, 5, "user_message", "turn-2", "")
	if got := terminalActivity(t, env); got.ErrorMessage != "" {
		t.Fatalf("accepted native input did not clear error: %+v", got)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: 6, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "turn_failed", OccurredAt: time.Now().UTC(),
		Data: json.RawMessage(`{"turn_id":"turn-2","willRetry":true,"error":{"message":"Temporary model capacity"}}`)}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if got := terminalActivity(t, env); got.ErrorMessage != "" {
		t.Fatalf("transient retry became terminal error: %+v", got)
	}
	terminalErrorEvent(t, env, 7, "turn-2", "Final model capacity failure")
	// A restart of the gateway has no in-memory event ring or selected session.
	path := env.store.pool.path
	env.store.Close()
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	env.store = reopened
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := terminalActivity(t, env); got.ErrorMessage != "Final model capacity failure" {
		t.Fatalf("gateway restart erased error: %+v", got)
	}
}

func TestSessionTerminalErrorMigrationBackfillsExistingFailures(t *testing.T) {
	for _, scenario := range []string{"unresolved", "new_input", "old_failure_after_new_completed", "prompt_command_rejection"} {
		t.Run(scenario, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			var failure protocol.Event
			if scenario == "prompt_command_rejection" {
				id := uuid.NewString()
				if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
                    (command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status)
                    VALUES($1,'telegram',$2,$3,1,$4,'start_turn','{}','pending')`, id, env.worker, env.runtime, env.session); err != nil {
					t.Fatal(err)
				}
				failure = promptLifecycleEvent(t, env, 2, "command_failed", protocol.Result{CommandID: id,
					Error: &protocol.Error{Message: "Existing capacity failure [REDACTED]"}})
			} else {
				progressEvent(t, env, 2, "turn_started", "old", "")
				failure = terminalErrorEvent(t, env, 3, "old", "Existing capacity failure [REDACTED]")
			}
			if scenario == "new_input" || scenario == "old_failure_after_new_completed" {
				progressEvent(t, env, 4, "turn_started", "new", "")
			}
			if scenario == "old_failure_after_new_completed" {
				progressEvent(t, env, 5, "turn_completed", "new", "")
				terminalErrorEvent(t, env, 6, "old", "Stale older failure")
			}
			// Model a database created before this projection existed; keep its
			// ledger and session state exactly as the installed gateway would.
			for _, query := range []string{
				`UPDATE sessions SET metadata=json_remove(metadata,'$.terminal_error','$.terminal_error_turn','$.terminal_error_generation')`,
				`DROP INDEX events_session_turn_state_idx`,
				`DELETE FROM schema_migrations WHERE version=29`,
			} {
				if _, err := env.store.pool.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			if err := env.store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			got := terminalActivity(t, env)
			if scenario == "unresolved" || scenario == "prompt_command_rejection" {
				if got.ErrorMessage != "Existing capacity failure [REDACTED]" || got.ErrorRevision != failure.ID {
					t.Fatalf("existing error not backfilled: %+v", got)
				}
			} else if got.ErrorMessage != "" {
				t.Fatalf("migration restored resolved/stale error: %+v", got)
			}
		})
	}
}
