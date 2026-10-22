package worker

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func TestAgentStartupReconcilesPersistedRunningSession(t *testing.T) {
	a, previousRuntime, server, cleanup := testAgent(t)
	defer cleanup()
	// Seed the inventory left by a stopped worker, without creating an actor.
	// Keep native metadata unchanged so no later metadata refresh masks recovery.
	previous, err := a.store.UpsertRuntimeSession(previousRuntime, protocol.Session{
		RuntimeID: previousRuntime.ID, ThreadID: "rebooted-thread", Name: "Saved session",
		CWD: previousRuntime.DefaultCWD, Loaded: true, State: "running", ActiveTurnID: "previous-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := a.store.BeginRuntime(previousRuntime.ProfileID, previousRuntime.Name, previousRuntime.DefaultCWD)
	if err != nil {
		t.Fatal(err)
	}
	client, _, _ := a.manager.Client(previousRuntime.ID)
	runtime.State = "running"
	a.manager.install(runtime, client)
	server.SetThreads([]map[string]any{{"id": previous.ThreadID, "name": previous.Name, "cwd": previous.CWD, "status": "notLoaded"}}, nil)
	if err := a.manager.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	current := storedSettingsSession(t, a.store, previous.ID)
	if current.ID != previous.ID || current.Name != previous.Name || current.Loaded || current.ActiveTurnID != "" || current.State != "not_loaded" {
		t.Fatalf("startup kept previous process activity: %+v", current)
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("fresh inventory was not published exactly once: %+v, %v", events, err)
	}
	var snapshot protocol.Session
	if event := events[0]; event.Kind != "session_state_changed" || event.RuntimeGeneration != runtime.Generation ||
		json.Unmarshal(event.Data, &snapshot) != nil || snapshot.ID != previous.ID || snapshot.ActiveTurnID != "" || snapshot.Loaded || snapshot.State != "not_loaded" {
		t.Fatalf("incorrect startup event: %+v, %s", event, event.Data)
	}
	if hasCall(server.Calls(), "thread/resume") {
		t.Fatal("startup loaded a cold thread")
	}
	if err := a.manager.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if events, err := a.store.OutboxAfter(0); err != nil || len(events) != 1 {
		t.Fatalf("unchanged discovery republished inventory: %+v, %v", events, err)
	}
	// Immutable steer targets remain fenced even after recovery.
	stale := agentCommand(runtime, previous, protocol.Steer)
	if ack, err := a.HandleCommand(context.Background(), stale); err != nil || ack.Error == nil || ack.Error.Code != protocol.StaleTurn {
		t.Fatalf("stale turn target was accepted: %+v, %v", ack, err)
	}
	if hasCall(server.Calls(), "turn/steer") {
		t.Fatal("stale steering reached Codex")
	}
	command := agentCommand(runtime, current, protocol.StartTurn)
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("fresh prompt was not accepted: %+v, %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	current = storedSettingsSession(t, a.store, previous.ID)
	if current.ActiveTurnID == "" || current.State != "running" || !current.Loaded || !hasCall(server.Calls(), "thread/resume") || !hasCall(server.Calls(), "turn/start") {
		t.Fatalf("fresh prompt did not resume and start normally: %+v", current)
	}
}

func TestAgentStartupKeepsNativeActiveTurnAndPreferences(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	previous, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{
		RuntimeID: runtime.ID, ThreadID: "loaded-recovery", Name: "Confirmed title", CWD: runtime.DefaultCWD,
		State: "running", Loaded: true, ActiveTurnID: "old-turn",
		Settings: &protocol.SessionSettings{RuntimeGeneration: runtime.Generation, Revision: 2, Model: "current-model", ReasoningEffort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreads([]map[string]any{{"id": previous.ThreadID, "cwd": previous.CWD, "status": "active",
		"turns": []map[string]any{{"id": "native-turn", "status": "inProgress"}},
	}}, []string{previous.ThreadID})
	client, _, _ := a.manager.Client(runtime.ID)
	if err := a.manager.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	current := storedSettingsSession(t, a.store, previous.ID)
	if current.ActiveTurnID != "native-turn" || current.State != "running" || !current.Loaded || current.Name != previous.Name || !reflect.DeepEqual(current.Settings, previous.Settings) {
		t.Fatalf("startup lost native activity or confirmed metadata: %+v", current)
	}
	// Existing actors retain their live turn when an older idle discovery arrives.
	older := current
	older.ActiveTurnID, older.State = "", "idle"
	older.Settings = nil
	a.onSession(runtime, older)
	current = storedSettingsSession(t, a.store, previous.ID)
	if current.ActiveTurnID != "native-turn" || current.State != "running" || !reflect.DeepEqual(current.Settings, previous.Settings) {
		t.Fatalf("older discovery replaced active actor state: %+v", current)
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "session_state_changed" {
		t.Fatalf("startup active snapshot publication: %+v, %v", events, err)
	}
	if hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/interrupt") {
		t.Fatal("reconciliation changed native activity")
	}
}

func TestReconcileRuntimeSessionSkipsUnchangedInventory(t *testing.T) {
	store, runtime, original := inventoryStoreFixture(t)
	for range 2 {
		saved, err := store.ReconcileRuntimeSession(runtime, original)
		if err != nil || !reflect.DeepEqual(saved, original) {
			t.Fatalf("unchanged inventory was rewritten: %+v, %v", saved, err)
		}
	}
	if events, err := store.OutboxAfter(0); err != nil || len(events) != 0 {
		t.Fatalf("unchanged inventory emitted duplicate events: %+v, %v", events, err)
	}
}

func TestAgentStartupReconciliationFailureKeepsInventoryAndActorUnavailable(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	original, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{
		RuntimeID: runtime.ID, ThreadID: "failed-recovery", CWD: runtime.DefaultCWD,
		State: "running", Loaded: true, ActiveTurnID: "old-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.Update(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(math.MaxInt64))
	}); err != nil {
		t.Fatal(err)
	}
	fresh := original
	fresh.ActiveTurnID, fresh.State, fresh.Loaded = "", "not_loaded", false
	a.onSession(runtime, fresh)
	if actor := a.actorForThread(runtime.ID, fresh.ThreadID); actor != nil {
		t.Fatal("actor became available before durable inventory publication")
	}
	if current := storedSettingsSession(t, a.store, original.ID); !reflect.DeepEqual(current, original) {
		t.Fatalf("failed publication did not roll back inventory: %+v", current)
	}
	if events, err := a.store.OutboxAfter(0); err != nil || len(events) != 0 {
		t.Fatalf("failed publication left events: %+v, %v", events, err)
	}
	select {
	case err := <-a.fatal:
		if err == nil {
			t.Fatal("persistence failure was not reported")
		}
	default:
		t.Fatal("initial discovery discarded its persistence failure")
	}
}

func TestReconcileRuntimeSessionClearsIdleActiveSince(t *testing.T) {
	store, runtime, original := inventoryStoreFixture(t)
	started := time.Now().UTC().Add(-time.Minute)
	original.Stats = &protocol.SessionStats{ActiveSince: &started}
	var err error
	original, err = store.UpsertRuntimeSession(runtime, original)
	if err != nil {
		t.Fatal(err)
	}
	fresh := original
	fresh.ActiveTurnID, fresh.State = "", "idle"
	saved, err := store.ReconcileRuntimeSession(runtime, fresh)
	if err != nil || saved.Stats == nil || saved.Stats.ActiveSince != nil {
		t.Fatalf("idle recovery retained turn activity time: %+v, %v", saved.Stats, err)
	}
	if fresh.Stats.ActiveSince == nil {
		t.Fatal("reconciliation mutated the incoming statistics")
	}
}

func TestAgentShutdownDoesNotReportCancelledSessionSnapshot(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(a.ctx)
	cancel()
	a.ctx = ctx
	a.onSession(runtime, protocol.Session{})
	select {
	case err := <-a.fatal:
		t.Fatalf("normal shutdown reported a fatal snapshot error: %v", err)
	default:
	}
}
