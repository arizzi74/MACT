package codexadapter

import (
	"context"
	"testing"
)

func TestClaudeStatusReplyMustBelongToRequestedSession(t *testing.T) {
	for _, thread := range []string{"selected", "other"} {
		t.Run(thread, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() { _, err := client.ReadClaudeStatus(context.Background(), "selected"); done <- err }()
			request := fake.next(t)
			if method(t, request) != "thread/status/read" || string(params(t, request)["threadId"]) != `"selected"` {
				t.Fatal("status request was not scoped")
			}
			fake.respond(t, request, map[string]any{"threadId": thread, "claudeStatus": map[string]any{"provider": "claude", "cost": map[string]any{"sessionUSD": 0}}})
			if err := <-done; (err == nil) != (thread == "selected") {
				t.Fatalf("reply thread %s: %v", thread, err)
			}
		})
	}
}
