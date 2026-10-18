package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestRenameSessionConfirmsSelectedIdentityAndDurableInventoryEvent(t *testing.T) {
	actor, client, server, _ := deletionFixture(t)
	before := actor.session
	result, err := actor.renameSession(context.Background(), client, "  Sessions / Release notes  ")
	if err != nil || result.Session == nil || result.Session.Name != "Sessions / Release notes" || result.Session.ID != before.ID || result.Session.CWD != before.CWD {
		t.Fatalf("rename = %#v %v", result, err)
	}
	if hasCall(server.Calls(), "thread/resume") || countCall(server.Calls(), "thread/name/set") != 1 {
		t.Fatalf("unexpected rename calls: %#v", server.Calls())
	}
	events, err := actor.agent.store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "session_state_changed" || events[0].SessionID != before.ID {
		t.Fatalf("rename event = %#v %v", events, err)
	}
	var saved protocol.Session
	if err := json.Unmarshal(events[0].Data, &saved); err != nil || saved.Name != result.Session.Name || saved.ID != before.ID {
		t.Fatalf("event snapshot = %#v %v", saved, err)
	}
	name := "Native client / title"
	actor.event(codexadapter.Event{Kind: "thread_name_updated", ThreadID: before.ThreadID, Name: &name})
	sessions, err := actor.agent.store.ListSessions(actor.runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].Name != name || sessions[0].ID != before.ID {
		t.Fatalf("native rename = %#v %v", sessions, err)
	}
}

func TestRenameSessionDoesNotReportSuccessWhenPersistenceFails(t *testing.T) {
	actor, client, _, _ := deletionFixture(t)
	if err := actor.agent.store.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := actor.renameSession(context.Background(), client, "Confirmed native title")
	if err == nil || result.Session != nil || result.Text != "" {
		t.Fatalf("unpersisted rename was successful: %#v %v", result, err)
	}
}

func TestRenameSessionRejectsInvalidTitlesBeforeRPC(t *testing.T) {
	actor, client, server, _ := deletionFixture(t)
	for _, name := range []string{"", "\n", "line\nbreak", strings.Repeat("界", 121)} {
		if _, err := actor.renameSession(context.Background(), client, name); err == nil {
			t.Errorf("invalid title accepted: %q", name)
		}
	}
	if hasCall(server.Calls(), "thread/name/set") {
		t.Fatal("invalid title reached native RPC")
	}
}
