package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func emptyDiscoveryFixture(t *testing.T) (*Agent, protocol.Runtime, *codexadapter.Client, *codextest.Server, string) {
	t.Helper()
	a, runtime, _, cleanup := testAgent(t)
	t.Cleanup(cleanup)
	home := t.TempDir()
	client, server, err := codextest.New(a.ctx, codexadapter.InitializeInfo{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	a.manager.install(runtime, client)
	return a, runtime, client, server, home
}

func emptyDiscoveryRollout(t *testing.T, home, thread string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "10", "28", "rollout-2026-10-28T15-00-00-"+thread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": thread}})
	if err := os.WriteFile(path, append(header, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertEmptyDiscoveryReadsOnlyMetadata(t *testing.T, calls []codextest.Call) {
	t.Helper()
	for _, call := range calls {
		switch call.Method {
		case "initialize", "initialized", "thread/list", "thread/loaded/list":
		case "thread/read":
			var params map[string]json.RawMessage
			if json.Unmarshal(call.Params, &params) != nil || string(params["includeTurns"]) != "false" {
				t.Fatalf("recovery requested thread history: %s", call.Params)
			}
		default:
			t.Fatalf("empty discovery sent a non-metadata RPC: %s", call.Method)
		}
	}
}

func TestDiscoveryRecoversUnlistedEmptySessionAfterRestart(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("hidden=%t", hidden), func(t *testing.T) {
			a, previousRuntime, client, server, home := emptyDiscoveryFixture(t)
			previous, err := a.store.UpsertRuntimeSession(previousRuntime, protocol.Session{
				RuntimeID: previousRuntime.ID, ThreadID: uuid.NewString(), Name: "Security Assessment - SmartStage", CWD: previousRuntime.DefaultCWD,
				State: "idle", Loaded: true, Stats: &protocol.SessionStats{PromptCount: statsNumber(0), AssistantMessageCount: statsNumber(0)},
			})
			if err != nil {
				t.Fatal(err)
			}
			if hidden {
				previous, _, err = a.store.ArchiveDiscoveredSession(previousRuntime, previous)
				if err != nil {
					t.Fatal(err)
				}
			}
			sibling, err := a.store.UpsertRuntimeSession(previousRuntime, protocol.Session{
				RuntimeID: previousRuntime.ID, ThreadID: uuid.NewString(), Name: "Security Assessment - MACT", Preview: "Existing conversation",
				CWD: previous.CWD, State: "not_loaded",
			})
			if err != nil {
				t.Fatal(err)
			}
			path := emptyDiscoveryRollout(t, home, previous.ThreadID)
			if err := server.SetThreadMethodResult("thread/read", previous.ThreadID, map[string]any{"thread": map[string]any{
				"id": previous.ThreadID, "cwd": previous.CWD, "source": "appServer", "path": path, "status": "notLoaded",
			}}); err != nil {
				t.Fatal(err)
			}
			server.SetThreads([]map[string]any{{"id": sibling.ThreadID, "name": sibling.Name, "preview": sibling.Preview, "cwd": sibling.CWD, "status": "notLoaded", "source": "appServer"}}, nil)
			runtime, err := a.store.BeginRuntime(previousRuntime.ProfileID, previousRuntime.Name, previousRuntime.DefaultCWD)
			if err != nil {
				t.Fatal(err)
			}
			runtime.State = "running"
			a.manager.install(runtime, client)
			before, err := a.store.OutboxAfter(0)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := a.manager.discover(t.Context(), runtime, client); err != nil {
					t.Fatal(err)
				}
			}
			current := inventoryByThread(t, a.store, runtime)
			rescued := current[previous.ThreadID]
			if rescued.ID != previous.ID || rescued.Name != previous.Name || rescued.CWD != previous.CWD || rescued.Archived || rescued.Deleted || rescued.Loaded || rescued.State != "not_loaded" || rescued.ActiveTurnID != "" || !reflect.DeepEqual(rescued.Stats, previous.Stats) {
				t.Fatalf("empty session was not recovered in its original identity: %+v", rescued)
			}
			if !reflect.DeepEqual(current[sibling.ThreadID], sibling) {
				t.Fatalf("same-directory sibling changed: %+v -> %+v", sibling, current[sibling.ThreadID])
			}
			if a.actorForThread(runtime.ID, previous.ThreadID) == nil {
				t.Fatal("recovered cold session has no actor")
			}
			events, err := a.store.OutboxAfter(0)
			if err != nil || len(events) != len(before)+1 {
				t.Fatalf("recovery did not publish exactly one state change: %d -> %d, %v", len(before), len(events), err)
			}
			var recovered protocol.Session
			event := events[len(events)-1]
			if event.SessionID != previous.ID || event.Kind != "session_state_changed" || json.Unmarshal(event.Data, &recovered) != nil || recovered.Archived {
				t.Fatalf("invalid recovery event: %+v", event)
			}
			if a.manager.stats.bytesRead != 0 {
				t.Fatal("rescue scanned conversation statistics")
			}
			assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
		})
	}
}

func TestDiscoveryEmptySessionRecoveryRequiresActiveExactPersistence(t *testing.T) {
	for _, scenario := range []string{"archived path", "missing file", "outside file", "symlink outside", "symlink archived", "symlink sessions root", "traversal", "directory", "wrong filename", "wrong header", "wrong thread", "helper", "ephemeral", "different cwd", "native preview", "stored preview", "stored messages", "deleted", "pinned"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, client, server, home := emptyDiscoveryFixture(t)
			prior, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), Name: "Empty session", CWD: runtime.DefaultCWD, State: "not_loaded"})
			if err != nil {
				t.Fatal(err)
			}
			path := emptyDiscoveryRollout(t, home, prior.ThreadID)
			thread := map[string]any{"id": prior.ThreadID, "cwd": prior.CWD, "source": "appServer", "path": path, "status": "notLoaded"}
			switch scenario {
			case "archived path", "outside file", "symlink outside", "symlink archived", "symlink sessions root":
				destination := filepath.Join(home, "archived_sessions", filepath.Base(path))
				if scenario == "outside file" || scenario == "symlink outside" {
					destination = filepath.Join(t.TempDir(), filepath.Base(path))
				}
				if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, destination); err != nil {
					t.Fatal(err)
				}
				if scenario == "symlink sessions root" {
					if err := os.RemoveAll(filepath.Join(home, "sessions")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(home, "archived_sessions"), filepath.Join(home, "sessions")); err != nil {
						t.Fatal(err)
					}
					thread["path"] = filepath.Join(home, "sessions", filepath.Base(destination))
				} else if scenario == "symlink outside" || scenario == "symlink archived" {
					if err := os.Symlink(destination, path); err != nil {
						t.Fatal(err)
					}
				} else {
					thread["path"] = destination
				}
			case "missing file", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if scenario == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "traversal":
				thread["path"] = filepath.Join(home, "sessions") + "/../sessions/" + filepath.ToSlash(path[len(filepath.Join(home, "sessions"))+1:])
			case "wrong filename":
				other := filepath.Join(filepath.Dir(path), "rollout-2026-10-28T15-00-00-"+uuid.NewString()+".jsonl")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				thread["path"] = other
			case "wrong header":
				if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"other"}}`+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong thread":
				thread["id"] = uuid.NewString()
			case "helper":
				thread["source"] = map[string]any{"subAgent": "review"}
			case "ephemeral":
				thread["ephemeral"] = true
			case "different cwd":
				cwd := filepath.Join(prior.CWD, "other")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				thread["cwd"] = cwd
			case "native preview":
				thread["preview"] = "A user message"
				prior, _, err = a.store.ArchiveDiscoveredSession(runtime, prior)
			case "stored preview", "stored messages":
				if scenario == "stored preview" {
					prior.Preview = "A user message"
				} else {
					prior.Stats = &protocol.SessionStats{PromptCount: statsNumber(1)}
				}
				prior, err = a.store.UpsertRuntimeSession(runtime, prior)
			case "deleted":
				prior, err = a.store.DeleteSession(runtime, prior)
			case "pinned":
				prior, err = a.store.ConfirmArchiveVisibility(runtime, prior, "session.tar.xz", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := server.SetThreadMethodResult("thread/read", prior.ThreadID, map[string]any{"thread": thread}); err != nil {
				t.Fatal(err)
			}
			server.SetThreads(nil, nil)
			if err := a.manager.discover(t.Context(), runtime, client); err != nil {
				t.Fatal(err)
			}
			current := inventoryByThread(t, a.store, runtime)[prior.ThreadID]
			if current.ID != prior.ID || !current.Archived || current.Deleted != prior.Deleted || a.actorForThread(runtime.ID, prior.ThreadID) != nil {
				t.Fatalf("ineligible empty session became selectable: %+v", current)
			}
			if (scenario == "stored preview" || scenario == "stored messages" || scenario == "deleted") && countCall(server.Calls(), "thread/read") != 0 {
				t.Fatal("recovery probed a nonempty or deleted identity")
			}
			assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
		})
	}
}

func TestDiscoveryEmptyRecoveryBoundsAndRotatesTransientProbes(t *testing.T) {
	a, runtime, client, server, _ := emptyDiscoveryFixture(t)
	for index := range emptyDiscoveryLimit + 5 {
		prior, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, State: "not_loaded"})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if _, _, err := a.store.ArchiveDiscoveredSession(runtime, prior); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := inventoryByThread(t, a.store, runtime)
	server.SetRPCError("thread/read", -32000, "temporary metadata failure")
	for attempt := range 2 {
		if err := a.manager.discover(t.Context(), runtime, client); err != nil {
			t.Fatal(err)
		}
		if countCall(server.Calls(), "thread/read") != (attempt+1)*emptyDiscoveryLimit {
			t.Fatal("empty-session probe budget was not enforced")
		}
	}
	seen := map[string]bool{}
	for _, call := range server.Calls() {
		if call.Method == "thread/read" {
			var params struct {
				ThreadID string `json:"threadId"`
			}
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			seen[params.ThreadID] = true
		}
	}
	if len(seen) != len(before) {
		t.Fatalf("bounded probes starved candidates: %d/%d", len(seen), len(before))
	}
	if after := inventoryByThread(t, a.store, runtime); !reflect.DeepEqual(before, after) {
		t.Fatal("a cap or transient metadata failure changed inventory")
	}
	assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
}

func TestDiscoveryEmptyRecoveryReadDeadlineKeepsVisibleSession(t *testing.T) {
	a, runtime, client, server, _ := emptyDiscoveryFixture(t)
	prior, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, State: "not_loaded"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetResponseDelay("thread/read", emptyDiscoveryReadTimeout+50*time.Millisecond)
	started := time.Now()
	if err := a.manager.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > emptyDiscoveryReadTimeout+500*time.Millisecond {
		t.Fatalf("metadata probe exceeded its deadline: %v", elapsed)
	}
	if current := inventoryByThread(t, a.store, runtime)[prior.ThreadID]; !reflect.DeepEqual(current, prior) {
		t.Fatalf("timed out metadata hid a visible session: %+v", current)
	}
	assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
}

func TestDiscoveryEmptyRecoveryKeepsConcurrentFirstPrompt(t *testing.T) {
	a, runtime, client, server, home := emptyDiscoveryFixture(t)
	prior, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, State: "running", Loaded: true, ActiveTurnID: "first-turn"})
	if err != nil {
		t.Fatal(err)
	}
	a.onSession(runtime, prior)
	path := emptyDiscoveryRollout(t, home, prior.ThreadID)
	if err := server.SetThreadMethodResult("thread/read", prior.ThreadID, map[string]any{"thread": map[string]any{
		"id": prior.ThreadID, "cwd": prior.CWD, "source": "appServer", "preview": "The first user prompt", "status": "active", "path": path,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := a.manager.discover(t.Context(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if current := inventoryByThread(t, a.store, runtime)[prior.ThreadID]; !reflect.DeepEqual(current, prior) {
		t.Fatalf("concurrent first prompt changed or hid its active session: %+v", current)
	}
	assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
}

func TestDiscoveryEmptyRecoveryWaitsForCompleteInventory(t *testing.T) {
	a, runtime, client, server, home := emptyDiscoveryFixture(t)
	prior, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, State: "not_loaded", Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	path := emptyDiscoveryRollout(t, home, prior.ThreadID)
	if err := server.SetThreadMethodResult("thread/read", prior.ThreadID, map[string]any{"thread": map[string]any{
		"id": prior.ThreadID, "cwd": prior.CWD, "source": "appServer", "status": "notLoaded", "path": path,
	}}); err != nil {
		t.Fatal(err)
	}
	threads := make([]map[string]any, discoveryLimit+1)
	outside := t.TempDir()
	for index := range threads {
		threads[index] = map[string]any{"id": fmt.Sprintf("unrelated-%d", index), "cwd": outside, "source": "cli"}
	}
	server.SetThreads(threads, nil)
	if err := a.manager.discover(t.Context(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if current := inventoryByThread(t, a.store, runtime)[prior.ThreadID]; !reflect.DeepEqual(current, prior) || countCall(server.Calls(), "thread/read") != 0 {
		t.Fatalf("incomplete inventory attempted fallback recovery: %+v", current)
	}
	assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
}

func TestDiscoveryEmptyRecoveryRetiresOnlyExactNativeNotFound(t *testing.T) {
	for _, scenario := range []string{"not found", "not loaded", "other identity", "temporary"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, client, server, _ := emptyDiscoveryFixture(t)
			prior, err := a.store.UpsertRuntimeSession(runtime, protocol.Session{RuntimeID: runtime.ID, ThreadID: uuid.NewString(), CWD: runtime.DefaultCWD, State: "not_loaded"})
			if err != nil {
				t.Fatal(err)
			}
			message := "thread not found: " + prior.ThreadID
			code := -32600
			switch scenario {
			case "not loaded":
				message = "thread not loaded: " + prior.ThreadID
			case "other identity":
				message += "-other"
			case "temporary":
				message, code = "temporary metadata failure", -32000
			}
			server.SetRPCError("thread/read", code, message)
			if err := a.manager.discover(t.Context(), runtime, client); err != nil {
				t.Fatal(err)
			}
			current := inventoryByThread(t, a.store, runtime)[prior.ThreadID]
			if current.Archived != (scenario == "not found") {
				t.Fatalf("incorrect metadata failure classification: %s: %+v", message, current)
			}
			assertEmptyDiscoveryReadsOnlyMetadata(t, server.Calls())
		})
	}
}
