package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func capacityFixture(t *testing.T) (eventTestEnv, string, time.Time) {
	t.Helper()
	env := newEventTestEnv(t)
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "capacity-turn", "")
	failure := terminalErrorEvent(t, env, 3, "capacity-turn", "The model is at capacity. Try again later.")
	return env, failure.ID, time.Now().UTC()
}

func TestCapacityRetrySharesDeadlineAndQueuesOneFrozenContinue(t *testing.T) {
	env, revision, now := capacityFixture(t)
	ctx := t.Context()
	first, err := env.store.SetCapacityRetry(ctx, env.session, revision, "schedule", now)
	if err != nil || first.DueAtMs != now.Add(30*time.Second).UnixMilli() {
		t.Fatal(first, err)
	}
	second, err := env.store.SetCapacityRetry(ctx, env.session, revision, "schedule", now.Add(10*time.Second))
	if err != nil || second != first {
		t.Fatal("another view reset the deadline", second, first, err)
	}
	if err := env.store.QueueDueCapacityRetries(ctx, now.Add(29*time.Second)); err != nil {
		t.Fatal(err)
	}
	commands, _ := env.store.PendingCommands(ctx, 10)
	if len(commands) != 0 {
		t.Fatal("early retry", commands)
	}
	var group sync.WaitGroup
	for range 5 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := env.store.QueueDueCapacityRetries(context.Background(), now.Add(30*time.Second)); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	commands, err = env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("duplicate continuation commands: %+v %v", commands, err)
	}
	c := commands[0]
	if c.ID != first.CommandID || c.Operation != protocol.StartTurn || c.Arguments.Text != "Continue" || c.SessionID != env.session.String() || c.RuntimeID != env.runtime.String() || c.WorkerID != env.worker.String() || c.RuntimeGeneration != 1 {
		t.Fatalf("continuation target/input changed: %+v", c)
	}
	if got := terminalActivity(t, env); got.CapacityRetryState != "submitted" {
		t.Fatal(got)
	}
}

func TestCapacityRetryCancelClearsErrorWithoutPromptAndIgnoresLateDuplicate(t *testing.T) {
	env, revision, now := capacityFixture(t)
	ctx := t.Context()
	if _, err := env.store.SetCapacityRetry(ctx, env.session, revision, "schedule", now); err != nil {
		t.Fatal(err)
	}
	cancelled, err := env.store.SetCapacityRetry(ctx, env.session, revision, "cancel", now.Add(10*time.Second))
	if err != nil || cancelled.State != "cancelled" {
		t.Fatal(cancelled, err)
	}
	for range 2 {
		got, err := env.store.SetCapacityRetry(ctx, env.session, revision, "schedule", now.Add(20*time.Second))
		if err != nil || got.State != "cancelled" {
			t.Fatal("cancelled deadline rearmed", got, err)
		}
	}
	terminalErrorEvent(t, env, 4, "capacity-turn", "The model is still at capacity.")
	if got := terminalActivity(t, env); got.ErrorMessage != "" || got.CapacityRetryState != "cancelled" {
		t.Fatal("late capacity report resurrected a cancelled failure", got)
	}
	if err := env.store.QueueDueCapacityRetries(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	commands, _ := env.store.PendingCommands(ctx, 10)
	if len(commands) != 0 {
		t.Fatal("Cancel submitted a prompt", commands)
	}
}

func TestCapacityRetryDuplicateTerminalReportKeepsDeadlineAndRevision(t *testing.T) {
	env, revision, now := capacityFixture(t)
	first, err := env.store.SetCapacityRetry(t.Context(), env.session, revision, "schedule", now)
	if err != nil {
		t.Fatal(err)
	}
	terminalErrorEvent(t, env, 4, "capacity-turn", "The model is at capacity. Please try again.")
	activity := terminalActivity(t, env)
	if activity.ErrorRevision != revision || activity.CapacityRetryAtMs != first.DueAtMs {
		t.Fatal("duplicate changed failure identity", activity)
	}
	again, err := env.store.SetCapacityRetry(t.Context(), env.session, activity.ErrorRevision, "schedule", now.Add(15*time.Second))
	if err != nil || again.DueAtMs != first.DueAtMs {
		t.Fatal(again, err)
	}
}

func TestCapacityRetryNewPromptGenerationQuestionsAndExpirySupersede(t *testing.T) {
	for _, cause := range []string{"new prompt", "generation", "expired", "new failure", "runtime stopped", "worker revoked", "queued prompt", "pending question"} {
		t.Run(cause, func(t *testing.T) {
			env, revision, now := capacityFixture(t)
			if _, err := env.store.SetCapacityRetry(t.Context(), env.session, revision, "schedule", now); err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "new prompt":
				progressEvent(t, env, 4, "turn_started", "new-user-turn", "")
			case "generation":
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime); err != nil {
					t.Fatal(err)
				}
			case "expired":
				now = now.Add(3 * time.Minute)
			case "new failure":
				progressEvent(t, env, 4, "turn_started", "new-user-turn", "")
				terminalErrorEvent(t, env, 5, "new-user-turn", "Different failure")
			case "runtime stopped":
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE runtimes SET state='stopped' WHERE runtime_id=$1`, env.runtime); err != nil {
					t.Fatal(err)
				}
			case "worker revoked":
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE workers SET enabled=FALSE WHERE worker_id=$1`, env.worker); err != nil {
					t.Fatal(err)
				}
			case "queued prompt":
				if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO commands(command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status) VALUES($1,'telegram',$2,$3,1,$4,'start_turn','{}','pending')`, uuid.NewString(), env.worker, env.runtime, env.session); err != nil {
					t.Fatal(err)
				}
			case "pending question":
				asyncQuestionEvent(t, env, 4, 1, "user_input_requested", protocol.Approval{ID: uuid.NewString(), RequestID: "capacity-question", ThreadID: "thread-1", TurnID: "capacity-turn", Type: "input", Async: true, Questions: []protocol.Question{{ID: "q", Prompt: "Choose", Options: []string{"Yes"}}}})
			}
			if err := env.store.QueueDueCapacityRetries(t.Context(), now.Add(30*time.Second)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := env.store.pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM commands WHERE source='webui'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("superseded retry submitted", count, err)
			}
			var state string
			if err := env.store.pool.QueryRow(t.Context(), `SELECT json_extract(metadata,'$.capacity_retry.state') FROM sessions WHERE session_id=$1`, env.session).Scan(&state); err != nil || state != "superseded" {
				t.Fatal(state, err)
			}
		})
	}
}

func TestCapacityRetryCancelledPromptAndCommandFailuresDoNotResurface(t *testing.T) {
	for _, kind := range []string{"session_prompt_failed", "command_failed"} {
		t.Run(kind, func(t *testing.T) {
			env := newEventTestEnv(t)
			if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			var data any
			seq := uint64(2)
			if kind == "session_prompt_failed" {
				prompt := protocol.SessionPromptEvent{RequestID: uuid.NewString(), StartedAt: time.Now().UTC()}
				promptLifecycleEvent(t, env, seq, "session_prompt_started", prompt)
				prompt.AfterEventSeq, prompt.Error = seq, &protocol.Error{Code: "model_capacity", Message: "The model is at capacity."}
				data, seq = prompt, seq+1
			} else {
				id := uuid.NewString()
				if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO commands(command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status) VALUES($1,'telegram',$2,$3,1,$4,'start_turn','{}','failed')`, id, env.worker, env.runtime, env.session); err != nil {
					t.Fatal(err)
				}
				data = protocol.Result{CommandID: id, State: "failed", Error: &protocol.Error{Code: "model_capacity", Message: "The model is at capacity."}}
			}
			failure := promptLifecycleEvent(t, env, seq, kind, data)
			if _, err := env.store.SetCapacityRetry(t.Context(), env.session, failure.ID, "cancel", time.Now()); err != nil {
				t.Fatal(err)
			}
			promptLifecycleEvent(t, env, seq+1, kind, data)
			if got := terminalActivity(t, env); got.ErrorMessage != "" || got.CapacityRetryState != "cancelled" {
				t.Fatal("duplicate rejection resurrected cancelled countdown", got)
			}
		})
	}
}

