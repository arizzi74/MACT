package admin

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUISessionActionsRequireAuthenticationOriginCSRFAndConfirmation(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"sessions/edit", "sessions/archive", "session-archives", "sessions/restore"} {
		t.Run(endpoint, func(t *testing.T) {
			body := map[string]any{"request_id": uuid.NewString()}
			if endpoint == "sessions/edit" || endpoint == "sessions/archive" {
				body["session_id"], body["confirmed"] = uuid.NewString(), true
				if endpoint == "sessions/edit" {
					body["name"] = "Project / title"
				} else {
					body["cwd"], body["delete_project"] = "/work/project", false
				}
			} else {
				body["worker_id"], body["runtime_id"], body["runtime_generation"] = uuid.NewString(), uuid.NewString(), 1
				if endpoint == "sessions/restore" {
					body["archive_id"], body["confirmed"] = "project.tar.xz", true
				}
			}
			raw, _ := json.Marshal(body)
			for _, tc := range []struct {
				name, method, token, origin, header, cookie string
				status                                      int
			}{
				{"missing auth", "POST", "", passkeyTestOrigin, "csrf", "csrf", 401},
				{"invalid auth", "POST", "invalid", passkeyTestOrigin, "csrf", "csrf", 401},
				{"missing origin", "POST", token, "", "csrf", "csrf", 403},
				{"wrong origin", "POST", token, "https://evil.example", "csrf", "csrf", 403},
				{"wrong port", "POST", token, passkeyTestOrigin + ":8443", "csrf", "csrf", 403},
				{"missing CSRF header", "POST", token, passkeyTestOrigin, "", "csrf", 403},
				{"missing CSRF cookie", "POST", token, passkeyTestOrigin, "csrf", "", 403},
				{"wrong CSRF", "POST", token, passkeyTestOrigin, "wrong", "csrf", 403},
				{"unknown target", "POST", token, passkeyTestOrigin, "csrf", "csrf", 404},
				{"status needs auth", "GET", "", "", "", "", 401},
				{"wrong method", "DELETE", token, passkeyTestOrigin, "csrf", "csrf", 405},
			} {
				t.Run(tc.name, func(t *testing.T) {
					response := webUICommandRequestForTest(server, tc.method, "/tgw/api/v1/webui/"+endpoint, string(raw), tc.token, tc.origin, tc.header, tc.cookie)
					if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
					}
				})
			}
			if endpoint != "session-archives" {
				delete(body, "confirmed")
				raw, _ = json.Marshal(body)
				response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/"+endpoint, string(raw), token, passkeyTestOrigin, "csrf", "csrf")
				if response.Code != 400 {
					t.Fatalf("missing confirmation: %d %s", response.Code, response.Body.String())
				}
				body["confirmed"] = true
			}
			body["untrusted_target_path"] = "/private/other-project"
			raw, _ = json.Marshal(body)
			if response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/"+endpoint, string(raw), token, passkeyTestOrigin, "csrf", "csrf"); response.Code != 400 {
				t.Fatalf("unknown target field: %d %s", response.Code, response.Body.String())
			}
		})
	}
	commands, err := store.PendingCommands(t.Context(), 20)
	if err != nil || len(commands) != 0 {
		t.Fatalf("rejected requests queued commands: %+v %v", commands, err)
	}
}

type webUISessionActionFixture struct {
	store                 *registry.Store
	server                *Server
	token                 string
	worker, runtime, conn uuid.UUID
	sequence              uint64
	sessions              []protocol.Session
}

