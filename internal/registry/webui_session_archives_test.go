package registry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func archiveEventEnv(t *testing.T) eventTestEnv {
	t.Helper()
	e := newEventTestEnv(t)
	if err := e.store.IngestEvent(t.Context(), e.worker, e.connection, e.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.pool.Exec(t.Context(), `UPDATE workers SET heartbeat_metadata=json_set(heartbeat_metadata,'$.supports_session_editing',json('true'),'$.supports_session_archives',json('true')) WHERE worker_id=$1`, e.worker); err != nil {
		t.Fatal(err)
	}
	return e
}
func archiveResultEvent(t *testing.T, e eventTestEnv, seq uint64, sessionID, kind string, result protocol.Result) protocol.Event {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: e.worker.String(), RuntimeID: e.runtime.String(), RuntimeGeneration: 1, SessionID: sessionID, Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}
}
func archiveSessionSnapshot(e eventTestEnv) protocol.Session {
	return protocol.Session{ID: e.session.String(), WorkerID: e.worker.String(), RuntimeID: e.runtime.String(), ThreadID: "thread-1", Name: "Thread", CWD: "/work", State: "idle", UpdatedAt: time.Now().UTC()}
}
func archiveEntryFixture() protocol.SessionArchiveEntry {
	return protocol.SessionArchiveEntry{ID: "Thread_20261018.tar.xz", Name: "Thread_20261018.tar.xz", SessionName: "Thread", OriginalPath: "/work", CreatedAt: time.Now().UTC(), Bytes: 128, CodexVersion: "0.163.0"}
}

func TestWebUISessionEditDurableTargetAndNoTelegramMessages(t *testing.T) {
	e := archiveEventEnv(t)
	ctx := t.Context()
	id := uuid.New()
	other := uuid.New()
	insertRouteSession(t, e, other, "other-thread", "")
	in := telegramUpdate(e, 0)
	setTestSelection(t, e.store, in, other)
	result, err := e.store.QueueWebUISessionEdit(ctx, e.session, id, "  Project / review  ")
	if err != nil || !result.Pending {
		t.Fatalf("queue: %+v %v", result, err)
	}
	if _, err = e.store.QueueWebUISessionEdit(ctx, e.session, id, "Different"); err == nil {
		t.Fatal("same request changed title")
	}
	commands, err := e.store.PendingCommandsForWorker(ctx, e.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].SessionID != e.session.String() || commands[0].Arguments.SessionName != "Project / review" || commands[0].Arguments.CWD != "/work" {
		t.Fatalf("pinned rename: %+v %v", commands, err)
	}
	session := archiveSessionSnapshot(e)
	session.Name = "Project / review"
	event := archiveResultEvent(t, e, 2, e.session.String(), "command_completed", protocol.Result{CommandID: id.String(), Session: &session})
	if err = e.store.IngestEvent(ctx, e.worker, e.connection, event); err != nil {
		t.Fatal(err)
	}
	result, err = e.store.WebUISessionAction(ctx, uuid.Nil, e.session, id, protocol.RenameSession)
	if err != nil || result.Pending || result.Session == nil || result.Session.Name != session.Name || result.Session.CWD != "/work" {
		t.Fatalf("confirmed rename: %+v %v", result, err)
	}
	if _, err = e.store.QueueWebUISessionEdit(ctx, e.session, id, "Project / review"); err != nil {
		t.Fatal(err)
	}
	if selected := testSelectedSession(t, e.store, in); selected != other {
		t.Fatal("editing another session changed bot selection")
	}
	var deliveries int
	if err = e.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("rename sent unrelated messages: %d %v", deliveries, err)
	}
	if _, err = e.store.WebUISessionAction(ctx, uuid.Nil, other, id, protocol.RenameSession); !errors.Is(err, ErrWebUISessionActionNotFound) {
		t.Fatalf("cross-session status: %v", err)
	}
}

