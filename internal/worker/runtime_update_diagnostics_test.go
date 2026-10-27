package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestRuntimeUpdateDiagnosticsReflectLiveCountersWithoutRPCsOrPrivatePayloads(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	client := mustClient(t, a, runtime)
	initialCalls := len(server.Calls())
	initial := runtimeUpdateSafety(t, a.manager.snapshotWithUpdateSafety()[0])
	if !initial.Ready || initial.Closed || initial.PendingRequests != 0 || initial.UnconfirmedRequests != 0 {
		t.Fatalf("initial diagnostics = %+v", initial)
	}
	if len(server.Calls()) != initialCalls {
		t.Fatal("taking runtime diagnostics issued an RPC")
	}
	server.SetResponseDelay("thread/name/set", 100*time.Millisecond)
	if err := server.SetMethodResult("thread/name/set", map[string]any{"private": "PRIVATE_RESPONSE"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RenameThread(ctx, "PRIVATE_THREAD_ID", "PRIVATE_SESSION_NAME") }()
	waitFor(t, func() bool { return hasCall(server.Calls(), "thread/name/set") })
	pendingSnapshot := a.manager.snapshotWithUpdateSafety()
	pending := runtimeUpdateSafety(t, pendingSnapshot[0])
	if pending.PendingRequests != 1 || pending.UnconfirmedRequests != 0 || len(pending.Blockers) != 1 || pending.Blockers[0].Method != "thread/name/set" || pending.Blockers[0].StartedAt.IsZero() {
		t.Fatalf("pending diagnostics = %+v", pending)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled mutation = %v", err)
	}
	waitFor(t, func() bool {
		status := client.UpdateSafety()
		return status.PendingRequests == 0 && status.UnconfirmedRequests == 1
	})
	// Wait for the private reply to cross the real adapter boundary. A late
	// success cannot settle this arbitrary abandoned mutation.
	if _, err := client.LoadedThreads(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	current := a.manager.snapshotWithUpdateSafety()
	status := runtimeUpdateSafety(t, current[0])
	if status.PendingRequests != 0 || status.UnconfirmedRequests != 1 || len(status.Blockers) != 1 || status.Blockers[0].Phase != "unconfirmed" {
		t.Fatalf("current diagnostics = %+v", status)
	}
	if runtimeUpdateSafety(t, pendingSnapshot[0]).PendingRequests != 1 {
		t.Fatal("refresh changed a previously returned snapshot")
	}
	for name, frame := range map[string]any{
		"hello":     protocol.Hello{Runtimes: current},
		"heartbeat": protocol.Heartbeat{Runtimes: current},
	} {
		encoded, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte(`"rpc_update_safety"`)) || bytes.Contains(encoded, []byte("PRIVATE_")) {
			t.Fatalf("%s diagnostics missing or leaked private content: %s", name, encoded)
		}
	}
	if runtime.ID != current[0].ID || runtime.Generation != current[0].Generation || len(a.manager.Snapshot()[0].Metadata) != 0 {
		t.Fatal("diagnostic snapshot modified stored runtime identity or metadata")
	}
}

func TestRuntimeUpdateDiagnosticsBoundBlockersIndependentlyOfCounts(t *testing.T) {
	a, _, server, cleanup := testAgent(t)
	defer cleanup()
	client := mustClient(t, a, a.manager.Snapshot()[0])
	server.SetResponseDelay("thread/name/set", time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const requests = codexadapter.MaxUpdateSafetyBlockers + 3
	done := make(chan error, requests)
	for i := range requests {
		go func(i int) { done <- client.RenameThread(ctx, fmt.Sprintf("PRIVATE_THREAD_%d", i), "PRIVATE_NAME") }(i)
	}
	waitFor(t, func() bool { return client.UpdateSafety().PendingRequests == requests })
	status := runtimeUpdateSafety(t, a.manager.snapshotWithUpdateSafety()[0])
	if status.PendingRequests != requests || len(status.Blockers) != codexadapter.MaxUpdateSafetyBlockers {
		t.Fatalf("unbounded or truncated count: %+v", status)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("PRIVATE_")) || len(encoded) > 8192 {
		t.Fatalf("unbounded or private diagnostic: %s", encoded)
	}
	cancel()
	for range requests {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled mutation = %v", err)
		}
	}
}

func TestRuntimeUpdateDiagnosticsPreserveMetadataWithoutAliases(t *testing.T) {
	for _, original := range []string{
		`{"custom":{"nested":[1,true]},"rpc_update_safety":{"stale":true}}`,
		"", "  ", "null", "  null  ",
	} {
		t.Run(original, func(t *testing.T) {
			metadata := make(json.RawMessage, len(original), len(original)+1024)
			copy(metadata, original)
			input := []protocol.Runtime{{ID: "runtime", Generation: 7, Metadata: metadata}}
			output := mergeRuntimeUpdateSafety(input, []RuntimeRPCUpdateSafety{{RuntimeID: "runtime", Generation: 7, UpdateSafetyStatus: codexadapter.UpdateSafetyStatus{Ready: true, UnconfirmedRequests: 2}}})
			if string(input[0].Metadata) != original || string(metadata) != original {
				t.Fatal("merging metadata changed the source record")
			}
			if runtimeUpdateSafety(t, output[0]).UnconfirmedRequests != 2 {
				t.Fatal("missing merged diagnostics")
			}
			if strings.Contains(original, "custom") {
				var merged map[string]json.RawMessage
				if err := json.Unmarshal(output[0].Metadata, &merged); err != nil {
					t.Fatal(err)
				}
				if string(merged["custom"]) != `{"nested":[1,true]}` || len(merged) != 2 {
					t.Fatalf("unrelated metadata changed: %s", output[0].Metadata)
				}
			}
			output[0].ID = "changed"
			output[0].Metadata[0] = '['
			if input[0].ID != "runtime" || string(input[0].Metadata) != original {
				t.Fatal("returned metadata or runtime aliases source storage")
			}
		})
	}
}

func TestRuntimeUpdateDiagnosticsSkipMalformedMetadataAndMismatchedGenerations(t *testing.T) {
	for _, original := range []string{`{`, `[]`, `"private"`, `true`, `7`} {
		input := []protocol.Runtime{{ID: "runtime", Generation: 7, Metadata: json.RawMessage(original)}}
		output := mergeRuntimeUpdateSafety(input, []RuntimeRPCUpdateSafety{{RuntimeID: "runtime", Generation: 7}})
		if string(output[0].Metadata) != original {
			t.Fatalf("invalid/non-object metadata replaced: %q => %q", original, output[0].Metadata)
		}
		output[0].Metadata[0] = '!'
		if string(input[0].Metadata) != original {
			t.Fatal("skipped metadata aliases source storage")
		}
	}
	input := []protocol.Runtime{
		{ID: "runtime", Generation: 7, Metadata: json.RawMessage(`{"custom":1}`)},
		{ID: "other", Generation: 8, Metadata: json.RawMessage(`{"custom":2}`)},
		{ID: "matching", Generation: 8},
	}
	output := mergeRuntimeUpdateSafety(input, []RuntimeRPCUpdateSafety{
		{RuntimeID: "runtime", Generation: 8, UpdateSafetyStatus: codexadapter.UpdateSafetyStatus{UnconfirmedRequests: 99}},
		{RuntimeID: "matching", Generation: 8, UpdateSafetyStatus: codexadapter.UpdateSafetyStatus{UnconfirmedRequests: 3}},
	})
	for i := range 2 {
		if string(output[i].Metadata) != string(input[i].Metadata) {
			t.Fatalf("diagnostics attached to mismatched identity: %+v", output[i])
		}
		output[i].Metadata[0] = '!'
		if input[i].Metadata[0] != '{' {
			t.Fatal("unmatched metadata aliases source storage")
		}
	}
	if runtimeUpdateSafety(t, output[2]).UnconfirmedRequests != 3 {
		t.Fatal("matching identity lost diagnostics")
	}
}

func runtimeUpdateSafety(t *testing.T, runtime protocol.Runtime) codexadapter.UpdateSafetyStatus {
	t.Helper()
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(runtime.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	var status codexadapter.UpdateSafetyStatus
	if raw, ok := metadata["rpc_update_safety"]; !ok {
		t.Fatal("runtime metadata lacks RPC update diagnostics")
	} else if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	return status
}
