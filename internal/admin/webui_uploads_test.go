package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func webUIUploadJSON(t *testing.T, id any, upload protocol.WebUIUpload) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"id": id, "method": "gateway/upload", "params": upload})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWebUIUploadResultPreservesOnlyCorrelatedDestination(t *testing.T) {
	redactor, _ := auth.NewRedactor([]string{"secret-sentinel"}, "[REDACTED]")
	server := &Server{redactor: redactor}
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "secret-sentinel report.bin", Data: []byte{}}
	cwd, id := "/work/secret-sentinel", "request-secret-sentinel"
	expected := filepath.Join(cwd, "inbound", protocol.InboundFilename(upload.Name, upload.UploadID))
	for _, scenario := range []string{"valid", "uncorrelated", "different upload", "other workspace", "parent traversal", "nested file", "extra result field", "extra reply field", "native request", "null text", "error"} {
		t.Run(scenario, func(t *testing.T) {
			pending := make(webUIUploadRequests)
			if err := pending.remember(webUIUploadJSON(t, id, upload), cwd); err != nil {
				t.Fatal(err)
			}
			result := map[string]any{"uploadId": upload.UploadID, "path": expected, "text": "File saved in " + expected}
			reply := map[string]any{"id": id, "result": result}
			switch scenario {
			case "uncorrelated":
				reply["id"] = "different-request"
			case "different upload":
				result["uploadId"] = uuid.NewString()
			case "other workspace":
				result["path"] = "/other/secret-sentinel/inbound/" + filepath.Base(expected)
			case "parent traversal":
				result["path"] = filepath.Dir(expected) + "/../inbound/" + filepath.Base(expected)
			case "nested file":
				result["path"] = filepath.Dir(expected) + "/secret-sentinel/" + filepath.Base(expected)
			case "extra result field":
				result["untrusted"] = "secret-sentinel"
			case "extra reply field":
				reply["untrusted"] = "secret-sentinel"
			case "native request":
				reply["method"] = "item/tool/requestUserInput"
			case "null text":
				result["text"] = nil
			case "error":
				reply["error"] = map[string]any{"message": "secret-sentinel"}
			}
			raw, _ := json.Marshal(reply)
			visible, err := server.redactWebUIUploadResponse(raw, pending)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" {
				if bytes.Contains(visible, []byte("secret-sentinel")) {
					t.Fatal("uncorrelated or invalid reply bypassed redaction")
				}
				return
			}
			var decoded struct {
				ID     string                     `json:"id"`
				Result protocol.WebUIUploadResult `json:"result"`
			}
			if json.Unmarshal(visible, &decoded) != nil || decoded.ID != id || decoded.Result.UploadID != upload.UploadID || decoded.Result.Path != expected || strings.Contains(decoded.Result.Text, "secret-sentinel") || len(pending) != 0 {
				t.Fatal("validated upload reply lost its destination, identity or text redaction")
			}
			duplicate, err := server.redactWebUIUploadResponse(raw, pending)
			if err != nil || bytes.Contains(duplicate, []byte("secret-sentinel")) {
				t.Fatal("completed correlation was reused")
			}
		})
	}
}

func TestWebUIUploadCorrelationBoundsAndIdentity(t *testing.T) {
	for _, id := range []any{"request", int64(9007199254740993)} {
		pending := make(webUIUploadRequests)
		upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "file.bin", Data: []byte{}}
		if err := pending.remember(webUIUploadJSON(t, id, upload), "/work"); err != nil || len(pending) != 1 {
			t.Fatal("bounded string or exact integer request identity was rejected")
		}
		if err := pending.remember(webUIUploadJSON(t, id, upload), "/work"); err == nil || len(pending) != 1 {
			t.Fatal("duplicate pending identity was replaced")
		}
	}
	pending := make(webUIUploadRequests)
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "file.bin", Data: []byte{}}
	for i := 0; i < 64; i++ {
		if err := pending.remember(webUIUploadJSON(t, fmt.Sprintf("request-%d", i), upload), "/work"); err != nil {
			t.Fatal(err)
		}
	}
	if err := pending.remember(webUIUploadJSON(t, "overflow", upload), "/work"); err == nil || len(pending) != 64 {
		t.Fatal("upload correlation exceeded its connection quota")
	}
	for _, cwd := range []string{"", "work", "/work/../other"} {
		invalid := make(webUIUploadRequests)
		if err := invalid.remember(webUIUploadJSON(t, "request", upload), cwd); err != nil || len(invalid) != 0 {
			t.Fatal("noncanonical selected workspace created a trusted upload destination")
		}
	}
}

