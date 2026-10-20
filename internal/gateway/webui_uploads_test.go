package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIUploadGatewayPreservesMaximumFileAndReleasesBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hub := NewHub(webUIImageRegistry{}, nil, time.Second, time.Second)
	p := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx, writes: make(chan writeRequest), supportsWebUIUploads: true}
	hub.peers[p.workerID.String()] = p
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: ctx, cancel: cancel}
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "report.bin", Data: bytes.Repeat([]byte{0xfa}, protocol.MaxFileBytes)}
	raw, _ := json.Marshal(map[string]any{"id": 4, "method": "gateway/upload", "params": upload})
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, raw) }()
	select {
	case request := <-p.writes:
		frame, err := protocol.Payload[protocol.WebUIFrame](request.envelope)
		if err != nil || !bytes.Equal(frame.Data, raw) || frame.Action != "input" {
			t.Fatal("gateway changed or corrupted the upload")
		}
		hub.mu.RLock()
		held := s.inputBytes == len(raw) && hub.webuiInputBytes == len(raw)
		hub.mu.RUnlock()
		if !held {
			t.Fatal("maximum upload lost its in-flight reservation")
		}
		request.finish(nil)
	case <-time.After(10 * time.Second):
		t.Fatal("maximum upload was not forwarded")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	hub.mu.RLock()
	remaining := s.inputBytes + hub.webuiInputBytes
	hub.mu.RUnlock()
	if remaining != 0 {
		t.Fatal("upload completion leaked the byte budget")
	}
}

func TestWebUIUploadGatewayRejectsOldWorkerAndInvalidInputBeforeWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hub := NewHub(webUIImageRegistry{}, nil, time.Second, time.Second)
	p := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx, writes: make(chan writeRequest, 1)}
	hub.peers[p.workerID.String()] = p
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: ctx, cancel: cancel}
	raw := json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"report.bin","data":"AA=="}}`)
	if err := s.Send(ctx, raw); !errors.Is(err, ErrWebUIUploadsUnsupported) {
		t.Fatalf("older worker error was not actionable: %v", err)
	}
	p.supportsWebUIUploads = true
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"/tmp/report.bin","data":"AA=="}}`),
		json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"report.bin","data":null}}`),
		json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"report.bin","data":"AA==","path":"/tmp/report.bin"}}`),
	} {
		if err := s.Send(ctx, invalid); err == nil {
			t.Fatal("invalid browser upload was forwarded")
		}
	}
	if len(p.writes) != 0 || s.inputBytes != 0 || hub.webuiInputBytes != 0 {
		t.Fatal("rejected upload was queued or leaked byte capacity")
	}
}