func TestCapacityRetryCancellationAndDeadlineClaimAreSerialized(t *testing.T) {
	env, revision, now := capacityFixture(t)
	if _, err := env.store.SetCapacityRetry(t.Context(), env.session, revision, "schedule", now); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	var cancelError error
	group.Go(func() {
		_, cancelError = env.store.SetCapacityRetry(context.Background(), env.session, revision, "cancel", now.Add(30*time.Second))
	})
	group.Go(func() {
		if err := env.store.QueueDueCapacityRetries(context.Background(), now.Add(30*time.Second)); err != nil {
			t.Error(err)
		}
	})
	group.Wait()
	commands, err := env.store.PendingCommands(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if cancelError == nil && len(commands) != 0 {
		t.Fatal("confirmed cancellation submitted a continuation", commands)
	}
	if cancelError != nil && (!errors.Is(cancelError, ErrCapacityRetryUnavailable) || len(commands) != 1) {
		t.Fatal(cancelError, commands)
	}
}

func TestCapacityRetryCancelBeforeScheduleAndWrongRevision(t *testing.T) {
	env, revision, now := capacityFixture(t)
	if _, err := env.store.SetCapacityRetry(t.Context(), env.session, "wrong", "schedule", now); !errors.Is(err, ErrCapacityRetryUnavailable) {
		t.Fatal(err)
	}
	if _, err := env.store.SetCapacityRetry(t.Context(), env.session, revision, "cancel", now); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SetCapacityRetry(t.Context(), env.session, revision, "schedule", now); err != nil {
		t.Fatal(err)
	}
	if got := terminalActivity(t, env); got.ErrorMessage != "" || got.CapacityRetryState != "cancelled" {
		t.Fatal(got)
	}
}

func TestModelCapacityClassificationExcludesOtherTerminalFailures(t *testing.T) {
	for _, message := range []string{"The model is at capacity.", "Model capacity rejected the prompt.", "API Error: 529 overloaded_error"} {
		if !isModelCapacityError("codex_protocol_error", message) {
			t.Fatal(message)
		}
	}
	for _, message := range []string{"The context window is at capacity.", "Quota exceeded.", "Rate limit reached.", "Insufficient credit balance.", "This content was flagged for cybersecurity risk.", "Network disconnected.", "Authentication failed."} {
		if isModelCapacityError("codex_protocol_error", message) {
			t.Fatal("unrelated error scheduled a prompt", message)
		}
	}
}
