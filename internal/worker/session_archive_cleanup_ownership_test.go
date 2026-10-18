package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestArchiveProjectRemovalChecksDirectoryOwnership(t *testing.T) {
	for _, authority := range []string{"gateway", "native"} {
		for _, location := range []string{"equal", "child", "missing child", "sibling prefix", "ancestor", "alias equal", "alias child", "alias sibling", "alias ancestor"} {
			t.Run(authority+"/"+location, func(t *testing.T) {
				a, runtime, server, session, _ := archiveJobFixture(t)
				project, parent := session.CWD, filepath.Dir(session.CWD)
				cwd, blocked := project, true
				switch location {
				case "child", "alias child":
					cwd = filepath.Join(project, "nested")
				case "missing child":
					cwd = filepath.Join(project, "not-created", "nested")
				case "sibling prefix", "alias sibling":
					cwd, blocked = project+"-other", false
				case "ancestor", "alias ancestor":
					cwd, blocked = parent, false
				}
				if location != "missing child" {
					if err := os.MkdirAll(cwd, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if strings.HasPrefix(location, "alias ") {
					alias := filepath.Join(parent, "other-session-alias")
					if err := os.Symlink(cwd, alias); err != nil {
						t.Fatal(err)
					}
					cwd = alias
				}
				other := uuid.NewString()
				if authority == "gateway" {
					if _, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: other, CWD: cwd, State: "idle"}); err != nil {
						t.Fatal(err)
					}
				} else {
					server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": project, "status": "notLoaded"}, {"id": other, "cwd": cwd, "status": "notLoaded", "source": "cli"}}, nil)
				}
				err := a.verifyProjectRemoval(sessionArchiveJob{ProjectPath: project, Session: session, Directory: filepath.Join(os.Getenv("HOME"), "CODEX_Archived")}, t.TempDir(), t.TempDir())
				var refusal *protocol.Error
				if blocked {
					if !errors.As(err, &refusal) || refusal.Code != "shared_directory" {
						t.Fatalf("shared project directory removal = %v", err)
					}
				} else if err != nil {
					t.Fatalf("directory outside the removed project blocked cleanup: %v", err)
				}
			})
		}
	}
}

func TestArchiveProjectRemovalStillProtectsArchivedSessionDirectory(t *testing.T) {
	a, runtime, _, session, _ := archiveJobFixture(t)
	if _, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: session.CWD, State: "not_loaded", Archived: true}); err != nil {
		t.Fatal(err)
	}
	err := a.verifyProjectRemoval(sessionArchiveJob{ProjectPath: session.CWD, Session: session, Directory: filepath.Join(os.Getenv("HOME"), "CODEX_Archived")}, t.TempDir(), t.TempDir())
	var refusal *protocol.Error
	if !errors.As(err, &refusal) || refusal.Code != "shared_directory" {
		t.Fatalf("archived session directory lost protection: %v", err)
	}
}

func TestArchiveActiveAncestorStillBlocksProjectCleanup(t *testing.T) {
	a, runtime, server, session, _ := archiveJobFixture(t)
	ancestor, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: filepath.Dir(session.CWD), State: "running", ActiveTurnID: "ancestor-turn"})
	if err != nil {
		t.Fatal(err)
	}
	a.onSession(runtime, ancestor)
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}, {"id": ancestor.ThreadID, "cwd": ancestor.CWD, "status": "active", "turns": []map[string]any{{"id": "ancestor-turn", "status": "inProgress"}}}}, []string{ancestor.ThreadID})
	c := agentCommand(runtime, session, protocol.ArchiveSession)
	c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{DeleteProject: true}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	record := waitArchiveTerminal(t, a, c.ID)
	if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.SessionBusy || hasCall(server.Calls(), "thread/archive") {
		t.Fatalf("active ancestor escaped project idle proof: %#v", record.Result)
	}
	if _, err := os.Stat(session.CWD); err != nil {
		t.Fatalf("active ancestor project was removed: %v", err)
	}
}