func TestWebUISessionArchiveProgressAndRestoreRemainPrivateAndResumable(t *testing.T) {
	e := archiveEventEnv(t)
	ctx := t.Context()
	archiveID := uuid.New()
	entry := archiveEntryFixture()
	if _, err := e.store.QueueWebUISessionArchive(ctx, e.session, uuid.New(), "/changed", true); err == nil {
		t.Fatal("stale editor could remove another project")
	}
	queued, err := e.store.QueueWebUISessionArchive(ctx, e.session, archiveID, "/work", true)
	if err != nil || !queued.Pending {
		t.Fatalf("queue: %+v %v", queued, err)
	}
	progress := protocol.Result{CommandID: archiveID.String(), SessionArchive: &protocol.SessionArchiveResult{Phase: "compressing", ArchiveDirectory: "/home/worker/CODEX_Archived"}}
	if err = e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 2, e.session.String(), "session_archive_progress", progress)); err != nil {
		t.Fatal(err)
	}
	status, err := e.store.WebUISessionAction(ctx, uuid.Nil, e.session, archiveID, protocol.ArchiveSession)
	if err != nil || !status.Pending || status.Phase != "compressing" {
		t.Fatalf("progress: %+v %v", status, err)
	}
	session := archiveSessionSnapshot(e)
	session.State, session.Archived = "not_loaded", true
	complete := protocol.Result{CommandID: archiveID.String(), Session: &session, SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: "/home/worker/CODEX_Archived", Archive: &entry}}
	if err = e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 3, e.session.String(), "command_completed", complete)); err != nil {
		t.Fatal(err)
	}
	status, err = e.store.QueueWebUISessionArchive(ctx, e.session, archiveID, "/work", true)
	if err != nil || status.Pending || status.Archive == nil || status.Session == nil || !status.Session.Archived {
		t.Fatalf("hidden-session recovery: %+v %v", status, err)
	}
	if _, err = e.store.QueueWebUISessionArchive(ctx, e.session, archiveID, "/work", false); err == nil {
		t.Fatal("retry changed destructive cleanup option")
	}
	target := WebUICreationTarget{WorkerID: e.worker, RuntimeID: e.runtime, RuntimeGeneration: 1, RequestID: uuid.New()}
	if _, err = e.store.QueueWebUIArchiveOperation(ctx, target, protocol.ListSessionArchives, protocol.SessionArchiveRequest{}); err != nil {
		t.Fatal(err)
	}
	listing := protocol.Result{CommandID: target.RequestID.String(), SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: "/home/worker/CODEX_Archived", Archives: []protocol.SessionArchiveEntry{entry}}}
	if err = e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 4, "", "command_completed", listing)); err != nil {
		t.Fatal(err)
	}
	listed, err := e.store.WebUISessionAction(ctx, e.worker, uuid.Nil, target.RequestID, protocol.ListSessionArchives)
	if err != nil || listed.Pending || len(listed.Archives) != 1 || listed.Archives[0].ID != entry.ID {
		t.Fatalf("listing: %+v %v", listed, err)
	}
	if _, err = e.store.WebUISessionAction(ctx, uuid.New(), uuid.Nil, target.RequestID, protocol.ListSessionArchives); !errors.Is(err, ErrWebUISessionActionNotFound) {
		t.Fatal("cross-worker listing exposed")
	}
	target.RequestID = uuid.New()
	if _, err = e.store.QueueWebUIArchiveOperation(ctx, target, protocol.RestoreSession, protocol.SessionArchiveRequest{ArchiveID: entry.ID}); err != nil {
		t.Fatal(err)
	}
	session.Archived, session.State = false, "idle"
	restored := protocol.Result{CommandID: target.RequestID.String(), Session: &session, SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", Archive: &entry, ArchiveDirectory: "/home/worker/CODEX_Archived"}}
	if err = e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 5, "", "command_completed", restored)); err != nil {
		t.Fatal(err)
	}
	result, err := e.store.WebUISessionAction(ctx, e.worker, uuid.Nil, target.RequestID, protocol.RestoreSession)
	if err != nil || result.Pending || result.Session == nil || result.Session.ID != e.session.String() || result.Session.Archived {
		t.Fatalf("restored original identity: %+v %v", result, err)
	}
	visible, err := e.store.WebUIInventorySnapshot(ctx)
	if err != nil || len(visible.Sessions) != 1 || visible.Sessions[0].ID != e.session.String() {
		t.Fatalf("restored inventory: %+v %v", visible, err)
	}
	var deliveries int
	if err = e.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("archive/list/restore leaked messages: %d %v", deliveries, err)
	}
}

