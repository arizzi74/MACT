package claudeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

func deleteTestConfig(t *testing.T) (Config, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.WorkingDirectory = filepath.Join(filepath.Dir(cfg.StateDirectory), "CLAUDE_CODE", "Project_with_spaces")
	if err := os.MkdirAll(cfg.WorkingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := nativeConfigDirectory(cfg.Env, cfg.WorkingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, root
}

func writeDeleteFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
func existsDeleteFixture(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if want && err != nil {
		t.Fatalf("expected preserved artifact %s: %v", filepath.Base(path), err)
	}
	if !want && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact %s was not removed: %v", filepath.Base(path), err)
	}
}
func deletionRPCCode(t *testing.T, err error, want int) {
	t.Helper()
	var rpc *codexadapter.RPCError
	if !errors.As(err, &rpc) || rpc.Code != want {
		t.Fatalf("deletion error=%v want RPC %d", err, want)
	}
}

func TestClaudeDeletionRemovesOnlyOwnedNativeArtifactsAndAdapterHistory(t *testing.T) {
	cfg, native := deleteTestConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	other := startThread(t, r)
	r.server.mu.Lock()
	r.server.threads[other.ID].CWD = filepath.Dir(cfg.StateDirectory)
	r.server.mu.Unlock() // A separate home-CWD session must not block metadata deletion below home.
	runTurn(t, r, thread.ID, "saved private session prompt")
	r.server.mu.Lock()
	owned := r.server.runners[thread.ID]
	saved := r.server.threads[thread.ID]
	r.server.mu.Unlock()
	if owned == nil {
		t.Fatal("no owned native process to retire")
	}
	project := filepath.Join(native, "projects", "arbitrary-project-encoding")
	deleted := []string{filepath.Join(project, thread.ID+".jsonl"), filepath.Join(project, thread.ID+".orphaned-1760000000000-abc123.jsonl"), filepath.Join(project, thread.ID+".jsonl.superseded-1760000000001"), filepath.Join(project, thread.ID, "subagents", "child.jsonl"), filepath.Join(project, thread.ID, "tool-results", "result.txt"), filepath.Join(project, thread.ID, "mcp-tasks", "task.json")}
	for _, path := range deleted {
		writeDeleteFixture(t, path, "PRIVATE_NATIVE_CONVERSATION")
	}
	preserved := []string{filepath.Join(cfg.WorkingDirectory, "project-sentinel"), filepath.Join(project, other.ID+".jsonl"), filepath.Join(project, other.ID, "subagents", "other.jsonl"), filepath.Join(project, "memory", "CLAUDE.md"), filepath.Join(project, thread.ID+"-another.jsonl"), filepath.Join(project, thread.ID+".jsonl.backup"), filepath.Join(native, "history.jsonl"), filepath.Join(native, "settings.json"), filepath.Join(native, ".credentials.json")}
	for _, path := range preserved {
		writeDeleteFixture(t, path, "KEEP_ME")
	}
	if err := r.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owned.done:
	default:
		t.Fatal("delete returned before the owned native process was reaped")
	}
	for _, path := range deleted {
		existsDeleteFixture(t, path, false)
	}
	existsDeleteFixture(t, filepath.Join(project, thread.ID), false)
	for _, path := range preserved {
		if data, err := os.ReadFile(path); err != nil || string(data) != "KEEP_ME" {
			t.Fatalf("unrelated artifact changed: %s %v", filepath.Base(path), err)
		}
	}
	existsDeleteFixture(t, filepath.Join(cfg.StateDirectory, thread.ID+".json"), false)
	r.server.mu.Lock()
	err := r.server.save(saved)
	r.server.mu.Unlock()
	if err == nil {
		t.Fatal("late native callback recreated deleted adapter history")
	}
	if _, err := r.ReadThread(context.Background(), thread.ID, true); err == nil {
		t.Fatal("deleted conversation was still readable")
	}
	if _, err := r.ReadThread(context.Background(), other.ID, false); err != nil {
		t.Fatal("same-project session was removed", err)
	}
	receipt := filepath.Join(cfg.StateDirectory, deletionJournalDirectory, thread.ID+".json")
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE_NATIVE_CONVERSATION") || strings.Contains(string(data), "saved private session prompt") || !strings.Contains(string(data), `"done":true`) {
		t.Fatal("deletion receipt retained conversation content or was incomplete")
	}
	var minimal map[string]json.RawMessage
	if json.Unmarshal(data, &minimal) != nil || len(minimal) != 3 || minimal["version"] == nil || minimal["threadId"] == nil || minimal["done"] == nil {
		t.Fatalf("completed receipt retained recovery metadata: %s", data)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	if err := r2.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal("completed deletion was not idempotent after restart", err)
	}
	if _, err := r2.ReadThread(context.Background(), thread.ID, false); err == nil {
		t.Fatal("deleted session resurrected after restart")
	}
	deletionRPCCode(t, r2.DeleteThread(context.Background(), uuid.NewString()), -32602)
}

