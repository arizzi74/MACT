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
	p := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx, writes: make(chan writeRequest), supportsWebUIUploads: true, maxFileBytes: protocol.MaxFileBytes}
	hub.peers[p.workerID.String()] = p
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: ctx, cancel: cancel}
	upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "report.bin", Data: bytes.Repeat([]byte{0xfa}, protocol.MaxFileBytes)}
	raw, _ := json.Marshal(map[string]any{"id": 4, "method": "gateway/upload", "params": upload})
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, raw) }()
	// Race instrumentation and concurrent package tests can make validation
	// of a full 20 MiB upload exceed the former 10-second fixture deadline.
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
	case <-time.After(30 * time.Second):
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
	p.maxFileBytes = protocol.MaxFileBytes
	oversized, err := json.Marshal(map[string]any{"id": 2, "method": "gateway/upload", "params": protocol.WebUIUpload{
		UploadID: uuid.NewString(), Name: "report.bin", Data: make([]byte, protocol.MaxFileBytes+1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"/tmp/report.bin","data":"AA=="}}`),
		json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"report.bin","data":null}}`),
		json.RawMessage(`{"id":1,"method":"gateway/upload","params":{"uploadId":"` + uuid.NewString() + `","name":"report.bin","data":"AA==","path":"/tmp/report.bin"}}`),
		oversized,
	} {
		if err := s.Send(ctx, invalid); err == nil {
			t.Fatal("invalid browser upload was forwarded")
		}
	}
	if len(p.writes) != 0 || s.inputBytes != 0 || hub.webuiInputBytes != 0 {
		t.Fatal("rejected upload was queued or leaked byte capacity")
	}
}

func TestWebUIUploadGatewayKeepsLegacyWorkerFileAndFrameBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hub := NewHub(webUIImageRegistry{}, nil, time.Second, time.Second)
	p := &peer{workerID: uuid.New(), connectionID: uuid.New(), ctx: ctx, writes: make(chan writeRequest, 1), supportsWebUIUploads: true}
	hub.peers[p.workerID.String()] = p
	s := &WebUIStream{hub: hub, peer: p, id: uuid.NewString(), ctx: ctx, cancel: cancel}
	request := func(size int) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"id": 3, "method": "gateway/upload", "params": protocol.WebUIUpload{
			UploadID: uuid.NewString(), Name: "report.bin", Data: make([]byte, size),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	maximum := request(protocol.LegacyMaxFileBytes)
	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, maximum) }()
	select {
	case request := <-p.writes:
		request.finish(nil)
	case <-time.After(30 * time.Second):
		t.Fatal("legacy maximum upload was not forwarded")
	}
	if err := <-done; err != nil {
		t.Fatalf("legacy maximum upload stopped working: %v", err)
	}
	padded := request(0)
	padded = append(padded, bytes.Repeat([]byte{' '}, protocol.LegacyMaxWebUIInputBytes+1-len(padded))...)
	for _, raw := range []json.RawMessage{request(protocol.LegacyMaxFileBytes + 1), padded} {
		if err := s.Send(ctx, raw); !errors.Is(err, ErrWebUIUploadsUnsupported) {
			t.Fatalf("legacy oversized upload needs an actionable upgrade error: %v", err)
		}
	}
	if len(p.writes) != 0 || s.inputBytes != 0 || hub.webuiInputBytes != 0 || ctx.Err() != nil {
		t.Fatal("legacy upload rejection queued a frame, leaked capacity, or disconnected the worker")
	}
}
