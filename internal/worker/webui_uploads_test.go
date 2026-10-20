package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func TestWebUIUploadRequiresResumeAndCannotBeCompletedByNativeReply(t *testing.T) {
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "report.bin", Data: []byte("private bytes")}
	raw, _ := json.Marshal(map[string]any{"id": 7, "method": "gateway/upload", "params": upload})
	r := testWebUIRelay()
	if _, err := r.clientMessage(raw); err == nil {
		t.Fatal("upload accepted before native resume validation")
	}
	r.resumed = true
	forward, err := r.clientMessage(raw)
	if err != nil || !bytes.Equal(forward, raw) {
		// JSON field order is not part of the contract.
		if err != nil || !webUIEqualJSON(forward, raw) {
			t.Fatalf("typed upload bytes changed: %s, %v", forward, err)
		}
	}
	if data, err := r.serverMessage([]byte(`{"id":7,"result":{"path":"/tmp/spoof","text":"saved"}}`)); err != nil || len(data) != 0 || r.pending["n:7"] != "gateway/upload" {
		t.Fatal("native response completed the local upload")
	}
	delete(r.pending, "n:7")
	if _, err := r.clientMessage(raw); err != nil {
		t.Fatalf("same upload could not be retried after its reply: %v", err)
	}
}

func TestWebUIUploadUsesPrivateDurableLedgerAndFrozenIdentity(t *testing.T) {
	store, workerID, database := inboundTestStore(t)
	cwd := t.TempDir()
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 3}
	session := protocol.Session{ID: uuid.NewString(), WorkerID: workerID, RuntimeID: runtime.ID, ThreadID: "web-upload-thread", CWD: cwd}
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "private report?.bin", Data: []byte{0, 255, 1, 2, 3}}
	path, err := storeWebUIUpload(store, runtime, session, upload, []string{cwd})
	if err != nil || path != filepath.Join(cwd, "inbound", protocol.InboundFilename(upload.Name, upload.UploadID)) {
		t.Fatalf("upload path = %q, err=%v", path, err)
	}
	before, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != 0o600 {
		t.Fatal("saved upload is not a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, upload.Data) {
		t.Fatal("upload bytes changed")
	}
	for _, kind := range []string{"name", "data", "session", "thread", "runtime", "generation", "cwd"} {
		r, s, u := runtime, session, upload
		switch kind {
		case "name":
			u.Name = "private report*.bin" // same sanitized destination
		case "data":
			u.Data = []byte("different")
		case "session":
			s.ID = uuid.NewString()
		case "thread":
			s.ThreadID = "another-thread"
		case "runtime":
			r.ID = uuid.NewString()
		case "generation":
			r.Generation++
		case "cwd":
			s.CWD = t.TempDir()
		}
		if _, err := storeWebUIUpload(store, r, s, u, []string{s.CWD}); err == nil {
			t.Errorf("upload ID reused with changed %s", kind)
		}
		if kind == "cwd" {
			if _, err := os.Lstat(filepath.Join(s.CWD, "inbound")); !os.IsNotExist(err) {
				t.Fatal("rejected scope created an inbound directory")
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(database, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retry, err := storeWebUIUpload(reopened, runtime, session, upload, []string{cwd})
	after, _ := os.Stat(path)
	if err != nil || retry != path || !os.SameFile(before, after) {
		t.Fatal("reconnect/restart retry replaced the saved file")
	}
	if record, found, err := reopened.LoadCommand(upload.UploadID); err != nil || found || record.InboundFile != nil {
		t.Fatal("browser upload entered the command ledger")
	}
	if pending, err := reopened.PendingCommands(); err != nil || len(pending) != 0 {
		t.Fatal("browser upload became replayable work")
	}
	if events, err := reopened.OutboxAfter(0); err != nil || len(events) != 0 {
		t.Fatal("browser upload leaked into the Telegram outbox")
	}
}

func TestWebUIUploadRecoversPublicationBeforeSaveAcknowledgment(t *testing.T) {
	store, workerID, _ := inboundTestStore(t)
	cwd := t.TempDir()
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1}
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "upload-recovery", CWD: cwd}
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "empty.txt", Data: []byte{}}
	path, err := storeWebUIUpload(store, runtime, session, upload, []string{cwd})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate loss of the process after its durable inode metadata, before
	// linking the final name or recording the save acknowledgment.
	if err := store.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketWebUIUploads)
		var record webUIUploadRecord
		if err := json.Unmarshal(bucket.Get([]byte(upload.UploadID)), &record); err != nil {
			return err
		}
		record.File.Published = false
		if err := os.Link(path, filepath.Join(cwd, "inbound", record.File.Temporary)); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		encoded, _ := json.Marshal(record)
		return bucket.Put([]byte(upload.UploadID), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	if retry, err := storeWebUIUpload(store, runtime, session, upload, []string{cwd}); err != nil || retry != path {
		t.Fatalf("cannot recover durable pending publication: %q %v", retry, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 || !singleLinkedWebUIFile(info) {
		t.Fatal("recovered empty file has an unsafe inode")
	}
}

func TestWebUIUploadActorSavesWithoutStartingOrSteeringCodex(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "upload-active", "active-turn")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "active"}}, nil)
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "report.pdf", Data: []byte("exact bytes")}
	result, err := a.saveWebUIUpload(t.Context(), runtime, session, upload)
	if err != nil || result.UploadID != upload.UploadID || result.Text != "File saved in "+result.Path {
		t.Fatalf("save-only upload = %#v %v", result, err)
	}
	if !hasCall(server.Calls(), "thread/read") || hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/steer") {
		t.Fatal("file-only upload prompted Codex or skipped live metadata validation")
	}
	if events, err := a.store.OutboxAfter(0); err != nil || len(events) != 0 {
		t.Fatal("file-only upload emitted a Telegram result")
	}
}

