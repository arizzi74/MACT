package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	binary := os.Getenv("CODEX_REAL_BINARY")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
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
			if err := os.WriteFile(filepath.Join(project, "project.txt"), []byte("fabricated project contents"), 0640); err != nil {
				t.Fatal(err)
			}
			threadID := uuid.NewString()
			writeWorkerArchiveNativeFixture(t, codexHome, project, threadID, mode)
			client, err := codexadapter.Start(ctx, codexadapter.Config{Command: binary, WorkingDirectory: workspace, Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + codexHome, "CODEX_SQLITE_HOME=" + codexHome}, Stderr: io.Discard, RequestTimeout: 15 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			thread, err := client.ResumeThread(ctx, threadID, codexadapter.ThreadOptions{})
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
			session, err := store.UpsertRuntimeSession(runtime, sessionFromThread(runtime, thread, true))
			if err != nil {
				t.Fatal(err)
			}
			a.onSession(runtime, session)
			command := agentCommand(runtime, session, protocol.ArchiveSession)
			command.Arguments.CWD, command.Arguments.SessionArchive = project, &protocol.SessionArchiveRequest{DeleteProject: true}
			if ack, err := a.HandleCommand(ctx, command); err != nil || ack.Error != nil {
				t.Fatalf("archive admission=%#v %v", ack, err)
			}
			archived := waitNativeArchiveRecord(t, ctx, store, command.ID)
			if archived.State != CommandCompleted || archived.Result.SessionArchive == nil || !archived.Result.SessionArchive.ProjectDeleted || archived.Result.SessionArchive.WarningCode != "" {
				t.Fatalf("native worker archive = %#v", archived.Result)
			}
			if _, err := os.Stat(project); !os.IsNotExist(err) {
				t.Fatalf("source project remains: %v", err)
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
			if data, err := os.ReadFile(filepath.Join(project, "project.txt")); err != nil || string(data) != "fabricated project contents" {
				t.Fatalf("restored project=%q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(home, "CODEX_Archived", archived.Result.SessionArchive.Archive.ID)); err != nil {
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
