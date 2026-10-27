package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Exercise the actual worker call sites and JSONL transport: canceling a
// browser or discovery must not turn its eventual observation acknowledgment
// into either a permanent update blocker or an acknowledged browser snapshot.
func cancelWorkerObservationResume(t *testing.T, a *Agent, runtime protocol.Runtime, server *codextest.Server, source string) (*codexadapter.Client, protocol.Session) {
	t.Helper()
	client := mustClient(t, a, runtime)
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "canceled-observer", CWD: runtime.DefaultCWD, State: "idle", Loaded: true})
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, []string{session.ThreadID})
	server.SetResponseDelay("thread/resume", 250*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if source == "discovery" {
			done <- a.manager.discover(ctx, runtime, client)
		} else {
			done <- a.observeWebUISession(ctx, runtime, session)
		}
	}()
	waitFor(t, func() bool { return hasCall(server.Calls(), "thread/resume") })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled %s observer = %v", source, err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled observation did not stop")
	}
	waitFor(t, func() bool {
		status := client.UpdateSafety()
		return status.UnconfirmedRequests == 1 && status.PendingRequests == 0
	})
	if ready, _ := a.manager.webUIObserverState(runtime, client, session.ThreadID); ready {
		t.Fatal("canceled observation enabled browser input")
	}
	// An idle-looking runtime cannot substitute for the missing response.
	before := countCall(server.Calls(), "thread/loaded/list")
	if lease, err := a.prepareUpdate(t.Context(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("unacknowledged observation allowed update")
	} else if !strings.Contains(err.Error(), "unconfirmed mutating requests") {
		t.Fatalf("unacknowledged observation refusal = %v", err)
	}
	if countCall(server.Calls(), "thread/loaded/list") != before {
		t.Fatal("update attempted to replace a missing acknowledgment with an idle snapshot")
	}
	return client, session
}

func TestUpdatePreparationRecoversAcknowledgedCanceledWorkerObservation(t *testing.T) {
	for _, source := range []string{"browser", "discovery"} {
		for _, state := range []string{"idle", "active", "verification_error"} {
			t.Run(source+"/"+state, func(t *testing.T) {
				a, runtime, server, cleanup := testAgent(t)
				defer cleanup()
				client, session := cancelWorkerObservationResume(t, a, runtime, server, source)
				waitFor(t, client.UpdateQuiescent)
				if ready, _ := a.manager.webUIObserverState(runtime, client, session.ThreadID); ready {
					t.Fatal("late acknowledgment installed the canceled observer snapshot")
				}
				if state == "active" {
					server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "active", "turns": []map[string]any{{"id": "fresh-turn", "status": "inProgress"}}}}, []string{session.ThreadID})
				} else if state == "verification_error" {
					server.SetRPCError("thread/loaded/list", -32000, "snapshot unavailable")
				}
				before := countCall(server.Calls(), "thread/loaded/list")
				lease, err := a.prepareUpdate(t.Context(), time.Minute)
				if err == nil {
					defer a.abortUpdate(lease.Token)
				}
				if countCall(server.Calls(), "thread/loaded/list") <= before {
					t.Fatal("late acknowledgment bypassed fresh runtime verification")
				}
				if state == "idle" && err != nil {
					t.Fatalf("acknowledged canceled observation blocked idle update: %v", err)
				}
				if state != "idle" && err == nil {
					t.Fatal("observation recovery bypassed failed fresh idle verification")
				}
			})
		}
	}
}

func TestDiscoveryDoesNotRepeatUnacknowledgedObservationResume(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	client, session := cancelWorkerObservationResume(t, a, runtime, server, "discovery")
	before := countCall(server.Calls(), "thread/resume")
	if _, subscribed, err := a.manager.subscribeLoadedThread(t.Context(), runtime, client, session.ThreadID, false); !errors.Is(err, codexadapter.ErrObservationResumePending) || subscribed {
		t.Fatalf("unacknowledged observation retry = subscribed %v, %v", subscribed, err)
	}
	if countCall(server.Calls(), "thread/resume") != before {
		t.Fatal("discovery sent overlapping observation resumes")
	}
	waitFor(t, client.UpdateQuiescent)
	if ready, _ := a.manager.webUIObserverState(runtime, client, session.ThreadID); ready {
		t.Fatal("late acknowledgment marked an uninstalled observer ready")
	}
	server.SetResponseDelay("thread/resume", 0)
	if err := a.manager.discover(t.Context(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if ready, _ := a.manager.webUIObserverState(runtime, client, session.ThreadID); !ready {
		t.Fatal("discovery could not install a freshly acknowledged observer")
	}
	if countCall(server.Calls(), "thread/resume") != before+1 {
		t.Fatal("discovery did not retry after the previous acknowledgment settled")
	}
}

func TestObservationRecoveryPreservesWorkerUpdateBlockers(t *testing.T) {
	for _, blocker := range []string{"native_queue", "queued_command", "goal_mutation", "approval"} {
		t.Run(blocker, func(t *testing.T) {
			a, server, proxy, native, backend, _ := newUpdateRecoveryTest(t)
			runtime := a.manager.Snapshot()[0]
			client, session := cancelWorkerObservationResume(t, a, runtime, server, "browser")
			waitFor(t, client.UpdateQuiescent)
			want := ""
			switch blocker {
			case "native_queue":
				nativeWrite(t, backend, `{"method":"thread/queue/changed","params":{"threadId":"canceled-observer"}}`)
				nativeRecoveryRead(t, proxy, native)
				if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{map[string]any{"id": "queued", "text": "pending"}}, "nextCursor": nil}); err != nil {
					t.Fatal(err)
				}
				want = "queued work"
			case "queued_command":
				if _, err := a.store.Receive(agentCommand(runtime, session, protocol.StartTurn)); err != nil {
					t.Fatal(err)
				}
				want = "commands are queued or executing"
			case "goal_mutation":
				server.SetResponseDelay("thread/goal/set", 100*time.Millisecond)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- client.SetGoal(ctx, session.ThreadID, "a goal", "active") }()
				waitFor(t, func() bool { return hasCall(server.Calls(), "thread/goal/set") })
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled goal mutation = %v", err)
				}
				// The fixture processes requests in order: this read proves the
				// goal's late success arrived without trusting it to settle work.
				if _, err := client.LoadedThreads(t.Context(), "", 1); err != nil {
					t.Fatal(err)
				}
				want = "unconfirmed mutating requests"
			case "approval":
				if err := server.Request("item/commandExecution/requestApproval", "approval", map[string]any{"threadId": session.ThreadID}); err != nil {
					t.Fatal(err)
				}
				waitFor(t, func() bool { return client.UpdateSafety().PendingApprovals == 1 })
				want = "approvals or input"
			}
			if lease, err := a.prepareUpdate(t.Context(), time.Minute); err == nil {
				_ = a.abortUpdate(lease.Token)
				t.Fatalf("observation recovery bypassed %s", blocker)
			} else if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s refusal = %v; want %s", blocker, err, want)
			}
		})
	}
}
