package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/sessionarchive"
)

func archiveJobFixture(t *testing.T) (*Agent, protocol.Runtime, *codextest.Server, protocol.Session, *sessionarchive.Manifest) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	a, runtime, _, cleanup := testAgent(t)
	t.Cleanup(cleanup)
	client, server, err := codextest.New(a.ctx, codexadapter.InitializeInfo{CodexHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	a.manager.install(runtime, client)
	project := filepath.Join(runtime.DefaultCWD, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), Name: "Project session", CWD: project, State: "not_loaded"})
	if err != nil {
		t.Fatal(err)
	}
	a.onSession(runtime, session)
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": project, "status": "notLoaded"}}, nil)
	if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}}); err != nil {
		t.Fatal(err)
	}
	if err := server.SetMethodResult("thread/backgroundTerminals/list", map[string]any{"data": []any{}}); err != nil {
		t.Fatal(err)
	}
	manifest := &sessionarchive.Manifest{ThreadID: session.ThreadID, ThreadIDs: []string{session.ThreadID}, DisplayName: session.Name, OriginalPath: project, CodexVersion: "0.163.0", CreatedAt: time.Now().UTC(), Files: []sessionarchive.Entry{{Path: "project", Kind: "directory", Mode: 0700}}}
	a.archiveOperations = &sessionArchiveOperations{
		compatible: func(string) error { return nil },
		export: func(_ context.Context, options sessionarchive.ExportOptions) (sessionarchive.Manifest, error) {
			manifest.ArchiveID = options.ArchiveID
			if err := os.MkdirAll(options.ArchiveDir, 0700); err != nil {
				return sessionarchive.Manifest{}, err
			}
			if err := os.WriteFile(filepath.Join(options.ArchiveDir, options.ArchiveID), []byte("verified test archive"), 0600); err != nil {
				return sessionarchive.Manifest{}, err
			}
			return *manifest, nil
		},
		read: func(context.Context, string) (sessionarchive.Manifest, error) { return *manifest, nil },
		verify: func(context.Context, string, sessionarchive.VerifyOptions) (sessionarchive.Manifest, error) {
			result := *manifest
			result.VerifiedSelfContained = true
			return result, nil
		},
		restore: func(context.Context, sessionarchive.RestoreOptions) (sessionarchive.Manifest, error) {
			return *manifest, nil
		},
	}
	return a, runtime, server, session, manifest
}

func waitArchiveTerminal(t *testing.T, a *Agent, id string) CommandRecord {
	t.Helper()
	var result CommandRecord
	deadline := time.After(10 * time.Second)
	for {
		record, found, err := a.store.LoadCommand(id)
		result = record
		if err == nil && found && record.State != CommandReceived && record.State != CommandExecuting {
			return result
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatalf("archive command did not reach a terminal state: %s", result.State)
		}
	}
}

func TestArchiveJobAllowsUnrelatedActiveProjectAndFencesTarget(t *testing.T) {
	a, runtime, server, session, _ := archiveJobFixture(t)
	otherPath := filepath.Join(runtime.DefaultCWD, "other")
	if err := os.Mkdir(otherPath, 0700); err != nil {
		t.Fatal(err)
	}
	other, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: otherPath, State: "running", ActiveTurnID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	a.onSession(runtime, other)
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}, {"id": other.ThreadID, "cwd": otherPath, "status": "active", "turns": []map[string]any{{"id": "live", "status": "inProgress"}}}}, []string{other.ThreadID})
	entered, release := make(chan struct{}), make(chan struct{})
	verify := a.archiveOperations.verify
	a.archiveOperations.verify = func(ctx context.Context, path string, options sessionarchive.VerifyOptions) (sessionarchive.Manifest, error) {
		close(entered)
		<-release
		return verify(ctx, path, options)
	}
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
	if ack, err := a.HandleCommand(context.Background(), c); err != nil || ack.Status != "accepted" {
		t.Fatalf("archive admission = %#v %v", ack, err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated active project blocked export")
	}
	blocked := agentCommand(runtime, session, protocol.StartTurn)
	if ack, err := a.HandleCommand(context.Background(), blocked); err != nil || ack.Error == nil || ack.Error.Code != protocol.SessionBusy {
		t.Fatalf("target command was not fenced: %#v %v", ack, err)
	}
	inspect := agentCommand(runtime, other, protocol.CodexCommand)
	inspect.Arguments.Codex = &protocol.CodexCommandPayload{Name: "pwd"}
	// Admission is independently testable without issuing a model prompt.
	a.updateMu.Lock()
	if a.archiveBlocksCommand(inspect) {
		t.Error("unrelated project command was fenced")
	}
	a.updateMu.Unlock()
	if _, err := a.prepareUpdate(context.Background(), time.Second); err == nil {
		t.Error("worker update bypassed active archive job")
	}
	close(release)
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandCompleted || record.Result == nil || record.Result.Session == nil || !record.Result.Session.Archived || record.Result.Session.Deleted {
		t.Fatalf("archive result = %#v", record)
	}
	if countCall(server.Calls(), "thread/archive") != 1 || hasCall(server.Calls(), "thread/delete") {
		t.Fatalf("archive used wrong native mutation: %#v", server.Calls())
	}
	if _, err := os.Stat(session.CWD); err != nil {
		t.Fatalf("default archive removed project: %v", err)
	}
	before := countCall(server.Calls(), "thread/archive")
	if ack, err := a.HandleCommand(context.Background(), c); err != nil || ack.Status != "duplicate" {
		t.Fatalf("replay = %#v %v", ack, err)
	}
	if countCall(server.Calls(), "thread/archive") != before {
		t.Fatal("duplicate archive replayed native mutation")
	}
}

