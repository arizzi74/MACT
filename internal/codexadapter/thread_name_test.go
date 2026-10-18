package codexadapter

import (
	"encoding/json"
	"testing"
)

func TestThreadNameUpdatedNotificationNormalizesTitle(t *testing.T) {
	event := newEvent("thread/name/updated", json.RawMessage(`{"threadId":"original","name":"Release / notes"}`))
	if event.Kind != "thread_name_updated" || event.ThreadID != "original" || event.Name == nil || *event.Name != "Release / notes" || event.Unknown {
		t.Fatalf("name event = %#v", event)
	}
	missing := newEvent("thread/name/updated", json.RawMessage(`{"threadId":"original"}`))
	if missing.Name != nil {
		t.Fatalf("missing name fabricated an update: %#v", missing)
	}
	canonical := newEvent("thread/name/updated", json.RawMessage("{\"threadId\":\"original\",\"threadName\":\"Canonical title\"}"))
	if canonical.Name == nil || *canonical.Name != "Canonical title" {
		t.Fatalf("canonical notification title = %#v", canonical)
	}
	cleared := newEvent("thread/name/updated", json.RawMessage("{\"threadId\":\"original\",\"threadName\":null}"))
	if cleared.Name == nil || *cleared.Name != "" {
		t.Fatalf("cleared native title = %#v", cleared)
	}
}