func newWebUISessionActionFixture(t *testing.T) *webUISessionActionFixture {
	t.Helper()
	ctx := t.Context()
	store := adminIntegrationStore(t)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	workerToken := "archive-test-worker-token"
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken(workerToken)})
	if err != nil {
		t.Fatal(err)
	}
	f := &webUISessionActionFixture{store: store, server: server, token: webuiTestLogin(t, store), worker: worker.ID, runtime: uuid.New(), conn: uuid.New()}
	hello := protocol.Hello{WorkerID: f.worker.String(), WorkerName: "Worker", OS: "linux", Arch: "arm64", ProtocolMin: 1, ProtocolMax: 1, SupportsSessionEditing: true, SupportsSessionArchives: true,
		Runtimes: []protocol.Runtime{{ID: f.runtime.String(), WorkerID: f.worker.String(), Name: "Runtime", ProfileID: "main", Generation: 7, State: "running"}}}
	if _, err := store.RegisterConnection(ctx, workerToken, f.conn, hello); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		session := protocol.Session{ID: uuid.NewString(), WorkerID: f.worker.String(), RuntimeID: f.runtime.String(), ThreadID: "thread-" + name, Name: name, CWD: "/work/" + name, State: "idle", UpdatedAt: time.Now().UTC()}
		f.sessions = append(f.sessions, session)
		f.ingest(t, "session_discovered", session.ID, session)
	}
	return f
}

func (f *webUISessionActionFixture) ingest(t *testing.T, kind, sessionID string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	f.sequence++
	event := protocol.Event{ID: uuid.NewString(), Seq: f.sequence, WorkerID: f.worker.String(), RuntimeID: f.runtime.String(), RuntimeGeneration: 7, SessionID: sessionID, Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}
	if err := f.store.IngestEvent(t.Context(), f.worker, f.conn, event); err != nil {
		t.Fatalf("ingest %s: %v", kind, err)
	}
}

func (f *webUISessionActionFixture) post(t *testing.T, endpoint string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return webUICommandRequestForTest(f.server, "POST", "/tgw/api/v1/webui/"+endpoint, string(raw), f.token, passkeyTestOrigin, "csrf", "csrf")
}

func (f *webUISessionActionFixture) get(endpoint, query string) *httptest.ResponseRecorder {
	return webUICommandRequestForTest(f.server, "GET", "/tgw/api/v1/webui/"+endpoint+"?"+query, "", f.token, "", "", "")
}

