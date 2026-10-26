package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func TestWebUIInventoryReadsDoNotWaitForTimestampWrites(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "inventory-during-index-write", "")
	runtime.LocalSocket = "/unused-inventory-test.sock"
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- a.store.db.Update(func(tx *workerdb.Tx) error {
			if err := tx.Bucket(bucketMeta).Put([]byte("pending-timestamp-batch"), []byte("index metadata")); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timestamp writer did not reach its held transaction")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	pool := newWebUIRelays(&Connection{cfg: a.cfg, store: a.store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}, ctx, nil)
	_, selected, err := pool.targetContext(ctx, protocol.WebUIFrame{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: session.ID})
	if err != nil || selected.ID != session.ID {
		t.Fatalf("session authorization waited for the index writer: %#v, %v", selected, err)
	}
}

func TestWebUIHistoryReplyAndInputDoNotWaitForTimestampIndex(t *testing.T) {
	for _, stage := range []string{"complete", "cancel", "generation", "archive"} {
		t.Run(stage, func(t *testing.T) {
			home := t.TempDir()
			stamp := time.Date(2026, 10, 9, 8, 1, 15, 123000000, time.UTC)
			path := historyTimestampFixture(t, home, "thread", webUITimestampEvent("assistant", "final_answer", stamp.Format(time.RFC3339Nano), "saved answer"))
			a, runtime, session, server := webUITimestampAgent(t, home, path, []map[string]any{webUITimestampItem("item-1", "assistant", "final_answer", "saved answer")})
			runtime.LocalSocket = "/unused-timestamp-test.sock"
			ctx, cancel := context.WithCancel(t.Context())
			writes := make(chan outbound, 8)
			frames := make(chan protocol.WebUIFrame, 8)
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
			r := testWebUIRelay()
			r.id, r.ctx, r.runtime, r.session, r.resumed, r.observerReady = "timing-relay", ctx, runtime, session, true, true
			r.pool = newWebUIRelays(&Connection{cfg: a.cfg, store: a.store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }, historyWebUI: a.webUIHistory, questionsWebUI: a.webUIQuestions}, ctx, writes)
			idx := &a.manager.stats.timestamps
			idx.mu.Lock()
			locked := true
			defer func() {
				cancel()
				r.stopHistoryTimestamps()
				if locked {
					idx.mu.Unlock()
				}
			}()
			if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/items/list","params":{}}`)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.history(webUIRPC{ID: json.RawMessage(`1`)}) }()
			frame := nextWebUIFrame(t, frames)
			if !strings.Contains(string(frame.Data), "saved answer") || strings.Contains(string(frame.Data), "recordedAtMs") {
				t.Fatalf("initial transcript waited for or invented a recorded timestamp: %s", frame.Data)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("timestamp indexing held the transcript input lane")
			}
			waitFor(t, func() bool { return len(a.webHistoryTiming) == 1 })
			if !a.updateMu.TryLock() {
				t.Fatal("optional indexing retained worker admission")
			}
			a.updateMu.Unlock()
			go func() { done <- r.forwardInput(nil, []byte(`{"id":2,"method":"gateway/questions","params":{}}`)) }()
			frame = nextWebUIFrame(t, frames)
			if !strings.Contains(string(frame.Data), `"id":2`) || !strings.Contains(string(frame.Data), `"questions":[]`) {
				t.Fatalf("essential next RPC blocked behind timestamps: %s", frame.Data)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if stage == "cancel" {
				cancel()
				stopped := make(chan struct{})
				go func() { r.stopHistoryTimestamps(); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("optional work ignored cancellation while waiting for the index")
				}
				if len(a.webHistoryTiming) != 0 {
					t.Fatal("canceled enrichment retained the worker timing slot")
				}
				return
			}
			if stage == "generation" {
				runtime.Generation++
				a.manager.install(runtime, mustClient(t, a, r.runtime))
			}
			if stage == "archive" {
				session.Archived = true
				if _, err := a.store.UpsertSession(session); err != nil {
					t.Fatal(err)
				}
			}
			idx.mu.Unlock()
			locked = false
			if stage != "complete" {
				waitFor(t, func() bool { return len(a.webHistoryTiming) == 0 })
				select {
				case frame := <-frames:
					t.Fatalf("stale history timing reached the browser: %s", frame.Data)
				default:
				}
				return
			}
			frame = nextWebUIFrame(t, frames)
			var event struct {
				Method string
				Params struct {
					ThreadID string `json:"threadId"`
					Items    []webUIHistoryTimestampItem
				}
			}
			if json.Unmarshal(frame.Data, &event) != nil || event.Method != "gateway/history/timestamps" || event.Params.ThreadID != session.ThreadID || len(event.Params.Items) != 1 || event.Params.Items[0].ItemID != "item-1" || event.Params.Items[0].TurnID != "turn" || event.Params.Items[0].RecordedAtMS != stamp.UnixMilli() || strings.Contains(string(frame.Data), "saved answer") {
				t.Fatalf("invalid deferred timestamp update: %s", frame.Data)
			}
			// Verified dates are immediately reusable from the bounded snapshot.
			idx.mu.Lock()
			locked = true
			if _, err := r.clientMessage([]byte(`{"id":3,"method":"thread/items/list","params":{}}`)); err != nil {
				t.Fatal(err)
			}
			go func() { done <- r.history(webUIRPC{ID: json.RawMessage(`3`)}) }()
			frame = nextWebUIFrame(t, frames)
			if !strings.Contains(string(frame.Data), `"recordedAtMs":`) || fullWebUIHistoryReads(server) != 1 {
				t.Fatalf("verified cached time was lost or full history reread: %s", frame.Data)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
