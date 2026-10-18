package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/sessionarchive"
)

// Uses fabricated conversations and an offline provider in an isolated HOME.
// No model turn, credentials, or existing native session is accessed.
func TestNativeWorkerArchiveDeleteProjectAndRestoreOriginalSession(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_ARCHIVE_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_ARCHIVE_TEST=1 for isolated installed-Codex worker proof")
	}
	if os.Getenv("CODEX_NATIVE_ARCHIVE_UMASK_CHILD") != "1" {
		// Umask is process-wide: isolate the production worker's restrictive
		// setting from other tests, including tests running in parallel.
		testBinary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, testBinary, "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=9m")
		command.Env = append(os.Environ(), "CODEX_NATIVE_ARCHIVE_UMASK_CHILD=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("native worker archive proof with umask 0077: %v\n%s", err, output)
		}
		t.Logf("native worker archive proof with umask 0077:\n%s", output)
		return
	}
	syscall.Umask(0077)
	binary := os.Getenv("CODEX_REAL_BINARY")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct {
		mode, state string
		loaded      bool
	}{
		{"legacy", "loaded", true},
		{"legacy", "unloaded", false},
		{"paginated", "loaded", true},
		{"paginated", "unloaded", false},
	} {
		t.Run(fixture.mode+"/"+fixture.state, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			home := t.TempDir()
			t.Setenv("HOME", home)
			codexHome := filepath.Join(home, ".codex")
			workspace := filepath.Join(home, "projects")
			project := filepath.Join(workspace, "source-project")
			stateDir := filepath.Join(home, "worker-state")
			for _, dir := range []string{codexHome, project, stateDir} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model_provider = \"archive_verify\"\nmodel = \"archive-proof\"\ncheck_for_update_on_startup = false\n[model_providers.archive_verify]\nname = \"Offline worker archive test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[features]\nplugins = false\n"), 0600); err != nil {
				t.Fatal(err)
			}
			projectFiles := []struct {
				name, contents string
				mode           os.FileMode
			}{
				{"project.txt", "fabricated project contents", 0640},
				{"readable.txt", "fabricated readable contents", 0644},
				{"executable.sh", "#!/bin/sh\nexit 0\n", 0755},
			}
			for _, file := range projectFiles {
				path := filepath.Join(project, file.name)
				if err := os.WriteFile(path, []byte(file.contents), 0600); err != nil {
					t.Fatal(err)
				}
				// Creation modes are masked by umask, so set and check the source
				// modes explicitly before exercising the archive worker.
				if err := os.Chmod(path, file.mode); err != nil {
					t.Fatal(err)
				}
			}
			assertProjectFiles := func() {
				t.Helper()
				for _, file := range projectFiles {
					path := filepath.Join(project, file.name)
					if data, err := os.ReadFile(path); err != nil || string(data) != file.contents {
						t.Fatalf("project file %s contents = %q, %v", file.name, data, err)
					}
					info, err := os.Lstat(path)
					if err != nil {
						t.Fatal(err)
					}
					if !info.Mode().IsRegular() || info.Mode().Perm() != file.mode {
						t.Fatalf("project file %s mode = %v, want regular file with permissions %04o", file.name, info.Mode(), file.mode)
					}
				}
			}
			assertProjectFiles()
			parentSentinel := filepath.Join(workspace, "parent-sentinel.txt")
			sibling := filepath.Join(workspace, "neighbor-project")
			if err := os.Mkdir(sibling, 0700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{parentSentinel, filepath.Join(sibling, "keep.txt")} {
				if err := os.WriteFile(path, []byte("outside the archived project"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			outsideInfo := make(map[string]os.FileInfo)
			for _, path := range []string{workspace, sibling, parentSentinel, filepath.Join(sibling, "keep.txt")} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				outsideInfo[path] = info
			}
			assertParentPreserved := func() {
				t.Helper()
				for path, before := range outsideInfo {
					after, err := os.Stat(path)
					if err != nil || !os.SameFile(before, after) {
						t.Fatalf("archive replaced or removed a path outside the selected project %s: %v", path, err)
					}
				}
				for _, path := range []string{parentSentinel, filepath.Join(sibling, "keep.txt")} {
					if data, err := os.ReadFile(path); err != nil || string(data) != "outside the archived project" {
						t.Fatalf("archive changed a file outside the selected project: %v", err)
					}
				}
			}
			threadID := uuid.NewString()
			writeWorkerArchiveNativeFixture(t, codexHome, project, threadID, fixture.mode)
			ancestorID := uuid.NewString()
			writeWorkerArchiveNativeFixture(t, codexHome, workspace, ancestorID, fixture.mode)
			client, err := codexadapter.Start(ctx, codexadapter.Config{Command: binary, WorkingDirectory: workspace, Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + codexHome, "CODEX_SQLITE_HOME=" + codexHome}, Stderr: io.Discard, RequestTimeout: 15 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var thread codexadapter.Thread
			if fixture.loaded {
				thread, err = client.ResumeThread(ctx, threadID, codexadapter.ThreadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := client.RenameThread(ctx, threadID, "Worker archive native proof"); err != nil {
					t.Fatal(err)
				}
				nameConfirmed := false
				nameDeadline := time.NewTimer(5 * time.Second)
			nameEvents:
				for {
					select {
					case event := <-client.Events():
						if event.Method == "thread/name/updated" {
							nameConfirmed = event.Name != nil && *event.Name == "Worker archive native proof"
							break nameEvents
						}
					case <-nameDeadline.C:
						break nameEvents
					}
				}
				nameDeadline.Stop()
				if !nameConfirmed {
					t.Fatal("native rename notification did not confirm the session title")
				}
			} else {
				thread, err = client.ReadThreadState(ctx, threadID)
				if err != nil || thread.Status != "notLoaded" || thread.ActiveTurnID != "" {
					t.Fatalf("persisted session must be unloaded and idle: status=%q active=%q err=%v", thread.Status, thread.ActiveTurnID, err)
				}
				loaded, complete, err := loadedThreadIDs(ctx, client)
				if err != nil || !complete || loaded[threadID] {
					t.Fatalf("persisted session unexpectedly loaded: loaded=%v complete=%v err=%v", loaded[threadID], complete, err)
				}
				// The native terminal RPC only accepts loaded threads. Preserve
				// this regression precondition instead of silently resuming the
				// saved session before the worker's idle proof runs.
				_, err = client.ListBackgroundTerminals(ctx, threadID)
				var rpc *codexadapter.RPCError
				if !errors.As(err, &rpc) || rpc.Code != -32600 || rpc.Message != "thread not found: "+threadID {
					t.Fatalf("expected native unloaded-thread terminal error, got %v", err)
				}
			}
			thread.Name = "Worker archive native proof"
			workerID := uuid.NewString()
			stateFile := filepath.Join(stateDir, "worker.sqlite")
			store, err := OpenStore(stateFile, workerID)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := config.WorkerConfig{WorkerID: workerID, StateFile: stateFile, TokenFile: filepath.Join(stateDir, "token"), AllowedWorkspaceRoots: []string{workspace}, Runtimes: []config.RuntimeProfile{{ID: "proof", CodexBinary: binary, WorkingDirectory: workspace}}}
			a, err := NewAgent(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			a.ctx = ctx
			engine := a.sessionArchiveEngine()
			restoreNative := engine.restore
			engine.restore = func(ctx context.Context, options sessionarchive.RestoreOptions) (sessionarchive.Manifest, error) {
				manifest, err := restoreNative(ctx, options)
				if err != nil {
					t.Logf("fabricated native restore failure: %v", err)
				}
				return manifest, err
			}
			a.archiveOperations = &engine
			defer func() { cancel(); a.group.Wait() }()
			runtime, err := store.BeginRuntime("proof", "Proof", workspace)
			if err != nil {
				t.Fatal(err)
			}
			runtime.State, runtime.CodexVersion = "running", "0.163.0"
			a.manager.install(runtime, client)
			session, err := store.UpsertRuntimeSession(runtime, sessionFromThread(runtime, thread, fixture.loaded))
			if err != nil {
				t.Fatal(err)
			}
			a.onSession(runtime, session)
			// A dormant session in the CODEX-like parent is independent of
			// the subfolder being archived. Preserve both its native record
			// and everything outside the selected project during cleanup.
			ancestor, err := client.ReadThreadState(ctx, ancestorID)
			if err != nil || ancestor.Status != "notLoaded" {
				t.Fatalf("native ancestor session = %q, %v", ancestor.Status, err)
			}
			ancestorSession, err := store.UpsertRuntimeSession(runtime, sessionFromThread(runtime, ancestor, false))
			if err != nil {
				t.Fatal(err)
			}
			a.onSession(runtime, ancestorSession)
			command := agentCommand(runtime, session, protocol.ArchiveSession)
			command.Arguments.CWD, command.Arguments.SessionArchive = project, &protocol.SessionArchiveRequest{DeleteProject: true}
			if ack, err := a.HandleCommand(ctx, command); err != nil || ack.Error != nil {
				t.Fatalf("archive admission=%#v %v", ack, err)
			}
			archived := waitNativeArchiveRecord(t, ctx, store, command.ID)
			if archived.State != CommandCompleted || archived.Result.SessionArchive == nil || !archived.Result.SessionArchive.ProjectDeleted || archived.Result.SessionArchive.WarningCode != "" {
				t.Fatalf("native worker archive = %#v error=%+v", archived.Result, archived.Result.Error)
			}
			if _, err := os.Stat(project); !os.IsNotExist(err) {
				t.Fatalf("source project remains: %v", err)
			}
			assertParentPreserved()
			archivePath := filepath.Join(home, "CODEX_Archived", archived.Result.SessionArchive.Archive.ID)
			manifest, err := sessionarchive.Read(ctx, archivePath)
			if err != nil {
				t.Fatal(err)
			}
			archiveFiles := make(map[string]sessionarchive.Entry)
			for _, entry := range manifest.Files {
				archiveFiles[entry.Path] = entry
			}
			for _, file := range projectFiles {
				path := "project/" + file.name
				entry, found := archiveFiles[path]
				if !found || entry.Kind != "file" || entry.Mode != uint32(file.mode) {
					t.Fatalf("archived project file %s = %#v, found=%v; want file with permissions %04o", path, entry, found, file.mode)
				}
			}
			if archived, err := client.ThreadArchived(ctx, ancestorID); err != nil || archived {
				t.Fatalf("archive affected the ancestor session: archived=%v error=%v", archived, err)
			}
			if archived.Result.Session.ID != session.ID || !archived.Result.Session.Archived || archived.Result.Session.Deleted {
				t.Fatalf("archive changed gateway identity: %#v", archived.Result.Session)
			}
			for {
				a.updateMu.Lock()
				idle := len(a.archiveAdmissions) == 0
				a.updateMu.Unlock()
				if idle {
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			restore := agentCommand(runtime, protocol.Session{}, protocol.RestoreSession)
			restore.Arguments.SessionArchive = &protocol.SessionArchiveRequest{ArchiveID: archived.Result.SessionArchive.Archive.ID}
			if ack, err := a.HandleCommand(ctx, restore); err != nil || ack.Error != nil {
				t.Fatalf("restore admission=%#v %v", ack, err)
			}
			restored := waitNativeArchiveRecord(t, ctx, store, restore.ID)
			if restored.State != CommandCompleted || restored.Result.Session == nil || restored.Result.Session.ID != session.ID || restored.Result.Session.ThreadID != threadID || restored.Result.Session.Archived || !restored.Result.Session.Loaded {
				t.Fatalf("native worker restore = %#v error=%+v", restored.Result, restored.Result.Error)
			}
			assertProjectFiles()
			assertParentPreserved()
			if _, err := os.Stat(archivePath); err != nil {
				t.Fatal("restore removed verified archive")
			}
		})
	}
}

func waitNativeArchiveRecord(t *testing.T, ctx context.Context, store *Store, id string) CommandRecord {
	t.Helper()
	for {
		record, found, err := store.LoadCommand(id)
		if err != nil {
			t.Fatal(err)
		}
		if found && record.State != CommandReceived && record.State != CommandExecuting {
			return record
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func writeWorkerArchiveNativeFixture(t *testing.T, home, project, id, mode string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "18")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "rollout-2026-10-18T12-00-00-"+id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": id, "session_id": id, "timestamp": "2026-10-18T12:00:00Z", "cwd": project, "originator": "codex_cli_rs", "cli_version": "0.163.0", "source": "cli", "model_provider": "archive_verify", "history_mode": mode, "base_instructions": map[string]any{"text": "Fabricated native worker archive proof.", "version": 1}}},
		{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "proof-turn", "model_context_window": 128000, "started_at": 1792324800}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Fabricated worker archive input."}}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Fabricated worker archive reply."}}, "phase": "final_answer"}},
		{"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "proof-turn", "last_agent_message": "Fabricated worker archive reply.", "started_at": 1792324800, "completed_at": 1792324801}},
	}
	for i, record := range records {
		record["timestamp"] = "2026-10-18T12:00:00Z"
		if mode == "paginated" {
			record["ordinal"] = i
		}
		if err := json.NewEncoder(f).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
}
