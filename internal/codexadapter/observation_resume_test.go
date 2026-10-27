package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func abandonObservationResume(t *testing.T, client *Client, fake *fakeServer, threadID string) int64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ResumeThreadForObservation(ctx, threadID); done <- err }()
	request := fake.next(t)
	if method(t, request) != "thread/resume" {
		t.Fatalf("observation method = %s", request["method"])
	}
	fields := params(t, request)
	if len(fields) != 2 || string(fields["threadId"]) != fmt.Sprintf("%q", threadID) || string(fields["excludeTurns"]) != "true" {
		t.Fatalf("observation changed resume settings: %s", request["params"])
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation = %v", err)
	}
	var id int64
	if err := json.Unmarshal(request["id"], &id); err != nil {
		t.Fatal(err)
	}
	awaitUpdateSafety(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.updateWriting[id] == nil
	})
	return id
}

func observationSuccess(id int64, threadID string) []byte {
	return []byte(fmt.Sprintf(`{"id":%d,"result":{"thread":{"id":%q,"status":"idle"}}}`, id, threadID))
}

func TestObservationResumeSettlesOnlyAfterMatchingLateAcknowledgment(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	id := abandonObservationResume(t, client, fake, "private-observed-thread")
	if client.UpdateQuiescent() || client.UpdateSafety().UnconfirmedRequests != 1 {
		t.Fatal("unanswered observation lost its RPC fence")
	}
	for i := 0; i < maxUpdateUnconfirmed+5; i++ {
		if _, err := client.ResumeThreadForObservation(context.Background(), "private-observed-thread"); !errors.Is(err, ErrObservationResumePending) {
			t.Fatalf("overlapping observation retry = %v", err)
		}
	}
	status := client.UpdateSafety()
	if status.UnconfirmedRequests != 1 || status.PendingRequests != 0 || status.Overflow {
		t.Fatalf("retries accumulated uncertain requests: %+v", status)
	}
	raw, err := json.Marshal(status)
	if err != nil || strings.Contains(string(raw), "private-observed-thread") {
		t.Fatalf("observation diagnostics leaked identity: %s %v", raw, err)
	}
	client.handleLine(observationSuccess(id, "private-observed-thread"))
	if !client.UpdateQuiescent() {
		t.Fatalf("matching late observation acknowledgment did not settle: %+v", client.UpdateSafety())
	}
	// A duplicate response cannot undo the first successful acknowledgment.
	client.handleLine([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"duplicate"}}`, id)))
	if !client.UpdateQuiescent() {
		t.Fatal("duplicate response changed settled observation")
	}
}

// Delay the writer's completion after the server has received the request. A
// response may arrive first, but neither deduplication nor the write fence may
// disappear before the transport write returns.
type observationCompletionWriter struct {
	io.WriteCloser
	release chan struct{}
}

func (w *observationCompletionWriter) Write(data []byte) (int, error) {
	n, err := w.WriteCloser.Write(data)
	<-w.release
	return n, err
}

func TestObservationResumeAcknowledgmentBeforeCancellationKeepsWriterFence(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	writer := &observationCompletionWriter{WriteCloser: client.t.In, release: make(chan struct{})}
	client.t.In = writer
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(writer.release) }) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ResumeThreadForObservation(ctx, "observed"); done <- err }()
	request := fake.next(t)
	var id int64
	_ = json.Unmarshal(request["id"], &id)
	if _, err := client.ResumeThreadForObservation(context.Background(), "observed"); !errors.Is(err, ErrObservationResumePending) {
		t.Fatalf("live observation was not deduplicated: %v", err)
	}
	client.handleLine(observationSuccess(id, "observed"))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("observation waiting for writer = %v", err)
	}
	status := client.UpdateSafety()
	if status.UnconfirmedRequests != 0 || status.PendingRequests != 1 || len(status.Blockers) != 1 || status.Blockers[0].Phase != "writing" || client.UpdateQuiescent() {
		t.Fatalf("acknowledged observation lost unfinished write: %+v", status)
	}
	if _, err := client.ResumeThreadForObservation(context.Background(), "observed"); !errors.Is(err, ErrObservationResumePending) {
		t.Fatalf("unfinished write was not deduplicated: %v", err)
	}
	release.Do(func() { close(writer.release) })
	awaitUpdateSafety(t, client.UpdateQuiescent)
}

func TestObservationResumeAmbiguousFirstResponseCannotBeRepaired(t *testing.T) {
	cases := []struct{ name, response string }{
		{"different_thread", `{"id":%d,"result":{"thread":{"id":"other"}}}`},
		{"missing_thread", `{"id":%d,"result":{}}`},
		{"null_result", `{"id":%d,"result":null}`},
		{"null_thread", `{"id":%d,"result":{"thread":null}}`},
		{"null_thread_id", `{"id":%d,"result":{"thread":{"id":null}}}`},
		{"duplicate_thread", `{"id":%d,"result":{"thread":{"id":"other"},"thread":{"id":"observed"}}}`},
		{"duplicate_thread_id", `{"id":%d,"result":{"thread":{"id":"other","id":"observed"}}}`},
		{"case_alias_thread", `{"id":%d,"result":{"thread":{"id":"observed"},"Thread":{"id":"observed"}}}`},
		{"case_alias_id", `{"id":%d,"result":{"thread":{"id":"observed","ID":"observed"}}}`},
		{"case_alias_envelope", `{"id":%d,"ID":999,"result":{"thread":{"id":"observed"}}}`},
		{"invalid_thread_field", `{"id":%d,"result":{"thread":{"id":"observed","cwd":42}}}`},
		{"internal_error", `{"id":%d,"error":{"code":-32603,"message":"uncertain"}}`},
		{"server_error", `{"id":%d,"error":{"code":-32000,"message":"uncertain"}}`},
		{"case_alias_error_code", `{"id":%d,"error":{"code":-32601,"Code":-32000,"message":"uncertain"}}`},
		{"ambiguous_envelope", `{"id":%d,"result":{"thread":{"id":"observed"}},"error":{"code":-32601,"message":"uncertain"}}`},
		{"duplicate_envelope_id", `{"id":%d,"id":999,"result":{"thread":{"id":"observed"}}}`},
		{"missing_error_message", `{"id":%d,"error":{"code":-32601}}`},
		{"malformed_json", `{"id":%d,"result":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			id := abandonObservationResume(t, client, fake, "observed")
			client.handleLine([]byte(fmt.Sprintf(tc.response, id)))
			client.handleLine(observationSuccess(id, "observed"))
			client.handleLine([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"duplicate"}}`, id)))
			if client.UpdateQuiescent() || client.UpdateSafety().UnconfirmedRequests != 1 {
				t.Fatalf("duplicate repaired ambiguous observation: %+v", client.UpdateSafety())
			}
			client.mu.Lock()
			hasPayload := client.updateUnconfirmed[id].reply != nil
			client.mu.Unlock()
			if hasPayload {
				t.Fatal("abandoned observation retained a response payload")
			}
		})
	}
}

func TestObservationResumeLiveInvalidAcknowledgmentRemainsFenced(t *testing.T) {
	for _, response := range []string{`{"id":%d,"result":null}`, `{"id":%d,"result":{"thread":{"id":"other"}}}`} {
		t.Run(response, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() { _, err := client.ResumeThreadForObservation(context.Background(), "observed"); done <- err }()
			request := fake.next(t)
			var id int64
			_ = json.Unmarshal(request["id"], &id)
			client.handleLine([]byte(fmt.Sprintf(response, id)))
			if err := <-done; err == nil {
				t.Fatal("invalid observation acknowledgment succeeded")
			}
			client.handleLine(observationSuccess(id, "observed"))
			if client.UpdateQuiescent() || client.UpdateSafety().UnconfirmedRequests != 1 {
				t.Fatal("consuming invalid acknowledgment erased uncertainty")
			}
		})
	}
}

func TestObservationResumeConsumedLiveErrorStillAllowsRetry(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { _, err := client.ResumeThreadForObservation(context.Background(), "observed"); done <- err }()
	request := fake.next(t)
	fake.write(t, map[string]any{"id": json.RawMessage(request["id"]), "error": map[string]any{"code": -32000, "message": "temporary failure"}})
	if err := <-done; err == nil {
		t.Fatal("live RPC error succeeded")
	}
	if !client.UpdateQuiescent() {
		t.Fatal("consumed live error changed normal retry behavior")
	}
	id := abandonObservationResume(t, client, fake, "observed")
	client.handleLine(observationSuccess(id, "observed"))
	if !client.UpdateQuiescent() {
		t.Fatal("retry after live error did not settle")
	}
}

func TestObservationResumeDoesNotRecoverGenericResumesOrOtherMutations(t *testing.T) {
	for _, options := range []ThreadOptions{{}, {Model: "override", CWD: "/other", ApprovalPolicy: "never"}} {
		t.Run(fmt.Sprint(options), func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.ResumeThread(ctx, "generic", options); done <- err }()
			request := fake.next(t)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			var id int64
			_ = json.Unmarshal(request["id"], &id)
			client.handleLine(observationSuccess(id, "generic"))
			observed := abandonObservationResume(t, client, fake, "observed")
			client.handleLine(observationSuccess(observed, "observed"))
			if client.UpdateQuiescent() || client.UpdateSafety().UnconfirmedRequests != 1 {
				t.Fatal("observation recovery cleared a generic resume")
			}
		})
	}
	client, fake := newFake(t)
	initialize(t, client, fake)
	id := abandonUpdateRPC(t, client, fake, "turn/start")
	client.handleLine(observationSuccess(id, "observed"))
	if client.UpdateQuiescent() || client.UpdateSafety().UnconfirmedRequests != 1 {
		t.Fatal("matching-looking acknowledgment cleared a turn mutation")
	}
}

func TestObservationResumeRepeatedLateAcknowledgmentsDoNotOverflow(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	for i := 0; i < maxUpdateUnconfirmed+5; i++ {
		id := abandonObservationResume(t, client, fake, "observed")
		client.handleLine(observationSuccess(id, "observed"))
		if !client.UpdateQuiescent() || client.UpdateSafety().Overflow {
			t.Fatalf("settled observation %d accumulated blockers: %+v", i, client.UpdateSafety())
		}
	}
	if _, err := client.ResumeThreadForObservation(context.Background(), strings.Repeat("x", 257)); err == nil || !client.UpdateQuiescent() {
		t.Fatal("unbounded observation identity was accepted")
	}
}

func TestObservationResumeAcknowledgmentCancellationRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		client := &Client{ready: true, pending: make(map[int64]*pendingRPC), updateUnconfirmed: make(map[int64]*pendingRPC), updateWriting: make(map[int64]*pendingRPC)}
		request := &pendingRPC{id: 1, method: "thread/resume", observationThreadID: "observed", writeDone: true, startedAt: time.Now(), done: make(chan struct{})}
		client.pending[1] = request
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			client.acceptRPCResponse(1, rpcResponse{Result: json.RawMessage(`{"thread":{"id":"observed"}}`)}, false)
		}()
		go func() { defer group.Done(); client.finishRPCCall(request, true, false) }()
		group.Wait()
		if !client.UpdateQuiescent() {
			t.Fatalf("acknowledged observation race remained uncertain: %+v", client.UpdateSafety())
		}
	}
}
