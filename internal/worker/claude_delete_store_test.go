package worker

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func TestClaudeDeletionPurgesPayloadsAndPreservesImmutableDelivery(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind = "claude"
	const content = "unique-private-deleted-conversation"
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, Name: content, Preview: content, State: "idle", Stats: &protocol.SessionStats{}})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, Name: "sibling", State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.StartTurn)
	command.Arguments.Text = content
	if _, err := a.store.Receive(command); err != nil {
		t.Fatal(err)
	}
	result := &protocol.Result{CommandID: command.ID, Text: content, State: "completed"}
	if _, err := a.record(command, CommandCompleted, result, "command_completed"); err != nil {
		t.Fatal(err)
	}
	creation := agentCommand(runtime, session, protocol.NewSession)
	creation.SessionID, creation.ThreadID = "", ""
	creation.Arguments = protocol.Arguments{SessionName: content, CWD: session.CWD}
	if _, err := a.store.Receive(creation); err != nil {
		t.Fatal(err)
	}
	if _, err := a.record(creation, CommandCompleted, &protocol.Result{CommandID: creation.ID, State: "completed", Session: &session, Text: content}, "command_completed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store.announceAsyncQuestion(runtime, session.ID, protocol.Approval{RequestID: "private-question", Summary: content}); err != nil {
		t.Fatal(err)
	}
	prefix := asyncHistoryPrefix(runtime.ID, session.ThreadID)
	if err := a.store.saveAsyncHistoryPage(prefix, "cursor", codexadapter.HistoryTurnPage{Turns: []codexadapter.HistoryTurn{{ID: content}}}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.Update(func(tx *workerdb.Tx) error {
		for key, target := range map[string]protocol.Session{"selected-upload": session, "sibling-upload": sibling} {
			data, _ := json.Marshal(webUIUploadRecord{Scope: webUIUploadScope{RuntimeID: runtime.ID, SessionID: target.ID, ThreadID: target.ThreadID, CWD: target.CWD}, Name: content})
			if err := tx.Bucket(bucketWebUIUploads).Put([]byte(key), data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := a.store.OutboxAfter(0)
	if len(before) != 3 {
		t.Fatal("fixture missing immutable queued events")
	}
	_, err = a.webHistory.insert(webUIHistoryScope{Runtime: runtime.ID, Thread: session.ThreadID, Generation: runtime.Generation}, codexadapter.TranscriptTurn{ID: "turn", Items: []json.RawMessage{json.RawMessage(`{"text":"` + content + `"}`)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := a.deleteStoredSession(runtime, session)
	if err != nil || !saved.Deleted || saved.CWD != "" || saved.Name != "" || saved.Preview != "" || saved.Stats != nil || saved.Settings != nil {
		t.Fatalf("not a content-free tombstone: %#v %v", saved, err)
	}
	a.webHistory.mu.Lock()
	if len(a.webHistory.snapshots) != 0 || a.webHistory.bytes != 0 {
		t.Error("deleted history still cached")
	}
	a.webHistory.mu.Unlock()
	after, _ := a.store.OutboxAfter(0)
	if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("deletion rewrote immutable events")
	}
	for _, id := range []string{command.ID, creation.ID} {
		record, found, err := a.store.LoadCommand(id)
		encoded, _ := json.Marshal(record)
		if err != nil || !found || strings.Contains(string(encoded), content) || strings.Contains(string(encoded), session.CWD) || record.State != CommandCompleted {
			t.Fatalf("conversation retained in receipt: %s %v", encoded, err)
		}
		if id == creation.ID && (record.Command.SessionID != "" || record.Command.ThreadID != "") {
			t.Fatal("purge changed immutable creation target")
		}
	}
	// A creation result may arrive after the actor has already deleted its
	// newly discovered session. The incoming result must honor the tombstone
	// while the creation command keeps its original empty target.
	if _, err := a.record(creation, CommandCompleted, &protocol.Result{CommandID: creation.ID, State: "completed", Session: &session, Text: content}, "command_completed"); err != nil {
		t.Fatal(err)
	}
	creationRecord, _, err := a.store.LoadCommand(creation.ID)
	encodedCreation, _ := json.Marshal(creationRecord)
	if err != nil || strings.Contains(string(encodedCreation), content) || strings.Contains(string(encodedCreation), session.CWD) {
		t.Fatal("late creation result restored deleted metadata", err)
	}
	questions, err := a.store.asyncQuestionRecords(session.ID)
	if err != nil || len(questions) != 0 {
		t.Fatal("deleted questions retained", err)
	}
	if _, found, err := a.store.asyncHistoryCachedPage(prefix, "cursor"); err != nil || found {
		t.Fatal("deleted question history retained", err)
	}
	if err := a.store.db.View(func(tx *workerdb.Tx) error {
		if tx.Bucket(bucketWebUIUploads).Get([]byte("selected-upload")) != nil || tx.Bucket(bucketWebUIUploads).Get([]byte("sibling-upload")) == nil {
			t.Error("upload metadata purge affected wrong session")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Late producers and new rejected commands cannot reintroduce payloads.
	late, err := a.store.AppendEvent(protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: session.ID, Kind: "agent_message", Data: json.RawMessage(`{"text":"` + content + `"}`)})
	if err != nil || late.Kind != "session_state_changed" || strings.Contains(string(late.Data), content) {
		t.Fatal("late event retained conversation", err)
	}
	lateCommand := agentCommand(runtime, session, protocol.StartTurn)
	lateCommand.Arguments.Text = content
	if _, err := a.store.Receive(lateCommand); err != nil {
		t.Fatal(err)
	}
	if _, err := a.record(lateCommand, CommandFailed, &protocol.Result{CommandID: lateCommand.ID, State: "failed", Text: content}, "command_failed"); err != nil {
		t.Fatal(err)
	}
	lateRecord, _, _ := a.store.LoadCommand(lateCommand.ID)
	encoded, _ := json.Marshal(lateRecord)
	if strings.Contains(string(encoded), content) {
		t.Fatal("late command restored payload")
	}
	_, high, _ := a.store.EventWatermarks()
	if err := a.store.AckThrough(high); err != nil {
		t.Fatal(err)
	}
	remaining, _ := a.store.OutboxAfter(0)
	if len(remaining) != 0 {
		t.Fatal("acknowledged payload remains queued")
	}
	stale := session
	stale.Name, stale.Archived, stale.Deleted = content, false, false
	retired, err := a.store.UpsertRuntimeSession(runtime, stale)
	if err != nil || !retired.Deleted || retired.Name != "" {
		t.Fatal("stale discovery resurrected deleted session", err)
	}
}

func TestClaudeDeletionPurgeRollsBackWithOutboxFailure(t *testing.T) {
	actor, _, _, _ := deletionFixture(t)
	actor.agent.cfg.AgentKind = "claude"
	before := actor.session
	if err := actor.agent.store.db.Update(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(math.MaxInt64))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.agent.deleteStoredSession(actor.runtime, before); err == nil {
		t.Fatal("purge unexpectedly committed without deletion event")
	}
	sessions, _ := actor.agent.store.ListSessions(actor.runtime.ID)
	if len(sessions) != 1 || sessions[0].Deleted || sessions[0].CWD != before.CWD {
		t.Fatal("failed transaction partially purged session")
	}
}

func TestClaudeDeletionIntentSurvivesRestartWithoutOrdinaryReplay(t *testing.T) {
	path, workerID := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.BeginRuntime("claude", "Claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, Name: "private name"})
	if err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.DeleteSession)
	command.Arguments = protocol.Arguments{}
	if _, err := store.Receive(command); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCommandState(command.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.beginClaudeDeletion(runtime, session, command); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, found, err := store.LoadCommand(command.ID)
	if err != nil || !found || record.State != CommandExecuting || record.Result != nil {
		t.Fatal("safe deletion incorrectly became outcome_unknown", err)
	}
	intents, err := store.claudeDeletionIntents()
	if err != nil || len(intents) != 1 || intents[0].SessionID != session.ID {
		t.Fatal("deletion recovery intent lost", err)
	}
	encoded, _ := json.Marshal(intents)
	if strings.Contains(string(encoded), session.CWD) || strings.Contains(string(encoded), session.Name) {
		t.Fatal("deletion intent contains conversation metadata")
	}
}

func TestClaudeDeletionRecoversNativeCommitBeforeWorkerCommit(t *testing.T) {
	nativeRoot := t.TempDir()
	t.Setenv("HOME", nativeRoot)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(nativeRoot, ".claude"))
	a, bridge := claudeSettingsAgent(t)
	runtime, err := a.store.BeginRuntime("primary", "Claude", a.cfg.AllowedWorkspaceRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	runtime.State = "running"
	a.manager.install(runtime, bridge.Client)
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: runtime.DefaultCWD})
	if err != nil {
		t.Fatal(err)
	}
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: thread.ID, CWD: thread.CWD, Name: "private recovery session", State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.DeleteSession)
	command.Arguments = protocol.Arguments{CWD: session.CWD}
	if _, err := a.store.Receive(command); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetCommandState(command.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.store.beginClaudeDeletion(runtime, session, command); err != nil {
		t.Fatal(err)
	}
	if err := bridge.DeleteThread(t.Context(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.ReadThread(t.Context(), thread.ID, false); err == nil {
		t.Fatal("native commit did not remove metadata")
	}
	// Reopen only the durable ledger, simulating a crash before the worker
	// transaction. The native tombstone is the authoritative idempotency proof.
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(a.cfg.StateFile, a.cfg.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	a.store, a.manager.store = reopened, reopened
	next, err := reopened.BeginRuntime("primary", "Claude", runtime.DefaultCWD)
	if err != nil {
		t.Fatal(err)
	}
	next.State = "running"
	a.manager.install(next, bridge.Client)
	if err := a.recoverClaudeDeletions(); err != nil {
		t.Fatal(err)
	}
	record, found, err := reopened.LoadCommand(command.ID)
	if err != nil || !found || record.State != CommandCompleted || record.Result == nil || record.Result.Session == nil || !record.Result.Session.Deleted || record.Result.Session.CWD != "" {
		t.Fatalf("native commit not recovered: %#v %v", record, err)
	}
	if record.Command.RuntimeGeneration != runtime.Generation {
		t.Fatal("recovery changed immutable command generation")
	}
	if pending, err := reopened.claudeDeletionPending(runtime.ID, thread.ID); err != nil || pending {
		t.Fatal("completed deletion still fenced", err)
	}
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "duplicate" {
		t.Fatal("recovered receipt was not idempotent", err)
	}
	if err := a.recoverClaudeDeletions(); err != nil {
		t.Fatal("repeat startup recovery", err)
	}
}

func TestClaudeDeletionBusyRefusalClearsIntentAndCleanupFailureRetainsFence(t *testing.T) {
	for _, code := range []int{-32602, -32010} {
		t.Run(map[int]string{-32602: "busy refusal", -32010: "incomplete native cleanup"}[code], func(t *testing.T) {
			actor, client, server, command := deletionFixture(t)
			actor.agent.cfg.AgentKind = "claude"
			command.ID = "" // Direct actor call follows the browser command path.
			if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}}); err != nil {
				t.Fatal(err)
			}
			server.SetRPCError("thread/delete", code, "fixture native refusal")
			if _, err := actor.deleteSession(t.Context(), client, command); err == nil {
				t.Fatal("native failure ignored")
			}
			pending, err := actor.agent.store.claudeDeletionPending(actor.runtime.ID, actor.session.ThreadID)
			if err != nil || pending != (code == -32010) {
				t.Fatalf("incorrect deletion intent after %d: %v %v", code, pending, err)
			}
			if code == -32010 {
				if err := actor.agent.recoverClaudeDeletions(); err != nil {
					t.Fatal("one failed deletion stopped runtime startup", err)
				}
				start := agentCommand(actor.runtime, actor.session, protocol.StartTurn)
				ack, err := actor.agent.HandleCommand(t.Context(), start)
				if err != nil || ack.Error == nil || ack.Error.Code != protocol.SessionBusy {
					t.Fatal("pending deletion admitted new work", err)
				}
				// Retry does not require removed native metadata. The exact
				// persisted intent authorizes only the idempotent deletion RPC.
				server.SetRPCError("thread/read", -32602, "missing after partial cleanup")
				server.SetRPCError("thread/delete", 0, "")
				if _, err := actor.deleteSession(t.Context(), client, command); err != nil {
					t.Fatal("pending cleanup could not be retried", err)
				}
			}
		})
	}
}

func TestClaudeDeletionRejectsAdapterQueueAndPendingQuestions(t *testing.T) {
	for _, pendingQuestion := range []bool{false, true} {
		t.Run(map[bool]string{false: "native queued input", true: "pending async question"}[pendingQuestion], func(t *testing.T) {
			actor, client, server, command := deletionFixture(t)
			actor.agent.cfg.AgentKind = "claude"
			if pendingQuestion {
				actor.asyncQuestions = map[string]protocol.Approval{"pending": {State: "pending"}}
			} else if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{map[string]any{"id": "queued"}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := actor.deleteSession(t.Context(), client, command); err == nil || hasCall(server.Calls(), "thread/delete") {
				t.Fatal("pending native work was deleted", err)
			}
			if pending, err := actor.agent.store.claudeDeletionPending(actor.runtime.ID, actor.session.ThreadID); err != nil || pending {
				t.Fatal("busy session left a deletion intent", err)
			}
			if _, err := os.Stat(actor.session.CWD); err != nil {
				t.Fatal("workspace changed", err)
			}
		})
	}
}

func TestClaudePendingDeletionRemainsRetryableAfterDiscovery(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.AgentKind, a.manager.cfg.AgentKind = "claude", "claude"
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, Name: "retry cleanup", State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.DeleteSession)
	command.Arguments = protocol.Arguments{}
	if _, err := a.store.Receive(command); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetCommandState(command.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.store.beginClaudeDeletion(runtime, session, command); err != nil {
		t.Fatal(err)
	}
	server.SetThreads(nil, nil) // Pending native deletions are intentionally hidden.
	server.SetRPCError("thread/delete", -32010, "fixture cleanup permissions blocked")
	client, _, _ := a.manager.Client(runtime.ID)
	if err := a.manager.discover(t.Context(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverClaudeDeletions(); err != nil {
		t.Fatal("one cleanup failure stopped startup", err)
	}
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].Archived || sessions[0].Deleted || a.actorForThread(runtime.ID, session.ThreadID) == nil {
		t.Fatal("pending deletion lost its retry target", err)
	}
	start := agentCommand(runtime, session, protocol.StartTurn)
	ack, err := a.HandleCommand(t.Context(), start)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.SessionBusy {
		t.Fatal("pending native deletion accepted new work", err)
	}
	create := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	create.Arguments = protocol.Arguments{CWD: runtime.DefaultCWD, SessionName: "unrelated session"}
	if ack, err := a.HandleCommand(t.Context(), create); err != nil || ack.Status != "accepted" {
		t.Fatal("unrelated session creation blocked", err)
	}
	waitFor(t, func() bool {
		record, found, _ := a.store.LoadCommand(create.ID)
		return found && record.State == CommandCompleted
	})
	// A replacement confirmation must preserve the interrupted attempt's
	// receipt too, otherwise that old executing record blocks maintenance.
	server.SetRPCError("thread/delete", -32602, "fixture busy refusal on retry")
	refused := agentCommand(runtime, session, protocol.DeleteSession)
	refused.Arguments = protocol.Arguments{CWD: session.CWD}
	if ack, err := a.HandleCommand(t.Context(), refused); err != nil || ack.Status != "accepted" {
		t.Fatal("refused retry was not dispatched", err)
	}
	waitFor(t, func() bool {
		record, found, _ := a.store.LoadCommand(refused.ID)
		return found && record.State == CommandFailed
	})
	if pending, err := a.store.claudeDeletionPending(runtime.ID, session.ThreadID); err != nil || !pending {
		t.Fatal("retry refusal discarded older uncertain intent", err)
	}
	server.SetRPCError("thread/delete", 0, "")
	retry := agentCommand(runtime, session, protocol.DeleteSession)
	retry.Arguments = protocol.Arguments{CWD: session.CWD}
	if ack, err := a.HandleCommand(t.Context(), retry); err != nil || ack.Status != "accepted" {
		t.Fatal("cleanup retry rejected", err)
	}
	waitFor(t, func() bool {
		first, foundFirst, _ := a.store.LoadCommand(command.ID)
		second, foundSecond, _ := a.store.LoadCommand(retry.ID)
		pending, _ := a.store.claudeDeletionPending(runtime.ID, session.ThreadID)
		return foundFirst && foundSecond && first.State == CommandCompleted && second.State == CommandCompleted && !pending
	})
	if pending, err := a.store.claudeDeletionPending(runtime.ID, session.ThreadID); err != nil || pending {
		t.Fatal("successful retry retained deletion fence", err)
	}
	rejected, _, _ := a.store.LoadCommand(start.ID)
	if rejected.State != CommandFailed {
		t.Fatal("deletion incorrectly completed an unrelated rejected command")
	}
	refusedRecord, _, _ := a.store.LoadCommand(refused.ID)
	if refusedRecord.State != CommandFailed {
		t.Fatal("cleanup incorrectly completed a natively refused retry")
	}
	_, high, err := a.store.EventWatermarks()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.AckThrough(high); err != nil {
		t.Fatal(err)
	}
	if err := a.store.checkUpdateIdle(); err != nil {
		t.Fatal("superseded deletion receipt blocks maintenance", err)
	}
}

func TestClaudePendingDeletionFencesCommandsAdmittedBeforeIntent(t *testing.T) {
	for _, operation := range []protocol.Operation{protocol.StoreFiles, protocol.StartTurn} {
		t.Run(string(operation), func(t *testing.T) {
			actor, _, _, deletion := deletionFixture(t)
			actor.agent.cfg.AgentKind = "claude"
			command := agentCommand(actor.runtime, actor.session, operation)
			command.Arguments = protocol.Arguments{CWD: actor.session.CWD, Text: "late prompt", Files: []protocol.File{{Name: "late.txt", Data: []byte("late attachment")}}}
			if operation == protocol.StoreFiles {
				command.Arguments.Text = ""
			}
			// Receive/dispatch happened while deletion was still doing its
			// native prechecks. Actor execution happens after intent commit.
			if _, err := actor.agent.store.Receive(command); err != nil {
				t.Fatal(err)
			}
			if err := actor.agent.store.beginClaudeDeletion(actor.runtime, actor.session, deletion); err != nil {
				t.Fatal(err)
			}
			reply := make(chan commandReply, 1)
			actor.command(actorCommand{command: command, reply: reply})
			result := <-reply
			if result.err != nil || result.ack.Error == nil || result.ack.Error.Code != protocol.SessionBusy {
				t.Fatal("late actor command bypassed deletion fence", result.err)
			}
			if len(actor.queue) != 0 {
				t.Fatal("late command entered turn queue")
			}
			if _, err := os.Stat(filepath.Join(actor.session.CWD, "inbound")); !os.IsNotExist(err) {
				t.Fatal("fenced command wrote workspace attachment", err)
			}
			record, _, err := actor.agent.store.LoadCommand(command.ID)
			if err != nil || record.State != CommandFailed || record.InboundFile != nil {
				t.Fatal("fenced command retained published file state", err)
			}
		})
	}
}