func TestClaudeDeletionHandlesEmptySessionAndRejectsBusyWorkBeforeIntent(t *testing.T) {
	for _, state := range []string{"empty", "turn", "queue", "question"} {
		t.Run(state, func(t *testing.T) {
			cfg, _ := deleteTestConfig(t)
			r := startTest(t, cfg)
			thread := startThread(t, r)
			r.server.mu.Lock()
			saved := r.server.threads[thread.ID]
			switch state {
			case "turn":
				saved.Turns = append(saved.Turns, &savedTurn{ID: uuid.NewString(), Status: "inProgress", Items: []map[string]any{}})
			case "queue":
				saved.Queue = append(saved.Queue, &queuedInput{ID: uuid.NewString(), Content: []map[string]any{{"type": "text", "text": "queued"}}})
			case "question":
				r.server.pending["pending"] = &permission{Process: &process{threadID: thread.ID}}
			}
			r.server.mu.Unlock()
			err := r.DeleteThread(context.Background(), thread.ID)
			if state == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				existsDeleteFixture(t, filepath.Join(cfg.StateDirectory, thread.ID+".json"), false)
			} else {
				deletionRPCCode(t, err, -32602)
				existsDeleteFixture(t, filepath.Join(cfg.StateDirectory, deletionJournalDirectory, thread.ID+".json"), false)
				existsDeleteFixture(t, filepath.Join(cfg.StateDirectory, thread.ID+".json"), true)
			}
		})
	}
}

func TestClaudeDeletionNeverFollowsNativeArtifactSymlinks(t *testing.T) {
	for _, kind := range []string{"transcript", "sidecar", "project", "projects", "nested link"} {
		t.Run(kind, func(t *testing.T) {
			cfg, native := deleteTestConfig(t)
			r := startTest(t, cfg)
			thread := startThread(t, r)
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "KEEP")
			writeDeleteFixture(t, sentinel, "outside project")
			project := filepath.Join(native, "projects", "project")
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			main := filepath.Join(project, thread.ID+".jsonl")
			writeDeleteFixture(t, main, "selected transcript")
			sidecar := filepath.Join(project, thread.ID)
			switch kind {
			case "transcript":
				if err := os.Remove(main); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, main); err != nil {
					t.Fatal(err)
				}
			case "sidecar":
				if err := os.Symlink(outside, sidecar); err != nil {
					t.Fatal(err)
				}
			case "project":
				if err := os.RemoveAll(project); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, project); err != nil {
					t.Fatal(err)
				}
			case "projects":
				if err := os.RemoveAll(filepath.Join(native, "projects")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(native, "projects")); err != nil {
					t.Fatal(err)
				}
			case "nested link":
				if err := os.MkdirAll(sidecar, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(sidecar, "tool-results")); err != nil {
					t.Fatal(err)
				}
			}
			err := r.DeleteThread(context.Background(), thread.ID)
			if kind == "nested link" {
				if err != nil {
					t.Fatal(err)
				}
				existsDeleteFixture(t, sidecar, false)
			} else {
				deletionRPCCode(t, err, -32010)
				existsDeleteFixture(t, filepath.Join(cfg.StateDirectory, thread.ID+".json"), true)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside project" {
				t.Fatal("symlink cleanup escaped into another project")
			}
		})
	}
}