func TestArchiveRequiresIndependentVerificationBeforeVisibilityOrCleanup(t *testing.T) {
	a, runtime, server, session, _ := archiveJobFixture(t)
	a.archiveOperations.verify = func(context.Context, string, sessionarchive.VerifyOptions) (sessionarchive.Manifest, error) {
		return sessionarchive.Manifest{}, errors.New("verification failed")
	}
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{DeleteProject: true}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil {
		t.Fatalf("verification failure = %#v", record)
	}
	if hasCall(server.Calls(), "thread/archive") {
		t.Fatal("native source archived before verification")
	}
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].Archived {
		t.Fatalf("verification failure hid source: %#v %v", sessions, err)
	}
	if _, err := os.Stat(session.CWD); err != nil {
		t.Fatalf("verification failure removed source: %v", err)
	}
}

func TestArchiveLateProjectChangesAreKeptAndReportedAsCleanupWarning(t *testing.T) {
	a, runtime, _, session, _ := archiveJobFixture(t)
	verify := a.archiveOperations.verify
	a.archiveOperations.verify = func(ctx context.Context, path string, options sessionarchive.VerifyOptions) (sessionarchive.Manifest, error) {
		if err := os.WriteFile(filepath.Join(session.CWD, "late-unarchived.txt"), []byte("keep late changes"), 0600); err != nil {
			return sessionarchive.Manifest{}, err
		}
		return verify(ctx, path, options)
	}
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{DeleteProject: true}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandCompleted || record.Result == nil || record.Result.SessionArchive == nil || record.Result.SessionArchive.WarningCode != "project_cleanup_failed" || record.Result.SessionArchive.ProjectDeleted {
		t.Fatalf("cleanup warning = %#v", record.Result)
	}
	data, err := os.ReadFile(filepath.Join(session.CWD, "late-unarchived.txt"))
	if err != nil || string(data) != "keep late changes" {
		t.Fatalf("late project data removed or moved: %q %v", data, err)
	}
}

