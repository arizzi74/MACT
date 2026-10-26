package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIHubSwitchAcrossWorkersDoesNotWaitForOldViewer(t *testing.T) {
	for _, blocked := range []string{"readiness", "detach"} {
		t.Run(blocked, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			hub := NewHub(webUIImageRegistry{}, nil, time.Second, time.Second)
			newWorker := func() (*peer, registry.AdminSession) {
				p := &peer{workerID: uuid.New(), connectionID: uuid.New(), supportsWebUI: true,
					ctx: ctx, cancel: cancel, writes: make(chan writeRequest)}
				hub.peers[p.workerID.String()] = p
				return p, registry.AdminSession{Session: protocol.Session{
					ID: uuid.NewString(), WorkerID: p.workerID.String(), RuntimeID: uuid.NewString(),
				}, RuntimeGeneration: 1}
			}
			workerA, sessionA := newWorker()
			workerB, sessionB := newWorker()
			open := func(p *peer, session registry.AdminSession) *WebUIStream {
				t.Helper()
				openCtx, stop := context.WithTimeout(ctx, time.Second)
				defer stop()
				type result struct {
					stream *WebUIStream
					err    error
				}
				opened := make(chan result, 1)
				go func() {
					// The stream outlives the caller's bounded wait below.
					stream, err := hub.OpenWebUI(ctx, session)
					opened <- result{stream, err}
				}()
				select {
				case request := <-p.writes:
					frame, err := protocol.Payload[protocol.WebUIFrame](request.envelope)
					request.finish(nil)
					if err != nil || request.envelope.Type != "webui" || frame.Action != "open" || frame.SessionID != session.ID {
						t.Fatalf("worker received incorrect open: %#v, %v", frame, err)
					}
				case <-openCtx.Done():
					t.Fatal("opening a viewer waited for another worker")
				}
				select {
				case result := <-opened:
					if result.err != nil {
						t.Fatal(result.err)
					}
					return result.stream
				case <-openCtx.Done():
					t.Fatal("viewer open did not finish after its worker write")
					return nil
				}
			}
			old := open(workerA, sessionA)
			var current *WebUIStream
			defer func() {
				cancel()
				old.Close()
				if current != nil {
					current.Close()
				}
			}()
			var detach writeRequest
			var detached chan struct{}
			if blocked == "detach" {
				detached = make(chan struct{})
				go func() { old.Close(); close(detached) }()
				select {
				case detach = <-workerA.writes:
					frame, err := protocol.Payload[protocol.WebUIFrame](detach.envelope)
					defer detach.finish(nil)
					if err != nil || frame.Action != "close" || frame.ID != old.id {
						t.Fatalf("incorrect old viewer detach: %#v, %v", frame, err)
					}
				case <-ctx.Done():
					t.Fatal("old viewer did not send its detach")
				}
				// Hold the old worker's socket write without acknowledging it.
			}

			// Worker A has never sent ready. Switching hosts must still allow
			// worker B to open and deliver ready, even during A's detach write.
			current = open(workerB, sessionB)
			envelope, err := protocol.NewEnvelope("webui", protocol.WebUIFrame{ID: current.id, Action: "ready"})
			if err != nil {
				t.Fatal(err)
			}
			if err := hub.handleWebUI(workerB, envelope); err != nil {
				t.Fatal(err)
			}
			select {
			case frame := <-current.Frames():
				current.Consumed(frame)
				if frame.ID != current.id || frame.Action != "ready" {
					t.Fatalf("incorrect new viewer readiness: %#v", frame)
				}
			case <-ctx.Done():
				t.Fatal("new worker readiness was blocked by the old viewer")
			}
			if len(old.Frames()) != 0 {
				t.Fatal("new worker readiness reached the old viewer")
			}
			if detached != nil {
				select {
				case <-detached:
					t.Fatal("switching workers waited for the old detach to finish")
				default:
				}
				detach.finish(nil)
				select {
				case <-detached:
				case <-ctx.Done():
					t.Fatal("old detach did not finish after its write completed")
				}
			}
			if workerA.ctx.Err() != nil || workerB.ctx.Err() != nil {
				t.Fatal("switching a viewer disconnected a worker")
			}
		})
	}
}