func TestClaudeDeletionPreservesCWDWhenConfigurationOverlapsArtifacts(t *testing.T) {
	for _, overlap := range []string{"artifact inside cwd", "cwd inside sidecar"} {
		t.Run(overlap, func(t *testing.T) {
			cfg, native := deleteTestConfig(t)
			if overlap == "artifact inside cwd" {
				native = filepath.Join(cfg.WorkingDirectory, "native-data")
				cfg.Env = append(cfg.Env, "CLAUDE_CONFIG_DIR="+native)
			}
			r := startTest(t, cfg)
			thread := startThread(t, r)
			project := filepath.Join(native, "projects", "project")
			main := filepath.Join(project, thread.ID+".jsonl")
			writeDeleteFixture(t, main, "selected")
			cwd := cfg.WorkingDirectory
			if overlap == "cwd inside sidecar" {
				cwd = filepath.Join(project, thread.ID, "actual-project")
				if err := os.MkdirAll(cwd, 0o700); err != nil {
					t.Fatal(err)
				}
				r.server.mu.Lock()
				r.server.threads[thread.ID].CWD = cwd
				r.server.mu.Unlock()
			}
			sentinel := filepath.Join(cwd, "project-sentinel")
			writeDeleteFixture(t, sentinel, "project data")
			err := r.DeleteThread(context.Background(), thread.ID)
			if overlap == "artifact inside cwd" {
				if err != nil {
					t.Fatal("requested metadata below an ancestor CWD was blocked", err)
				}
				existsDeleteFixture(t, main, false)
			} else {
				deletionRPCCode(t, err, -32010)
				existsDeleteFixture(t, main, true)
			}
			existsDeleteFixture(t, sentinel, true)
		})
	}
}

func TestClaudeDeletionPendingIntentRecoversAndFilesystemFailuresStayLocal(t *testing.T) {
	cfg, native := deleteTestConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	other := startThread(t, r)
	project := filepath.Join(native, "projects", "project")
	writeDeleteFixture(t, filepath.Join(project, thread.ID+".jsonl"), "history")
	sidecar := filepath.Join(project, thread.ID)
	outside := t.TempDir()
	if err := os.Symlink(outside, sidecar); err != nil {
		t.Fatal(err)
	}
	deletionRPCCode(t, r.DeleteThread(context.Background(), thread.ID), -32010)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg) // Failed recovery of one target must not stop runtime.
	if _, err := r2.ReadThread(context.Background(), other.ID, false); err != nil {
		t.Fatal("unrelated session blocked by failed deletion", err)
	}
	if _, err := r2.ResumeThread(context.Background(), thread.ID, codexadapter.ThreadOptions{}); err == nil || !strings.Contains(err.Error(), "Retry deleting") {
		t.Fatal("pending deletion was not fenced with actionable failure", err)
	}
	deletionRPCCode(t, r2.DeleteThread(context.Background(), thread.ID), -32010)
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	writeDeleteFixture(t, filepath.Join(sidecar, "subagents", "child.jsonl"), "child")
	if err := r2.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal("corrected filesystem did not permit retry", err)
	}
	existsDeleteFixture(t, filepath.Join(project, thread.ID+".jsonl"), false)
	existsDeleteFixture(t, sidecar, false)
}

func TestClaudeDeletionPermissionFailureIsRetryableAndDoesNotBlockOtherSessions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	cfg, native := deleteTestConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	other := startThread(t, r)
	project := filepath.Join(native, "projects", "project")
	main := filepath.Join(project, thread.ID+".jsonl")
	writeDeleteFixture(t, main, "private native history")
	if err := os.Chmod(project, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(project, 0o700) })
	deletionRPCCode(t, r.DeleteThread(context.Background(), thread.ID), -32010)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	if _, err := r2.ReadThread(context.Background(), other.ID, false); err != nil {
		t.Fatal("unrelated session unavailable after deletion permission failure", err)
	}
	if err := os.Chmod(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r2.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	existsDeleteFixture(t, main, false)
}