func TestArchiveIdleProofRejectsNativeQueueTerminalAndGoal(t *testing.T) {
	for _, test := range []struct {
		name, method string
		reply        any
	}{
		{"queued input", "thread/queue/list", map[string]any{"data": []any{map[string]any{"id": "queued"}}}},
		{"background terminal", "thread/backgroundTerminals/list", map[string]any{"data": []any{map[string]any{"id": "terminal"}}}},
		{"active goal", "thread/goal/get", map[string]any{"goal": map[string]any{"status": "active", "objective": "fabricated active goal"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, runtime, server, session, _ := archiveJobFixture(t)
			if test.method == "thread/backgroundTerminals/list" {
				server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, []string{session.ThreadID})
			}
			if err := server.SetThreadMethodResult(test.method, session.ThreadID, test.reply); err != nil {
				t.Fatal(err)
			}
			c := agentCommand(runtime, session, protocol.ArchiveSession)
			c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
			if _, err := a.HandleCommand(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			record := waitArchiveTerminal(t, a, c.ID)
			if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.SessionBusy {
				t.Fatalf("idle proof = %#v", record.Result)
			}
			if hasCall(server.Calls(), "thread/archive") {
				t.Fatal("busy native project archived")
			}
		})
	}
}

func TestArchiveRestoreResumesOriginalIdentityAndKeepsBundle(t *testing.T) {
	a, runtime, server, session, manifest := archiveJobFixture(t)
	child := uuid.NewString()
	manifest.ThreadIDs = append(manifest.ThreadIDs, child)
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	archived := waitArchiveTerminal(t, a, c.ID)
	if archived.State != CommandCompleted {
		t.Fatalf("archive = %#v", archived.Result)
	}
	server.SetArchivedThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}, {"id": child, "cwd": session.CWD, "status": "notLoaded"}})
	waitFor(t, func() bool { a.updateMu.Lock(); defer a.updateMu.Unlock(); return len(a.archiveAdmissions) == 0 })
	restore := agentCommand(runtime, protocol.Session{}, protocol.RestoreSession)
	restore.Arguments.SessionArchive = &protocol.SessionArchiveRequest{ArchiveID: archived.Result.SessionArchive.Archive.ID}
	if ack, err := a.HandleCommand(context.Background(), restore); err != nil || ack.Error != nil {
		t.Fatalf("restore admission = %#v %v", ack, err)
	}
	record := waitArchiveTerminal(t, a, restore.ID)
	if record.State != CommandCompleted || record.Result == nil || record.Result.Session == nil || record.Result.Session.Archived || record.Result.Session.ID != session.ID || record.Result.Session.ThreadID != session.ThreadID {
		t.Fatalf("restore = %#v", record.Result)
	}
	if countCall(server.Calls(), "thread/unarchive") != 2 || countCall(server.Calls(), "thread/resume") != 1 {
		t.Fatalf("restore calls = %#v", server.Calls())
	}
	if _, err := os.Stat(filepath.Join(archived.ArchiveJob.Directory, archived.Result.SessionArchive.Archive.ID)); err != nil {
		t.Fatalf("restore removed archive: %v", err)
	}
}