func TestWebUIUploadActorFencesSessionWorkspaceRuntimeAndAdmission(t *testing.T) {
	for _, scenario := range []string{"runtime", "worker", "thread", "frozen-cwd", "native-cwd", "helper", "archived", "deleted", "update", "archive"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "upload-fenced", "")
			originalCWD := session.CWD
			thread := map[string]any{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}
			switch scenario {
			case "runtime":
				runtime.Generation++
			case "worker":
				session.WorkerID = uuid.NewString()
			case "thread":
				session.ThreadID = "other"
			case "frozen-cwd":
				session.CWD = t.TempDir()
			case "native-cwd":
				thread["cwd"] = t.TempDir()
			case "helper":
				thread["source"] = "subAgent"
			case "archived", "deleted":
				changed := session
				changed.Archived = scenario == "archived"
				changed.Deleted = scenario == "deleted"
				a.mu.RLock()
				actor := a.sessions[session.ID]
				a.mu.RUnlock()
				done := make(chan struct{})
				actor.snapshots <- actorSnapshot{runtime: runtime, session: changed, processed: done}
				<-done
			case "update":
				a.updateMu.Lock()
				a.update = &updateState{}
				a.updateMu.Unlock()
			case "archive":
				a.updateMu.Lock()
				a.archiveAdmissions = map[string]*archiveAdmission{"test": {Project: session.CWD}}
				a.updateMu.Unlock()
			}
			server.SetThreads([]map[string]any{thread}, nil)
			upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "guarded.txt", Data: []byte("data")}
			if _, err := a.saveWebUIUpload(t.Context(), runtime, session, upload); err == nil {
				t.Fatal("unsafe upload target was accepted")
			}
			if _, err := os.Lstat(filepath.Join(originalCWD, "inbound")); !os.IsNotExist(err) {
				t.Fatal("rejected upload wrote to the session workspace")
			}
			if hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/steer") {
				t.Fatal("rejected upload reached a Codex prompt")
			}
		})
	}
}

func TestWebUIUploadCancellationHonorsUpdateAdmission(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "upload-canceled", "")
	before := len(server.Calls())
	a.updateMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := a.saveWebUIUpload(ctx, runtime, session, protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "file", Data: []byte{}})
	a.updateMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || len(server.Calls()) != before {
		t.Fatal("canceled upload escaped update admission")
	}
}

func TestWebUIUploadLocalReplyPreservesExactPathAndRedactsVisibleText(t *testing.T) {
	store, workerID, _ := inboundTestStore(t)
	redactor, _ := newWorkerRedactor([]string{"private", "c2VjcmV0"})
	if err := store.configureRedactor(redactor); err != nil {
		t.Fatal(err)
	}
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1, State: "running", LocalSocket: "/tmp/native.sock"}
	session, err := store.UpsertSession(protocol.Session{WorkerID: workerID, RuntimeID: runtime.ID, CWD: t.TempDir(), ThreadID: "upload-local"})
	if err != nil {
		t.Fatal(err)
	}
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "private.txt", Data: []byte("secret")}
	paramsData, _ := json.Marshal(upload)
	var params map[string]json.RawMessage
	_ = json.Unmarshal(paramsData, &params)
	writes := make(chan outbound, 1)
	c := &Connection{store: store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}
	c.cfg.WorkerID = workerID
	c.cfg.AllowedWorkspaceRoots = []string{session.CWD}
	c.uploadWebUI = func(_ context.Context, gotRuntime protocol.Runtime, gotSession protocol.Session, got protocol.WebUIUpload) (protocol.WebUIUploadResult, error) {
		if !bytes.Equal(got.Data, upload.Data) || gotRuntime.ID != runtime.ID || gotSession.ID != session.ID {
			t.Error("file bytes or scope were redacted or changed")
		}
		path := filepath.Join(session.CWD, "inbound", protocol.InboundFilename(got.Name, got.UploadID))
		return protocol.WebUIUploadResult{UploadID: got.UploadID, Path: path, Text: "File saved in " + path}, nil
	}
	r := testWebUIRelay()
	r.pool = newWebUIRelays(c, t.Context(), writes)
	r.runtime, r.session, r.ctx, r.id = runtime, session, t.Context(), uuid.NewString()
	r.resumed = true
	done := make(chan error, 1)
	raw, _ := json.Marshal(map[string]any{"id": 1, "method": "gateway/upload", "params": params})
	// A nil native socket proves this method is fully intercepted locally.
	go func() { done <- r.forwardInput(nil, raw) }()
	output := <-writes
	frame, err := protocol.Payload[protocol.WebUIFrame](output.envelope)
	output.done <- err
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result protocol.WebUIUploadResult `json:"result"`
	}
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.Result.UploadID != upload.UploadID || !strings.Contains(response.Result.Path, "private") || strings.Contains(response.Result.Text, "private") {
		t.Fatalf("upload response path was corrupted or text unredacted: %s %v", frame.Data, err)
	}
}