func TestClaudeDeletionReplaysIntentAfterCrashBeforeAndDuringCleanup(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "before cleanup", true: "partial cleanup"}[partial], func(t *testing.T) {
			cfg, native := deleteTestConfig(t)
			r := startTest(t, cfg)
			thread := startThread(t, r)
			project := filepath.Join(native, "projects", "project")
			main := filepath.Join(project, thread.ID+".jsonl")
			child := filepath.Join(project, thread.ID, "tool-results", "output")
			writeDeleteFixture(t, main, "history")
			writeDeleteFixture(t, child, "result")
			r.server.mu.Lock()
			_, _, err := r.server.beginDeletion(thread.ID)
			r.server.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if partial {
				if err := os.Remove(main); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(cfg.StateDirectory, thread.ID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r2 := startTest(t, cfg)
			existsDeleteFixture(t, main, false)
			existsDeleteFixture(t, child, false)
			existsDeleteFixture(t, filepath.Join(cfg.StateDirectory, thread.ID+".json"), false)
			if err := r2.DeleteThread(context.Background(), thread.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClaudeDeletionUsesRecordedLaunchConfigRootAfterSettingsChange(t *testing.T) {
	cfg, native := deleteTestConfig(t)
	settings := LaunchSettings{}
	cfg.LoadSettings = func() (LaunchSettings, error) { return settings, nil }
	r := startTest(t, cfg)
	thread := startThread(t, r)
	runTurn(t, r, thread.ID, "launch records actual root")
	old := filepath.Join(native, "projects", "project", thread.ID+".jsonl")
	writeDeleteFixture(t, old, "old real session")
	newRoot := t.TempDir()
	unlaunched := filepath.Join(newRoot, "projects", "project", thread.ID+".jsonl")
	writeDeleteFixture(t, unlaunched, "not launched here")
	if err := r.WithIdleSettingsUpdate(func() error { settings.Env = map[string]string{"CLAUDE_CONFIG_DIR": newRoot}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	existsDeleteFixture(t, old, false)
	existsDeleteFixture(t, unlaunched, true)
}

func TestClaudeDeletionDetectsOtherLiveWriterButIgnoresDeadRegistryRecords(t *testing.T) {
	cfg, native := deleteTestConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	main := filepath.Join(native, "projects", "project", thread.ID+".jsonl")
	writeDeleteFixture(t, main, "history")
	registry := filepath.Join(native, "sessions")
	live := filepath.Join(registry, "live.json")
	record, _ := json.Marshal(map[string]any{"sessionId": thread.ID, "pid": os.Getpid(), "status": "idle", "peerToken": "DO_NOT_EXPOSE"})
	writeDeleteFixture(t, live, string(record))
	err := r.DeleteThread(context.Background(), thread.ID)
	deletionRPCCode(t, err, -32010)
	if strings.Contains(err.Error(), "DO_NOT_EXPOSE") {
		t.Fatal("registry credential entered deletion error")
	}
	existsDeleteFixture(t, main, true)
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(map[string]any{"sessionId": thread.ID, "pid": 99999999, "status": "busy"})
	writeDeleteFixture(t, filepath.Join(registry, "stale.json"), string(stale))
	unrelated, _ := json.Marshal(map[string]any{"sessionId": uuid.NewString(), "pid": os.Getpid(), "status": "running"})
	writeDeleteFixture(t, filepath.Join(registry, "unrelated.json"), string(unrelated))
	if err := r.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal("stale or unrelated native records blocked deletion", err)
	}
	existsDeleteFixture(t, main, false)
	existsDeleteFixture(t, filepath.Join(registry, "stale.json"), true)
	existsDeleteFixture(t, filepath.Join(registry, "unrelated.json"), true)
}

func TestNativeConfigDirectoryUsesCompleteChildEnvironment(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	root := t.TempDir()
	for _, test := range []struct {
		env      []string
		override map[string]string
		want     string
	}{{[]string{"HOME=" + home, "CLAUDE_CONFIG_DIR="}, nil, filepath.Join(home, ".claude")}, {[]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=wrong"}, map[string]string{"CLAUDE_CONFIG_DIR": root}, root}, {[]string{"HOME=" + home, "CLAUDE_CONFIG_DIR=relative"}, nil, filepath.Join(cwd, "relative")}} {
		env, err := childEnvironment(test.env, test.override)
		if err != nil {
			t.Fatal(err)
		}
		got, err := nativeConfigDirectory(env, cwd)
		if err != nil || got != test.want {
			t.Fatalf("native root=%q want=%q error=%v", got, test.want, err)
		}
	}
}

func TestClaudeDeletionRefusesSymlinkReplacementOfRecordedRootAncestor(t *testing.T) {
	cfg, _ := deleteTestConfig(t)
	storage := t.TempDir()
	native := filepath.Join(storage, "config")
	cfg.Env = append(cfg.Env, "CLAUDE_CONFIG_DIR="+native)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	runTurn(t, r, thread.ID, "records canonical native root")
	old := filepath.Join(native, "projects", "project", thread.ID+".jsonl")
	writeDeleteFixture(t, old, "owned")
	outside := t.TempDir()
	foreign := filepath.Join(outside, "config", "projects", "project", thread.ID+".jsonl")
	writeDeleteFixture(t, foreign, "KEEP OUTSIDE")
	moved := storage + "-moved"
	if err := os.Rename(storage, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(storage); _ = os.Rename(moved, storage) })
	if err := os.Symlink(outside, storage); err != nil {
		t.Fatal(err)
	}
	deletionRPCCode(t, r.DeleteThread(context.Background(), thread.ID), -32010)
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "KEEP OUTSIDE" {
		t.Fatal("ancestor symlink redirected native deletion")
	}
	existsDeleteFixture(t, filepath.Join(moved, "config", "projects", "project", thread.ID+".jsonl"), true)
}