func TestRestoreFencesArchivedDescendantInAnotherRuntimeOutsideProject(t *testing.T) {
	a, runtime, _, session, manifest := archiveJobFixture(t)
	manifest.ArchiveID = "descendant-restore.tar.xz"
	child := uuid.NewString()
	manifest.ThreadIDs = append(manifest.ThreadIDs, child)
	otherCWD := filepath.Join(runtime.DefaultCWD, "outside-project")
	if err := os.Mkdir(otherCWD, 0700); err != nil {
		t.Fatal(err)
	}
	client, server, err := codextest.New(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	other, err := a.store.BeginRuntime("other-runtime", "Other", otherCWD)
	if err != nil {
		t.Fatal(err)
	}
	other.State = "running"
	a.manager.install(other, client)
	server.SetThreads([]map[string]any{{"id": child, "cwd": otherCWD, "status": "active", "turns": []map[string]any{{"id": "busy-child", "status": "inProgress"}}, "source": map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": session.ThreadID, "depth": 1}}}}}, []string{child})
	if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}}); err != nil {
		t.Fatal(err)
	}
	if err := server.SetMethodResult("thread/backgroundTerminals/list", map[string]any{"data": []any{}}); err != nil {
		t.Fatal(err)
	}
	var imports atomic.Int32
	a.archiveOperations.restore = func(context.Context, sessionarchive.RestoreOptions) (sessionarchive.Manifest, error) {
		imports.Add(1)
		return *manifest, nil
	}
	c := agentCommand(runtime, protocol.Session{}, protocol.RestoreSession)
	c.Arguments.SessionArchive = &protocol.SessionArchiveRequest{ArchiveID: manifest.ArchiveID}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandFailed || record.Result.Error == nil || record.Result.Error.Code != protocol.SessionBusy || imports.Load() != 0 {
		t.Fatalf("cross-runtime descendant escaped restore fence: imports=%d result=%#v", imports.Load(), record.Result)
	}
}

func TestRestoreValidationRunsAfterAckWithoutHoldingCommandAdmission(t *testing.T) {
	a, runtime, _, _, _ := archiveJobFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	a.archiveOperations.read = func(context.Context, string) (sessionarchive.Manifest, error) {
		close(entered)
		<-release
		return sessionarchive.Manifest{}, errors.New("fabricated invalid archive")
	}
	c := agentCommand(runtime, protocol.Session{}, protocol.RestoreSession)
	c.Arguments.SessionArchive = &protocol.SessionArchiveRequest{ArchiveID: "slow-validation.tar.xz"}
	ack, err := a.HandleCommand(context.Background(), c)
	if err != nil || ack.Status != "accepted" {
		t.Fatalf("restore ack = %#v %v", ack, err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background validation did not start")
	}
	if !a.updateMu.TryLock() {
		close(release)
		t.Fatal("archive decompression holds command admission")
	}
	a.updateMu.Unlock()
	close(release)
	if record := waitArchiveTerminal(t, a, c.ID); record.State != CommandFailed {
		t.Fatalf("invalid archive result = %#v", record.Result)
	}
}

func TestRestorePinsNewIdentityAgainstDiscoveryDuringImport(t *testing.T) {
	a, runtime, server, source, manifest := archiveJobFixture(t)
	manifest.ThreadID, manifest.ArchiveID = uuid.NewString(), "new-native-import.tar.xz"
	manifest.ThreadIDs = []string{manifest.ThreadID}
	dir, err := sessionArchiveDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.ArchiveID), []byte("fabricated verified archive"), 0600); err != nil {
		t.Fatal(err)
	}
	server.SetThreads([]map[string]any{{"id": source.ThreadID, "cwd": source.CWD, "status": "notLoaded"}, {"id": manifest.ThreadID, "cwd": source.CWD, "status": "notLoaded"}}, nil)
	var hidden atomic.Bool
	a.archiveOperations.restore = func(context.Context, sessionarchive.RestoreOptions) (sessionarchive.Manifest, error) {
		discovered, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: manifest.ThreadID, CWD: manifest.OriginalPath, Name: manifest.DisplayName, State: "idle", Loaded: true})
		hidden.Store(err == nil && discovered.Archived && !discovered.Loaded)
		return *manifest, err
	}
	c := agentCommand(runtime, protocol.Session{}, protocol.RestoreSession)
	c.Arguments.SessionArchive = &protocol.SessionArchiveRequest{ArchiveID: manifest.ArchiveID}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if !hidden.Load() || record.State != CommandCompleted || record.Result.Session == nil || record.Result.Session.Archived {
		t.Fatalf("import visibility escaped pin: hidden=%v result=%#v", hidden.Load(), record.Result)
	}
}

func TestNativeArchiveFenceAllowsUnrelatedThreadAndBlocksDirectTargetMutations(t *testing.T) {
	fence := &nativeArchiveFence{project: "/projects/archive", defaultCWD: "/projects/other", known: map[string]bool{"selected": true, "other": true}, threads: map[string]bool{"selected": true}}
	proxy := &attachmentProxy{archiveFences: map[string]*nativeArchiveFence{"job": fence}, sessions: map[*attachmentSession]struct{}{}}
	for _, test := range []struct {
		payload string
		blocked bool
	}{
		{`{"id":1,"method":"turn/start","params":{"threadId":"selected"}}`, true},
		{`{"id":2,"method":"turn/start","params":{"threadId":"other"}}`, false},
		{`{"id":3,"method":"thread/start","params":{"cwd":"/projects/archive/subdir"}}`, true},
		{`{"id":4,"method":"thread/start","params":{"cwd":"/projects/other"}}`, false},
		{`{"id":5,"method":"fs/writeFile","params":{"path":"/projects/archive/a"}}`, true},
		{`{"id":6,"method":"thread/read","params":{"threadId":"selected"}}`, false},
		{`{"id":7,"method":"turn/start","params":{"threadId":"unknown"}}`, true},
	} {
		if got := proxy.archiveBlocksLocked([]byte(test.payload)); got != test.blocked {
			t.Errorf("blocked(%s)=%v", test.payload, got)
		}
	}
	activity := nativeActivity{turns: map[nativeTurn]struct{}{{thread: "other", turn: "live"}: {}}}
	attached := &attachmentSession{activity: activity}
	proxy.sessions[attached] = struct{}{}
	if err := proxy.checkArchiveIdleLocked(fence); err != nil {
		t.Fatalf("unrelated native activity blocked project: %v", err)
	}
	attached.activity.turns[nativeTurn{thread: "selected", turn: "live"}] = struct{}{}
	if err := proxy.checkArchiveIdleLocked(fence); err == nil {
		t.Fatal("selected native activity passed idle guard")
	}
}

