package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIPromptRejectionPersistsAfterBrowserDetach(t *testing.T) {
	_, socket, requests := newNativeProxyTest(t)
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	t.Cleanup(func() { _ = store.Close() })
	cfg.AllowedWorkspaceRoots = []string{t.TempDir()}
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1, State: "running", LocalSocket: socket}
	session, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "prompt-errors", CWD: cfg.AllowedWorkspaceRoots[0], State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	redactor, err := newWorkerRedactor([]string{`private-value`})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.configureRedactor(redactor); err != nil {
		t.Fatal(err)
	}
	c := &Connection{cfg: cfg, store: store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writes := make(chan outbound, 16)
	frames := make(chan protocol.WebUIFrame, 16)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case output := <-writes:
				frame, err := protocol.Payload[protocol.WebUIFrame](output.envelope)
				output.done <- err
				if err == nil {
					frames <- frame
				}
			}
		}
	}()
	pool := newWebUIRelays(c, ctx, writes)
	defer pool.close()
	id := uuid.NewString()
	pool.receive(protocol.WebUIFrame{ID: id, Action: "open", RuntimeID: runtime.ID, RuntimeGeneration: 1, SessionID: session.ID})
	initialize := nextNativeRequest(t, requests)
	nativeWrite(t, initialize.connection, `{"id":"webui-initialize","result":{}}`)
	_ = nextNativeRequest(t, requests) // initialized notification
	_ = nextWebUIFrame(t, frames)
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: json.RawMessage(`{"id":1,"method":"thread/resume","params":{}}`)})
	resume := nextNativeRequest(t, requests)
	response, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"thread": map[string]any{"id": session.ThreadID, "cwd": session.CWD}}})
	nativeWrite(t, resume.connection, string(response))
	_ = nextWebUIFrame(t, frames)
	pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: json.RawMessage(`{"id":2,"method":"turn/start","params":{"input":[{"type":"text","text":"hello"}]}}`)})
	start := nextNativeRequest(t, requests)
	pool.receive(protocol.WebUIFrame{ID: id, Action: "close"})
	// The browser has left this session before its definite rejection arrives.
	nativeWrite(t, start.connection, `{"id":2,"error":{"code":-32000,"message":"Model capacity unavailable private-value","data":{"secret":"private-value"}}}`)
	done := make(chan struct{})
	go func() { pool.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("browser detach failed to drain the prompt rejection")
	}
	events, err := store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	var started protocol.Event
	count := 0
	for _, event := range events {
		if event.Kind == "session_prompt_started" {
			started = event
		}
		if event.Kind != "session_prompt_failed" {
			continue
		}
		count++
		var failure protocol.SessionPromptEvent
		if json.Unmarshal(event.Data, &failure) != nil || failure.Error == nil || failure.AfterEventSeq != started.Seq ||
			failure.Error.Message != "Model capacity unavailable [REDACTED]" || strings.Contains(string(event.Data), "private-value") || strings.Contains(string(event.Data), "secret") {
			t.Fatalf("unredacted or uncorrelated rejection persisted: %s", event.Data)
		}
	}
	if started.Seq == 0 || count != 1 {
		t.Fatalf("prompt lifecycle counts: started=%d failed=%d", started.Seq, count)
	}
}

func TestWebUIPromptRejectionExcludesStaleRuntimeSuccessAndReadOnlyErrors(t *testing.T) {
	for _, scenario := range []string{"stale_runtime", "success", "read_only"} {
		t.Run(scenario, func(t *testing.T) {
			workerID := uuid.NewString()
			store, cfg := testConnectionStore(t, workerID)
			t.Cleanup(func() { _ = store.Close() })
			cfg.AllowedWorkspaceRoots = []string{t.TempDir()}
			runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1, State: "running", LocalSocket: "/unused.sock"}
			session, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread", CWD: cfg.AllowedWorkspaceRoots[0], State: "idle"})
			if err != nil {
				t.Fatal(err)
			}
			r := testWebUIRelay()
			r.session, r.runtime = session, runtime
			r.ctx = t.Context()
			r.pool = &webUIRelays{ctx: t.Context(), c: &Connection{cfg: cfg, store: store,
				snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}}
			r.pending["n:1"] = "turn/start"
			if scenario == "read_only" {
				r.pending["n:1"] = "model/list"
			} else if err := r.startPromptAttempt(json.RawMessage(`1`)); err != nil {
				t.Fatal(err)
			}
			response := []byte(`{"id":1,"error":{"code":-32000,"message":"capacity"}}`)
			if scenario == "stale_runtime" {
				runtime.Generation++
			}
			if scenario == "success" {
				response = []byte(`{"id":1,"result":{"turn":{"id":"accepted"}}}`)
			}
			if _, err := r.serverMessage(response); err != nil {
				t.Fatal(err)
			}
			// Duplicate/late responses have no reserved request left to persist.
			if _, err := r.serverMessage(response); err != nil {
				t.Fatal(err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "session_prompt_failed" {
					t.Fatalf("nonterminal/unrelated reply created failure: %s", event.Data)
				}
			}
		})
	}
}