func TestWebUISessionArchiveRejectsForgedResultsAndUnsafeTargets(t *testing.T) {
	e := archiveEventEnv(t)
	ctx := t.Context()
	id := uuid.New()
	if _, err := e.store.QueueWebUISessionArchive(ctx, e.session, id, "/work", false); err != nil {
		t.Fatal(err)
	}
	entry := archiveEntryFixture()
	session := archiveSessionSnapshot(e)
	session.ID = uuid.NewString()
	session.Archived = true
	session.State = "not_loaded"
	result := protocol.Result{CommandID: id.String(), Session: &session, SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", Archive: &entry}}
	if err := e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 2, e.session.String(), "command_completed", result)); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("forged target accepted: %v", err)
	}
	result.Session = nil
	result.SessionArchive.Archive = nil
	result.SessionArchive.Archives = []protocol.SessionArchiveEntry{entry}
	if err := e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 2, e.session.String(), "session_archive_progress", result)); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("directory leaked in progress: %v", err)
	}
	result.SessionArchive.Archives = nil
	result.SessionArchive.ProjectDeleted = true
	if err := e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 2, e.session.String(), "session_archive_progress", result)); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("cleanup completion leaked in progress: %v", err)
	}
	session = archiveSessionSnapshot(e)
	session.Archived, session.State = true, "not_loaded"
	result.Session, result.SessionArchive.Archive = &session, &entry
	if err := e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 2, e.session.String(), "command_completed", result)); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("unrequested project deletion accepted: %v", err)
	}
	if _, err := e.store.pool.Exec(ctx, `UPDATE commands SET status='failed',error_code='SECRET',error_message='SECRET /private/data' WHERE command_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	status, err := e.store.WebUISessionAction(ctx, uuid.Nil, e.session, id, protocol.ArchiveSession)
	raw, _ := json.Marshal(status)
	if err != nil || strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "/private") {
		t.Fatalf("raw worker error exposed: %s %v", raw, err)
	}
	for _, mutation := range []string{`UPDATE workers SET enabled=FALSE`, `UPDATE workers SET connectivity='offline'`, `UPDATE workers SET heartbeat_metadata='{}'`, `UPDATE sessions SET state='running',active_turn_id='busy'`} {
		t.Run(mutation, func(t *testing.T) {
			env := archiveEventEnv(t)
			if _, err := env.store.pool.Exec(t.Context(), mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.QueueWebUISessionEdit(t.Context(), env.session, uuid.New(), "New"); err == nil {
				t.Fatal("unsafe target accepted")
			}
		})
	}
}

func TestWebUISessionArchiveRecoveryCompletesOriginalGenerationWithoutStaleInventory(t *testing.T) {
	e := archiveEventEnv(t)
	ctx := t.Context()
	id := uuid.New()
	if _, err := e.store.QueueWebUISessionArchive(ctx, e.session, id, "/work", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.pool.Exec(ctx, `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, e.runtime); err != nil {
		t.Fatal(err)
	}
	session := archiveSessionSnapshot(e)
	session.Archived, session.State = true, "not_loaded"
	entry := archiveEntryFixture()
	result := protocol.Result{CommandID: id.String(), Session: &session, SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", Archive: &entry}}
	if err := e.store.IngestEvent(ctx, e.worker, e.connection, archiveResultEvent(t, e, 2, e.session.String(), "command_completed", result)); err != nil {
		t.Fatal(err)
	}
	status, err := e.store.WebUISessionAction(ctx, uuid.Nil, e.session, id, protocol.ArchiveSession)
	if err != nil || status.Pending || status.Status != "completed" || status.Archive == nil {
		t.Fatalf("recovered immutable command: %+v %v", status, err)
	}
	var archived bool
	if err := e.store.pool.QueryRow(ctx, `SELECT archived FROM sessions WHERE session_id=$1`, e.session).Scan(&archived); err != nil || archived {
		t.Fatalf("old-generation result changed live inventory: %t %v", archived, err)
	}
	raw, _ := json.Marshal(session)
	event := protocol.Event{ID: uuid.NewString(), Seq: 3, WorkerID: e.worker.String(), RuntimeID: e.runtime.String(), RuntimeGeneration: 2, SessionID: e.session.String(), Kind: "session_state_changed", OccurredAt: time.Now().UTC(), Data: raw}
	if err := e.store.IngestEvent(ctx, e.worker, e.connection, event); err != nil {
		t.Fatal(err)
	}
	if err := e.store.pool.QueryRow(ctx, `SELECT archived FROM sessions WHERE session_id=$1`, e.session).Scan(&archived); err != nil || !archived {
		t.Fatalf("fresh recovered state missing: %t %v", archived, err)
	}
}