func TestArchiveJournalRecoversAndPinsVisibility(t *testing.T) {
	a, runtime, _, session, _ := archiveJobFixture(t)
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
	if _, err := a.store.Receive(c); err != nil {
		t.Fatal(err)
	}
	job := sessionArchiveJob{ArchiveID: "recover.tar.xz", Directory: t.TempDir(), ProjectPath: session.CWD, Session: session, Phase: "archiving", Exported: true, NativeArchived: true}
	if err := a.store.saveArchiveJob(c, job); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.Update(recoverExecuting); err != nil {
		t.Fatal(err)
	}
	records, err := a.store.archiveJobs()
	if err != nil || len(records) != 1 || records[0].State != CommandExecuting || records[0].ArchiveJob.Phase != "archiving" {
		t.Fatalf("recovery journal = %#v %v", records, err)
	}
	saved, err := a.store.ConfirmArchiveVisibility(runtime, session, job.ArchiveID, true)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := a.store.UpsertRuntimeSession(runtime, session)
	if err != nil || !stale.Archived || stale.ID != session.ID {
		t.Fatalf("stale inventory resurrected archive = %#v %v", stale, err)
	}
	_, changed, err := a.store.RestoreDiscoveredSession(runtime, saved, session)
	if err != nil || changed {
		t.Fatalf("discovery bypassed archive pin: %v %v", changed, err)
	}
	restored, err := a.store.ConfirmArchiveVisibility(runtime, session, job.ArchiveID, false)
	if err != nil || restored.Archived || restored.ID != session.ID {
		t.Fatalf("restore identity = %#v %v", restored, err)
	}
	var progress protocol.Result
	events, _ := a.store.OutboxAfter(0)
	if err := json.Unmarshal(events[0].Data, &progress); err != nil || progress.SessionArchive == nil || progress.SessionArchive.Phase != "archiving" {
		t.Fatalf("durable progress = %#v %v", progress, err)
	}
}

func TestArchiveCleanupResumesTombstoneAndRefusesReplacement(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(project)
	stat := info.Sys().(*syscall.Stat_t)
	job := sessionArchiveJob{ProjectPath: project, RemovalPath: filepath.Join(filepath.Dir(project), ".codex-archived-job"), ProjectDevice: uint64(stat.Dev), ProjectInode: uint64(stat.Ino)}
	parentInfo, _ := os.Stat(filepath.Dir(project))
	parentStat := parentInfo.Sys().(*syscall.Stat_t)
	job.ParentDevice, job.ParentInode = uint64(parentStat.Dev), uint64(parentStat.Ino)
	if err := os.Rename(project, job.RemovalPath); err != nil {
		t.Fatal(err)
	}
	if err := removeArchivedProject(context.Background(), job, sessionarchive.Manifest{Files: []sessionarchive.Entry{{Path: "project", Kind: "directory", Mode: 0700}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(job.RemovalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup staging retained: %v", err)
	}
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	// Inode reuse is possible, so explicitly change the expected identity.
	job.ProjectInode++
	if err := removeArchivedProject(context.Background(), job, sessionarchive.Manifest{Files: []sessionarchive.Entry{{Path: "project", Kind: "directory", Mode: 0700}}}); err == nil {
		t.Fatal("replacement project removed")
	}
}

func TestArchiveRecoveredExportDoesNotReexport(t *testing.T) {
	a, runtime, server, session, manifest := archiveJobFixture(t)
	var exports atomic.Int32
	a.archiveOperations.export = func(context.Context, sessionarchive.ExportOptions) (sessionarchive.Manifest, error) {
		exports.Add(1)
		return sessionarchive.Manifest{}, errors.New("unexpected export")
	}
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
	if _, err := a.store.Receive(c); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	manifest.ArchiveID = "recovered.tar.xz"
	if err := os.WriteFile(filepath.Join(dir, manifest.ArchiveID), []byte("verified"), 0600); err != nil {
		t.Fatal(err)
	}
	job := sessionArchiveJob{ArchiveID: manifest.ArchiveID, Directory: dir, ProjectPath: session.CWD, Session: session, Phase: "verifying", Exported: true}
	if err := a.store.saveArchiveJob(c, job); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverArchiveJobs(); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandCompleted || exports.Load() != 0 || countCall(server.Calls(), "thread/archive") != 1 {
		t.Fatalf("recovered export replayed: %#v exports=%d calls=%#v", record, exports.Load(), server.Calls())
	}
}

func TestArchiveRecoversLostNativeArchiveAcknowledgement(t *testing.T) {
	a, runtime, server, session, manifest := archiveJobFixture(t)
	server.SetArchivedThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}})
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
	if _, err := a.store.Receive(c); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	manifest.ArchiveID = "lost-ack.tar.xz"
	if err := os.WriteFile(filepath.Join(dir, manifest.ArchiveID), []byte("verified"), 0600); err != nil {
		t.Fatal(err)
	}
	job := sessionArchiveJob{ArchiveID: manifest.ArchiveID, Directory: dir, ProjectPath: session.CWD, Session: session, Phase: "archiving", Exported: true}
	if err := a.store.saveArchiveJob(c, job); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverArchiveJobs(); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandCompleted || record.Result.Session == nil || !record.Result.Session.Archived || countCall(server.Calls(), "thread/archive") != 0 {
		t.Fatalf("lost ACK recovery = %#v", record.Result)
	}
}

