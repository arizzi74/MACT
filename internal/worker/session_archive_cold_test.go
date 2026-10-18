package worker

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestArchiveColdThreadsVerifyIdleWithoutLiveTerminalRPC(t *testing.T) {
	for _, descendant := range []bool{false, true} {
		name := "selected session"
		if descendant {
			name = "historical descendant"
		}
		t.Run(name, func(t *testing.T) {
			a, runtime, server, session, manifest := archiveJobFixture(t)
			ids := []string{session.ThreadID}
			threads := []map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}}
			if descendant {
				child := uuid.NewString()
				ids = append(ids, child)
				manifest.ThreadIDs = append(manifest.ThreadIDs, child)
				threads = append(threads, map[string]any{"id": child, "cwd": session.CWD, "status": "notLoaded", "parentThreadId": session.ThreadID, "source": map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": session.ThreadID, "depth": 1}}}})
			}
			server.SetThreads(threads, nil)
			// Codex only keeps the background-terminal inventory for loaded
			// threads. Its live-only RPC rejects a persisted, unloaded thread.
			server.SetRPCError("thread/backgroundTerminals/list", -32600, "thread not found: "+session.ThreadID)
			c := agentCommand(runtime, session, protocol.ArchiveSession)
			c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{DeleteProject: true}
			if ack, err := a.HandleCommand(context.Background(), c); err != nil || ack.Status != "accepted" {
				t.Fatalf("archive admission = %#v %v", ack, err)
			}
			record := waitArchiveTerminal(t, a, c.ID)
			if record.State != CommandCompleted || record.Result == nil || record.Result.SessionArchive == nil || !record.Result.SessionArchive.ProjectDeleted {
				t.Fatalf("cold archive = %#v", record.Result)
			}
			calls := server.Calls()
			if hasCall(calls, "thread/backgroundTerminals/list") {
				t.Fatal("cold thread used the loaded-only terminal RPC")
			}
			for _, id := range ids {
				for _, method := range []string{"thread/queue/list", "thread/goal/get", "thread/read", "thread/turns/list"} {
					if updateFailedSubAgentCallCount(t, calls, method, id) == 0 {
						t.Fatalf("cold thread omitted %s verification", method)
					}
				}
			}
		})
	}
}

func TestArchiveLoadedMembershipIsSpecificToEachRuntime(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		primaryLoaded, terminal bool
	}{
		{name: "primary runtime loaded", primaryLoaded: true},
		{name: "other runtime loaded"},
		{name: "other runtime terminal", terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, runtime, primary, session, _ := archiveJobFixture(t)
			client, other, err := codextest.New(a.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(other.Close)
			otherRuntime, err := a.store.BeginRuntime("other-runtime", "Other", runtime.DefaultCWD)
			if err != nil {
				t.Fatal(err)
			}
			otherRuntime.State = "running"
			a.manager.install(otherRuntime, client)
			loadedServer, coldServer := other, primary
			if tc.primaryLoaded {
				loadedServer, coldServer = primary, other
			}
			loadedServer.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, []string{session.ThreadID})
			coldServer.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}}, nil)
			coldServer.SetRPCError("thread/backgroundTerminals/list", -32600, "thread not found: "+session.ThreadID)
			for _, server := range []*codextest.Server{primary, other} {
				if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}}); err != nil {
					t.Fatal(err)
				}
			}
			terminals := []any{}
			if tc.terminal {
				terminals = append(terminals, map[string]any{"processId": "live-process"})
			}
			if err := loadedServer.SetMethodResult("thread/backgroundTerminals/list", map[string]any{"data": terminals}); err != nil {
				t.Fatal(err)
			}
			c := agentCommand(runtime, session, protocol.ArchiveSession)
			c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
			if _, err := a.HandleCommand(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			record := waitArchiveTerminal(t, a, c.ID)
			if tc.terminal {
				if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.SessionBusy || hasCall(primary.Calls(), "thread/archive") {
					t.Fatalf("other runtime's live terminal escaped verification: %#v", record.Result)
				}
			} else if record.State != CommandCompleted {
				t.Fatalf("runtime-specific archive = %#v", record.Result)
			}
			if hasCall(coldServer.Calls(), "thread/backgroundTerminals/list") || !hasCall(loadedServer.Calls(), "thread/backgroundTerminals/list") {
				t.Fatal("terminal verification did not use this runtime's loaded membership")
			}
		})
	}
}

func TestArchiveColdThreadRequiresFreshNotLoadedProof(t *testing.T) {
	for _, status := range []string{"idle", "active", "systemError", "notLoaded with active turn"} {
		t.Run(status, func(t *testing.T) {
			a, runtime, server, session, _ := archiveJobFixture(t)
			fresh := map[string]any{"id": session.ThreadID, "cwd": session.CWD, "status": status}
			if status == "notLoaded with active turn" {
				fresh["status"] = "notLoaded"
				server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded", "turns": []map[string]any{{"id": "new-turn", "status": "inProgress"}}}}, nil)
			}
			if err := server.SetThreadMethodResult("thread/read", session.ThreadID, map[string]any{"thread": fresh}); err != nil {
				t.Fatal(err)
			}
			c := agentCommand(runtime, session, protocol.ArchiveSession)
			c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
			if _, err := a.HandleCommand(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			record := waitArchiveTerminal(t, a, c.ID)
			if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.SessionBusy || hasCall(server.Calls(), "thread/archive") {
				t.Fatalf("unproven cold thread archived: %#v", record.Result)
			}
		})
	}
}

func TestArchiveColdThreadRequiresCompleteLoadedInventory(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "incomplete inventory"
		if failed {
			name = "inventory RPC error"
		}
		t.Run(name, func(t *testing.T) {
			a, runtime, server, session, _ := archiveJobFixture(t)
			if failed {
				server.SetMethodUnavailable("thread/loaded/list", true)
			} else if err := server.SetMethodResult("thread/loaded/list", map[string]any{"data": []string{}, "nextCursor": "repeated"}); err != nil {
				t.Fatal(err)
			}
			c := agentCommand(runtime, session, protocol.ArchiveSession)
			c.Arguments.CWD, c.Arguments.SessionArchive = session.CWD, &protocol.SessionArchiveRequest{}
			if _, err := a.HandleCommand(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			record := waitArchiveTerminal(t, a, c.ID)
			if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.SessionBusy || hasCall(server.Calls(), "thread/archive") {
				t.Fatalf("unverified loaded inventory allowed archive: %#v", record.Result)
			}
		})
	}
}