func newWebUIUploadFixture(t *testing.T, supported bool) *webUIFileFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	store := adminIntegrationStore(t)
	f := &webUIFileFixture{t: t, ctx: ctx, store: store, adminToken: webuiTestLogin(t, store), workerID: uuid.New(), runtimeID: uuid.New()}
	workerToken, err := auth.GenerateWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: f.workerID, Name: "Upload test worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(workerToken)}); err != nil {
		t.Fatal(err)
	}
	hub := gateway.NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Minute)
	hub.EventHandler = store.IngestEvent
	redactor, _ := auth.NewRedactor([]string{"secret-sentinel"}, "[REDACTED]")
	console, err := New(store, Config{Origin: passkeyTestOrigin, WebUI: hub, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	mux := gateway.NewMux(store, hub)
	mux.Handle("/tgw/api/v1/webui/", console)
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	f.client = passkeyHTTPClient(t, server)
	f.worker, _, err = websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/workers/connect", &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + workerToken}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.worker.CloseNow() })
	f.worker.SetReadLimit(protocol.MaxFrameBytes)
	f.send("hello", protocol.Hello{WorkerID: f.workerID.String(), WorkerName: "Upload test worker", OS: "linux", Arch: "amd64", ProtocolMin: 1, ProtocolMax: 1, SupportsWebUI: true, SupportsWebUIUploads: supported,
		Runtimes: []protocol.Runtime{{ID: f.runtimeID.String(), WorkerID: f.workerID.String(), ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}})
	ack, err := protocol.Payload[protocol.HelloAck](f.read("hello_ack"))
	if err != nil {
		t.Fatal(err)
	}
	f.connectionID = uuid.MustParse(ack.ConnectionID)
	f.session = protocol.Session{ID: uuid.NewString(), WorkerID: f.workerID.String(), RuntimeID: f.runtimeID.String(), ThreadID: "upload-thread", Name: "Upload session", CWD: "/work/secret-sentinel", State: "idle", UpdatedAt: time.Now().UTC()}
	f.ingestSession()
	return f
}

func readWebUIUploadMessage(t *testing.T, ctx context.Context, browser *websocket.Conn, wantID, wantType string) json.RawMessage {
	t.Helper()
	for {
		kind, raw, err := browser.Read(ctx)
		if err != nil {
			t.Fatalf("read browser upload response: %v", err)
		}
		var frame map[string]json.RawMessage
		if kind != websocket.MessageText || json.Unmarshal(raw, &frame) != nil {
			t.Fatal("browser returned an invalid upload response")
		}
		var frameType string
		_ = json.Unmarshal(frame["type"], &frameType)
		// Maximum-size transfer checks can cross the live connection's 15s
		// heartbeat under race instrumentation. Only that exact control frame
		// may precede the response; every other message must match this read.
		if frameType == "heartbeat" && len(frame) == 1 {
			continue
		}
		var id string
		if (wantID != "" && (json.Unmarshal(frame["id"], &id) != nil || id != wantID)) || (wantType != "" && frameType != wantType) {
			var message string
			_ = json.Unmarshal(frame["message"], &message)
			t.Fatalf("browser returned an unexpected upload response: want id=%q type=%q, got id=%q type=%q message=%.512q", wantID, wantType, id, frameType, message)
		}
		return raw
	}
}

func TestWebUIUploadMaximumBytesAndExactPathAcrossAuthenticatedTransport(t *testing.T) {
	f := newWebUIUploadFixture(t, true)
	browser, _, err := websocket.Dial(f.ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/connect?session_id="+f.session.ID, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}, "Cookie": []string{adminCookie + "=" + f.adminToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.CloseNow()
	opened := f.frame()
	if opened.Action != "open" || opened.SessionID != f.session.ID || opened.RuntimeGeneration != 1 {
		t.Fatal("browser upload target changed")
	}
	f.send("webui", protocol.WebUIFrame{ID: opened.ID, Action: "ready"})
	readWebUIUploadMessage(t, f.ctx, browser, "", "ready")
	data := bytes.Repeat([]byte{0, 0xff, 0x80, 'a'}, protocol.MaxFileBytes/4)
	copy(data, []byte("secret-sentinel\x00\xff"))
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "secret-sentinel report.bin", Data: data}
	input := webUIUploadJSON(t, "maximum-upload", upload)
	if err := browser.Write(f.ctx, websocket.MessageText, input); err != nil {
		t.Fatal(err)
	}
	forwarded := f.frame()
	var request struct {
		ID     string               `json:"id"`
		Method string               `json:"method"`
		Params protocol.WebUIUpload `json:"params"`
	}
	if forwarded.ID != opened.ID || forwarded.Action != "input" || json.Unmarshal(forwarded.Data, &request) != nil || request.Method != "gateway/upload" || request.Params.UploadID != upload.UploadID || request.Params.Name != upload.Name || !bytes.Equal(request.Params.Data, data) {
		t.Fatalf("maximum-size upload bytes or frozen target changed in transit: action=%q viewer=%q method=%q upload=%q bytes=%d", forwarded.Action, forwarded.ID, request.Method, request.Params.UploadID, len(request.Params.Data))
	}
	expected := filepath.Join(f.session.CWD, "inbound", protocol.InboundFilename(upload.Name, upload.UploadID))
	result := protocol.WebUIUploadResult{UploadID: upload.UploadID, Path: expected, Text: "File saved in " + expected}
	output, _ := json.Marshal(map[string]any{"id": request.ID, "result": result})
	f.send("webui", protocol.WebUIFrame{ID: opened.ID, Action: "output", Data: output})
	response := readWebUIUploadMessage(t, f.ctx, browser, request.ID, "")
	var reply struct {
		Result protocol.WebUIUploadResult `json:"result"`
	}
	if json.Unmarshal(response, &reply) != nil || reply.Result.Path != expected || reply.Result.UploadID != upload.UploadID || strings.Contains(reply.Result.Text, "secret-sentinel") {
		t.Fatal("typed saved path did not survive redaction while its visible text was redacted")
	}
	// A repeated response has no pending local upload and receives the ordinary
	// text boundary, even when it carries a formerly valid result shape.
	f.send("webui", protocol.WebUIFrame{ID: opened.ID, Action: "output", Data: output})
	response = readWebUIUploadMessage(t, f.ctx, browser, request.ID, "")
	if bytes.Contains(response, []byte("secret-sentinel")) {
		t.Fatal("uncorrelated repeated result bypassed redaction")
	}
	commands, err := f.store.PendingCommands(f.ctx, 10)
	if err != nil || len(commands) != 0 {
		t.Fatal("browser upload entered the durable command queue")
	}
	if err := f.store.RevokeAdminSession(f.ctx, f.adminToken); err != nil {
		t.Fatal(err)
	}
	upload.Data = []byte{}
	if err := browser.Write(f.ctx, websocket.MessageText, webUIUploadJSON(t, "revoked-upload", upload)); err != nil {
		t.Fatal(err)
	}
	response = readWebUIUploadMessage(t, f.ctx, browser, "", "error")
	if !bytes.Contains(response, []byte("session expired")) {
		t.Fatal("upload bypassed admin-session revocation")
	}
	_, _, _ = browser.Read(f.ctx)
	f.expectClose(opened.ID)
}

func TestWebUIUploadOldWorkerReceivesUpgradeAdvice(t *testing.T) {
	f := newWebUIUploadFixture(t, false)
	browser, _, err := websocket.Dial(f.ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/connect?session_id="+f.session.ID, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}, "Cookie": []string{adminCookie + "=" + f.adminToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.CloseNow()
	opened := f.frame()
	f.send("webui", protocol.WebUIFrame{ID: opened.ID, Action: "ready"})
	readWebUIUploadMessage(t, f.ctx, browser, "", "ready")
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "file.bin", Data: []byte{}}
	if err := browser.Write(f.ctx, websocket.MessageText, webUIUploadJSON(t, "old-worker-upload", upload)); err != nil {
		t.Fatal(err)
	}
	response := readWebUIUploadMessage(t, f.ctx, browser, "old-worker-upload", "")
	if !bytes.Contains(response, []byte("update this worker")) {
		t.Fatal("old worker upload lacked actionable upgrade advice")
	}
	// The upload error is correlated to its caller; the viewer remains usable
	// and a generic disconnect cannot replace the actionable upgrade advice.
	if err := browser.Write(f.ctx, websocket.MessageText, []byte(`{"id":"still-connected","method":"thread/read","params":{"includeTurns":false}}`)); err != nil {
		t.Fatal(err)
	}
	forwarded := f.frame()
	if forwarded.ID != opened.ID || forwarded.Action != "input" || !bytes.Contains(forwarded.Data, []byte("still-connected")) {
		t.Fatal("old worker upload was forwarded or its viewer disconnected")
	}
	browser.CloseNow()
	f.expectClose(opened.ID)
}