func TestArchiveCleanupKeepsReplacementBetweenCheckAndRename(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(project)
	stat := info.Sys().(*syscall.Stat_t)
	parentInfo, _ := os.Stat(parent)
	parentStat := parentInfo.Sys().(*syscall.Stat_t)
	job := sessionArchiveJob{ProjectPath: project, RemovalPath: filepath.Join(parent, ".codex-archived-test"), ProjectDevice: uint64(stat.Dev), ProjectInode: uint64(stat.Ino), ParentDevice: uint64(parentStat.Dev), ParentInode: uint64(parentStat.Ino)}
	err := removeArchivedProjectWithRename(context.Background(), job, sessionarchive.Manifest{Files: []sessionarchive.Entry{{Path: "project", Kind: "directory", Mode: 0700}}}, func(handle *os.File, source, target string) error {
		if err := os.Rename(project, filepath.Join(parent, "source-kept")); err != nil {
			return err
		}
		if err := os.Mkdir(project, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(project, "replacement.txt"), []byte("keep this"), 0600); err != nil {
			return err
		}
		return renameArchiveProject(handle, source, target)
	})
	if err == nil {
		t.Fatal("source replacement was deleted")
	}
	data, readErr := os.ReadFile(filepath.Join(job.RemovalPath, "replacement.txt"))
	if readErr != nil || string(data) != "keep this" {
		t.Fatalf("replacement contents lost: %q %v", data, readErr)
	}
	if _, err := os.Stat(filepath.Join(parent, "source-kept")); err != nil {
		t.Fatalf("original source lost: %v", err)
	}
}