func TestWebUISessionEditAndArchiveFreezeExactTargetAndRecoverDurably(t *testing.T) {
	for _, endpoint := range []string{"sessions/edit", "sessions/archive"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newWebUISessionActionFixture(t)
			session, other := f.sessions[1], f.sessions[0]
			requestID := uuid.NewString()
			body := map[string]any{"session_id": session.ID, "request_id": requestID, "confirmed": true}
			op := protocol.RenameSession
			if endpoint == "sessions/edit" {
				body["name"] = "Renamed / project 😀"
			} else {
				op = protocol.ArchiveSession
				body["cwd"], body["delete_project"] = session.CWD, true
			}
			for range 2 {
				response := f.post(t, endpoint, body)
				var result registry.WebUISessionAction
				if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Pending || result.CommandID != requestID || result.SessionID != session.ID || result.RuntimeGeneration != 7 {
					t.Fatalf("queue: %d %s", response.Code, response.Body.String())
				}
			}
			commands, err := f.store.PendingCommandsForWorker(t.Context(), f.worker, 20)
			if err != nil || len(commands) != 1 {
				t.Fatalf("duplicate command: %+v %v", commands, err)
			}
			command := commands[0]
			if command.ID != requestID || command.Operation != op || command.SessionID != session.ID || command.ThreadID != session.ThreadID || command.Arguments.CWD != session.CWD || command.RuntimeID != f.runtime.String() || command.RuntimeGeneration != 7 {
				t.Fatalf("mutable/wrong session target: %+v", command)
			}
			if endpoint == "sessions/edit" && command.Arguments.SessionName != body["name"] {
				t.Fatalf("session title normalized as a folder: %+v", command.Arguments)
			}
			if endpoint == "sessions/archive" && (command.Arguments.SessionArchive == nil || !command.Arguments.SessionArchive.DeleteProject) {
				t.Fatalf("explicit cleanup choice lost: %+v", command.Arguments)
			}
			for _, parameter := range []string{"request_id", "command_id"} {
				response := f.get(endpoint, "session_id="+session.ID+"&"+parameter+"="+requestID)
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"pending":true`) {
					t.Fatalf("read-only recovery: %d %s", response.Code, response.Body.String())
				}
			}
			if response := f.get(endpoint, "session_id="+other.ID+"&request_id="+requestID); response.Code != 404 {
				t.Fatalf("status escaped exact session: %d %s", response.Code, response.Body.String())
			}
			if response := f.get("sessions/delete", "session_id="+session.ID+"&request_id="+requestID); response.Code != 404 {
				t.Fatalf("status escaped operation: %d %s", response.Code, response.Body.String())
			}
			if response := f.get(endpoint, "session_id="+session.ID+"&request_id="+requestID+"&command_id="+uuid.NewString()); response.Code != 400 {
				t.Fatalf("conflicting request IDs accepted: %d", response.Code)
			}
			if endpoint == "sessions/edit" {
				body["name"] = "Different title"
			} else {
				body["delete_project"] = false
			}
			if response := f.post(t, endpoint, body); response.Code != 409 {
				t.Fatalf("request ID reused with changed action: %d %s", response.Code, response.Body.String())
			}
			result := protocol.Result{CommandID: requestID, State: "completed", Session: &session}
			if endpoint == "sessions/edit" {
				session.Name = "Renamed / project 😀"
			} else {
				session.Archived = true
				entry := webUITestArchiveEntry()
				entry.OriginalPath = session.CWD
				result.SessionArchive = &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: "/home/demo/CODEX_Archived", Archive: entry, WarningCode: "project_cleanup_failed", RetainedProjectPath: "/work/.codex-archived-retained"}
			}
			f.ingest(t, "command_completed", session.ID, result)
			response := f.get(endpoint, "session_id="+session.ID+"&request_id="+requestID)
			var confirmed registry.WebUISessionAction
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &confirmed) != nil || confirmed.Pending || confirmed.Status != "completed" || confirmed.Session == nil || confirmed.Session.ID != session.ID || confirmed.Session.CWD != session.CWD {
				t.Fatalf("durable outcome: %d %s", response.Code, response.Body.String())
			}
			if endpoint == "sessions/archive" && (!confirmed.Session.Archived || confirmed.ArchiveDirectory != "/home/demo/CODEX_Archived" || confirmed.Archive == nil) {
				t.Fatalf("archive outcome omitted: %+v", confirmed)
			}
			if endpoint == "sessions/archive" && (confirmed.WarningCode != "project_cleanup_failed" || confirmed.ProjectDeleted || confirmed.RetainedProjectPath != "/work/.codex-archived-retained" || !strings.Contains(confirmed.Message, "Retained project files: /work/.codex-archived-retained")) {
				t.Fatalf("archive cleanup warning lost: %+v", confirmed)
			}
			if err := f.store.RevokeAdminSession(t.Context(), f.token); err != nil {
				t.Fatal(err)
			}
			if response := f.get(endpoint, "session_id="+session.ID+"&request_id="+requestID); response.Code != 401 {
				t.Fatalf("revoked login read outcome: %d", response.Code)
			}
		})
	}
}

func webUITestArchiveEntry() *protocol.SessionArchiveEntry {
	return &protocol.SessionArchiveEntry{ID: "project-20261018.tar.xz", Name: "project-20261018.tar.xz", SessionName: "Archived project", OriginalPath: "/work/project", CreatedAt: time.Now().UTC(), Bytes: 2048, CodexVersion: "0.100.0"}
}

func TestWebUISessionArchiveRejectsMissingAndStaleProjectSnapshot(t *testing.T) {
	f := newWebUISessionActionFixture(t)
	for _, cwd := range []string{"", "/work/previous-project"} {
		response := f.post(t, "sessions/archive", map[string]any{"session_id": f.sessions[0].ID, "request_id": uuid.NewString(), "confirmed": true, "delete_project": true, "cwd": cwd})
		if response.Code != 409 || !strings.Contains(response.Body.String(), `"error_code":"invalid_workspace"`) {
			t.Fatalf("unreviewed cleanup path accepted: %d %s", response.Code, response.Body.String())
		}
	}
	commands, err := f.store.PendingCommandsForWorker(t.Context(), f.worker, 20)
	if err != nil || len(commands) != 0 {
		t.Fatalf("stale project queued work: %+v %v", commands, err)
	}
}

func TestWebUIArchiveListAndRestorePinRuntimeAndScopeStatus(t *testing.T) {
	for _, endpoint := range []string{"session-archives", "sessions/restore"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newWebUISessionActionFixture(t)
			requestID := uuid.NewString()
			body := map[string]any{"worker_id": f.worker.String(), "runtime_id": f.runtime.String(), "runtime_generation": 7, "request_id": requestID}
			op := protocol.ListSessionArchives
			if endpoint == "sessions/restore" {
				op = protocol.RestoreSession
				body["archive_id"], body["confirmed"] = webUITestArchiveEntry().ID, true
			}
			for range 2 {
				if response := f.post(t, endpoint, body); response.Code != 202 {
					t.Fatalf("queue: %d %s", response.Code, response.Body.String())
				}
			}
			commands, err := f.store.PendingCommandsForWorker(t.Context(), f.worker, 20)
			if err != nil || len(commands) != 1 || commands[0].Operation != op || commands[0].SessionID != "" || commands[0].RuntimeID != f.runtime.String() || commands[0].RuntimeGeneration != 7 || commands[0].Arguments.SessionArchive == nil {
				t.Fatalf("unfrozen or duplicated runtime request: %+v %v", commands, err)
			}
			query := "worker_id=" + f.worker.String() + "&request_id=" + requestID
			if response := f.get(endpoint, query); response.Code != 200 {
				t.Fatalf("read-only recovery: %d %s", response.Code, response.Body.String())
			}
			if response := f.get(endpoint, "worker_id="+uuid.NewString()+"&request_id="+requestID); response.Code != 404 {
				t.Fatalf("status escaped worker: %d %s", response.Code, response.Body.String())
			}
			other := "sessions/restore"
			if endpoint == other {
				other = "session-archives"
			}
			if response := f.get(other, query); response.Code != 404 {
				t.Fatalf("status escaped archive operation: %d %s", response.Code, response.Body.String())
			}
			body["runtime_generation"] = 8
			if response := f.post(t, endpoint, body); response.Code != 409 {
				t.Fatalf("idempotency changed pinned runtime: %d %s", response.Code, response.Body.String())
			}
			body["request_id"] = uuid.NewString()
			if response := f.post(t, endpoint, body); response.Code != 409 {
				t.Fatalf("new request accepted stale runtime: %d %s", response.Code, response.Body.String())
			}
			progress := protocol.Result{CommandID: requestID, State: "running", SessionArchive: &protocol.SessionArchiveResult{Phase: "verifying", ArchiveDirectory: "/home/demo/CODEX_Archived"}}
			f.ingest(t, "session_archive_progress", "", progress)
			var current registry.WebUISessionAction
			response := f.get(endpoint, query)
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &current) != nil || !current.Pending || current.Phase != "verifying" || current.ArchiveDirectory != "/home/demo/CODEX_Archived" {
				t.Fatalf("durable progress: %d %s", response.Code, response.Body.String())
			}
			result := protocol.Result{CommandID: requestID, State: "completed", SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: "/home/demo/CODEX_Archived"}}
			if endpoint == "session-archives" {
				next := 1
				result.SessionArchive.Archives = []protocol.SessionArchiveEntry{*webUITestArchiveEntry()}
				result.SessionArchive.NextOffset = &next
			} else {
				session := f.sessions[0]
				session.ID, session.ThreadID, session.Name = uuid.NewString(), "restored-thread", "Restored project"
				session.CWD = webUITestArchiveEntry().OriginalPath
				result.Session, result.SessionArchive.Archive = &session, webUITestArchiveEntry()
			}
			f.ingest(t, "command_completed", "", result)
			response = f.get(endpoint, query)
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &current) != nil || current.Pending || current.Status != "completed" || current.ArchiveDirectory != "/home/demo/CODEX_Archived" {
				t.Fatalf("durable result: %d %s", response.Code, response.Body.String())
			}
			if endpoint == "session-archives" && (len(current.Archives) != 1 || current.NextOffset == nil || *current.NextOffset != 1) {
				t.Fatalf("archive list metadata/pagination omitted: %+v", current)
			}
			if endpoint == "sessions/restore" && (current.Session == nil || current.Session.ID != result.Session.ID || current.Archive == nil || current.Archive.ID != body["archive_id"]) {
				t.Fatalf("restore result omitted: %+v", current)
			}
		})
	}
}