func TestArchiveCleanupRefusesReplacedParentSymlink(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "parent")
	project := filepath.Join(parent, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(project)
	stat := info.Sys().(*syscall.Stat_t)
	parentInfo, _ := os.Stat(parent)
	parentStat := parentInfo.Sys().(*syscall.Stat_t)
	job := sessionArchiveJob{ProjectPath: project, RemovalPath: filepath.Join(parent, ".codex-archived-test"), ProjectDevice: uint64(stat.Dev), ProjectInode: uint64(stat.Ino), ParentDevice: uint64(parentStat.Dev), ParentInode: uint64(parentStat.Ino)}
	if err := os.Rename(parent, filepath.Join(base, "parent-kept")); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(base, "replacement")
	if err := os.MkdirAll(filepath.Join(replacement, "project"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, parent); err != nil {
		t.Fatal(err)
	}
	if err := removeArchivedProject(context.Background(), job, sessionarchive.Manifest{Files: []sessionarchive.Entry{{Path: "project", Kind: "directory", Mode: 0700}}}); err == nil {
		t.Fatal("replaced parent admitted for cleanup")
	}
	if _, err := os.Stat(filepath.Join(replacement, "project")); err != nil {
		t.Fatalf("replacement removed: %v", err)
	}
}

func TestArchiveCleanupRecoversAfterHalfwayRemovalWithoutRestoringPartialSource(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged remaining file", true: "changed remaining file"}[changed], func(t *testing.T) {
			a, runtime, _, session, manifest := archiveJobFixture(t)
			for _, name := range []string{"a.txt", "b.txt"} {
				data := []byte("archived " + name)
				if err := os.WriteFile(filepath.Join(session.CWD, name), data, 0600); err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(data)
				manifest.Files = append(manifest.Files, sessionarchive.Entry{Path: "project/" + name, Kind: "file", Mode: 0600, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])})
			}
			info, _ := os.Stat(session.CWD)
			stat := info.Sys().(*syscall.Stat_t)
			parentInfo, _ := os.Stat(filepath.Dir(session.CWD))
			parentStat := parentInfo.Sys().(*syscall.Stat_t)
			c := agentCommand(runtime, session, protocol.ArchiveSession)
			c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{DeleteProject: true}
			if _, err := a.store.Receive(c); err != nil {
				t.Fatal(err)
			}
			job := sessionArchiveJob{ArchiveID: "halfway.tar.xz", Directory: t.TempDir(), ProjectPath: session.CWD, Session: session, Phase: "cleaning", Exported: true, NativeArchived: true, ProjectDevice: uint64(stat.Dev), ProjectInode: uint64(stat.Ino), ParentDevice: uint64(parentStat.Dev), ParentInode: uint64(parentStat.Ino), RemovalPath: filepath.Join(filepath.Dir(session.CWD), ".codex-archived-halfway")}
			if err := a.store.saveArchiveJob(c, job); err != nil {
				t.Fatal(err)
			}
			err := removeArchivedProjectWithHooks(context.Background(), job, *manifest, archiveCleanupHooks{rename: renameArchiveProject, checkpoint: func() error { job.CleanupVerified = true; return a.store.saveArchiveJob(c, job) }, afterRemove: func(string) error { return errors.New("fabricated crash after first child") }})
			if err == nil {
				t.Fatal("halfway fault not injected")
			}
			entries, err := os.ReadDir(job.RemovalPath)
			if err != nil || len(entries) != 1 {
				t.Fatalf("halfway tree = %#v %v", entries, err)
			}
			if changed {
				if err := os.WriteFile(filepath.Join(job.RemovalPath, entries[0].Name()), []byte("new unarchived content"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.store.db.Update(recoverExecuting); err != nil {
				t.Fatal(err)
			}
			records, err := a.store.archiveJobs()
			if err != nil || len(records) != 1 || !records[0].ArchiveJob.CleanupVerified {
				t.Fatalf("cleanup checkpoint = %#v %v", records, err)
			}
			err = removeArchivedProject(context.Background(), *records[0].ArchiveJob, *manifest)
			if changed {
				if err == nil {
					t.Fatal("changed remaining file deleted")
				}
				if data, err := os.ReadFile(filepath.Join(job.RemovalPath, entries[0].Name())); err != nil || string(data) != "new unarchived content" {
					t.Fatalf("changed content lost: %q %v", data, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(job.RemovalPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("partial cleanup did not finish: %v", err)
				}
			}
			if _, err := os.Stat(session.CWD); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial source path recreated: %v", err)
			}
		})
	}
}

func TestNativeArchiveFenceResolvesMissingPathThroughSymlinkParent(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	proxy := &attachmentProxy{archiveFences: map[string]*nativeArchiveFence{"job": {project: project, known: map[string]bool{}, threads: map[string]bool{}}}}
	payload, _ := json.Marshal(map[string]any{"id": 1, "method": "fs/writeFile", "params": map[string]any{"path": filepath.Join(alias, "not-created.txt")}})
	if !proxy.archiveBlocksLocked(payload) {
		t.Fatal("symlink-parent file mutation bypassed archive fence")
	}
}

func TestNativeArchiveFenceLearnsNewUnrelatedUserThreads(t *testing.T) {
	proxy := &attachmentProxy{archiveFences: map[string]*nativeArchiveFence{"job": {project: "/projects/archive", known: map[string]bool{}, threads: map[string]bool{"selected": true}}}}
	mutation := []byte("{\"id\":7,\"method\":\"turn/start\",\"params\":{\"threadId\":\"new-other\"}}")
	if !proxy.archiveBlocksLocked(mutation) {
		t.Fatal("unknown thread admitted before CWD proof")
	}
	proxy.observeArchiveBackendThreadLocked([]byte("{\"id\":1,\"result\":{\"thread\":{\"id\":\"new-other\",\"cwd\":\"/projects/other\",\"source\":\"cli\"}}}"))
	if proxy.archiveBlocksLocked(mutation) {
		t.Fatal("new unrelated user thread remained blocked after native CWD proof")
	}
	piggyback, _ := json.Marshal(map[string]any{"id": 10, "method": "fs/remove", "params": map[string]any{"threadId": "new-other", "paths": []string{"/projects/archive/unarchived.txt"}}})
	if !proxy.archiveBlocksLocked(piggyback) {
		t.Fatal("filesystem mutation borrowed unrelated thread admission")
	}
	override, _ := json.Marshal(map[string]any{"id": 8, "method": "thread/resume", "params": map[string]any{"threadId": "new-other", "cwd": "/projects/archive"}})
	if !proxy.archiveBlocksLocked(override) {
		t.Fatal("known unrelated thread moved into protected CWD")
	}
	proxy.observeArchiveBackendThreadLocked([]byte("{\"id\":9,\"result\":{\"thread\":{\"id\":\"new-other\",\"cwd\":\"/projects/archive\",\"source\":\"cli\"}}}"))
	if !proxy.archiveBlocksLocked(mutation) {
		t.Fatal("known thread retained admission after its CWD changed")
	}
	proxy.observeArchiveBackendThreadLocked([]byte("{\"id\":2,\"result\":{\"thread\":{\"id\":\"selected\",\"cwd\":\"/projects/other\",\"source\":\"cli\"}}}"))
	if !proxy.archiveBlocksLocked([]byte("{\"id\":3,\"method\":\"turn/start\",\"params\":{\"threadId\":\"selected\"}}")) {
		t.Fatal("protected thread identity lost its fence")
	}
}

func TestArchiveBlocksQueuedCreationWithDefaultRuntimeDirectory(t *testing.T) {
	a, runtime, _, session, _ := archiveJobFixture(t)
	c := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	if _, err := a.store.Receive(c); err != nil {
		t.Fatal(err)
	}
	if err := a.store.checkArchivePending("archive-job", session.CWD, map[string]string{runtime.ID: runtime.DefaultCWD}); err == nil {
		t.Fatal("queued default workspace creation bypassed idle proof")
	}
}

func TestWebUINewSessionCannotTargetProjectBeingArchived(t *testing.T) {
	a, runtime, server, session, _ := archiveJobFixture(t)
	other := filepath.Join(runtime.DefaultCWD, "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{CWD: other, ThreadID: "unrelated"}}
	a.updateMu.Lock()
	a.archiveAdmissions = map[string]*archiveAdmission{"job": {Project: session.CWD, ThreadID: session.ThreadID, Threads: map[string]map[string]bool{}}}
	args, _ := json.Marshal(map[string]any{"name": "new session", "cwd": session.CWD})
	client, _, _ := a.manager.Client(runtime.ID)
	_, err := actor.webUINewSession(context.Background(), client, string(args))
	a.updateMu.Unlock()
	var refusal *protocol.Error
	if !errors.As(err, &refusal) || refusal.Code != protocol.SessionBusy || hasCall(server.Calls(), "thread/start") {
		t.Fatalf("new session bypassed archive admission: %v", err)
	}
}

func TestArchiveCleanupProtectsConfiguredRuntimeStartupDirectory(t *testing.T) {
	a, _, _, session, _ := archiveJobFixture(t)
	a.cfg.Runtimes[0].WorkingDirectory = session.CWD
	err := a.verifyProjectRemoval(sessionArchiveJob{ProjectPath: session.CWD, Session: session, Directory: filepath.Join(os.Getenv("HOME"), "CODEX_Archived")}, t.TempDir(), t.TempDir())
	var refusal *protocol.Error
	if !errors.As(err, &refusal) || refusal.Code != protocol.InvalidWorkspace {
		t.Fatalf("runtime startup directory deletion accepted: %v", err)
	}
}

func TestArchiveRejectsUnsupportedNativeVersionBeforeGuardRPCs(t *testing.T) {
	a, runtime, server, session, _ := archiveJobFixture(t)
	a.archiveOperations.compatible = sessionarchive.CheckCompatibility
	runtime.CodexVersion = "codex-cli 0.162.0"
	client, _, _ := a.manager.Client(runtime.ID)
	a.manager.install(runtime, client)
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandFailed || record.Result.Error == nil || record.Result.Error.Code != protocol.CodexMethodUnsupported {
		t.Fatalf("unsupported version result=%#v", record.Result)
	}
	if hasCall(server.Calls(), "thread/list") || hasCall(server.Calls(), "thread/archive") {
		t.Fatal("unsupported version reached new inventory or archive APIs")
	}
}
