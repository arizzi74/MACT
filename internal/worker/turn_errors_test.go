package worker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func publicTurnErrorActor(t *testing.T, a *Agent, runtime protocol.Runtime) *sessionActor {
	t.Helper()
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread-error", CWD: runtime.DefaultCWD, State: "running", Loaded: true, ActiveTurnID: "turn-live"})
	if err != nil {
		t.Fatal(err)
	}
	return &sessionActor{agent: a, runtime: runtime, session: session}
}

func publicTurnErrorResult(t *testing.T, a *Agent) protocol.Result {
	t.Helper()
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("public turn error outbox: %+v %v", events, err)
	}
	var result protocol.Result
	if events[0].Kind != "turn_failed" || json.Unmarshal(events[0].Data, &result) != nil || result.State != "failed" || result.TurnID != "turn-live" || result.Text != "" {
		t.Fatalf("public turn failure lost terminal identity: %+v", events[0])
	}
	return result
}

func TestTurnFailurePublicNoticeSurvivesNativeAdapterAndOutbox(t *testing.T) {
	const notice = "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. If you’re doing authorized security work that requires more cyber permissive safeguards, apply for Daybreak access via https://platform.openai.com/settings/organization/status-and-access before retrying."
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	actor := publicTurnErrorActor(t, a, runtime)
	actor.finalText, actor.hasFinalAnswer = "An incomplete candidate answer", true
	if err := server.Emit("turn/completed", map[string]any{
		"threadId": actor.session.ThreadID,
		"turn": map[string]any{"id": "turn-live", "status": "failed", "error": map[string]any{
			"message": notice, "additionalDetails": "PRIVATE_PROVIDER_BODY", "stack": "PRIVATE_STACK", "headers": map[string]string{"Authorization": "PRIVATE_HEADER"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-mustClient(t, a, runtime).Events():
		actor.event(event)
	case <-time.After(time.Second):
		t.Fatal("native public turn error was not received")
	}
	result := publicTurnErrorResult(t, a)
	if result.Error == nil || result.Error.Code != "" || result.Error.Message != notice || result.Error.Retryable {
		t.Fatalf("full public turn error was not retained: %+v", result.Error)
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events[0].Data), "PRIVATE_") || actor.session.ActiveTurnID != "" || actor.session.State != "failed" {
		t.Fatalf("public failure leaked diagnostics or changed terminal handling: %+v / %+v", events[0], actor.session)
	}
}

func TestTurnFailureUsesOnlyMatchingNonretryPublicNotice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		notices  []codexadapter.Event
		terminal string
		want     string
	}{
		{name: "public notice", notices: []codexadapter.Event{{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "Public reason\nRecovery instruction."}}, want: "Public reason\nRecovery instruction."},
		{name: "terminal reason wins", notices: []codexadapter.Event{{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "Earlier public reason"}}, terminal: "Final public reason", want: "Final public reason"},
		{name: "transient retry", notices: []codexadapter.Event{{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "Reconnecting... 2/5", WillRetry: true}}},
		{name: "other turn", notices: []codexadapter.Event{{Kind: "turn_error", TurnID: "turn-stale", PublicErrorMessage: "Other turn reason"}}},
		{name: "missing turn", notices: []codexadapter.Event{{Kind: "turn_error", PublicErrorMessage: "Uncorrelated reason"}}},
		{name: "private adapter failure", notices: []codexadapter.Event{{Kind: "turn_error", TurnID: "turn-live", Err: errors.New("PRIVATE_TRANSPORT_ERROR")}}},
		{name: "later retry recovery", notices: []codexadapter.Event{
			{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "An earlier attempt failed"},
			{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "Reconnecting... 2/5", WillRetry: true},
		}},
		{name: "no public reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, runtime, _, cleanup := testAgent(t)
			defer cleanup()
			actor := publicTurnErrorActor(t, a, runtime)
			for _, notice := range tc.notices {
				actor.event(notice)
			}
			if events, err := a.store.OutboxAfter(0); err != nil || len(events) != 0 || actor.session.ActiveTurnID != "turn-live" {
				t.Fatalf("a notice ended the turn before terminal confirmation: %+v %v", events, err)
			}
			actor.event(codexadapter.Event{Kind: "turn_failed", TurnID: "turn-live", PublicErrorMessage: tc.terminal, Err: errors.New("PRIVATE_ADAPTER_ERROR")})
			result := publicTurnErrorResult(t, a)
			if tc.want == "" {
				if result.Error != nil {
					t.Fatalf("unconfirmed or private reason was invented: %+v", result.Error)
				}
			} else if result.Error == nil || result.Error.Message != tc.want {
				t.Fatalf("public notice lost: %+v; want %q", result.Error, tc.want)
			}
		})
	}
}

func TestTurnFailurePublicNoticeClearsWhenOutputResumes(t *testing.T) {
	for _, resumed := range []codexadapter.Event{
		{Kind: "agent_message_completed", TurnID: "turn-live", Phase: "commentary", Text: "Recovered and continuing"},
		{Kind: "tool_call_started", TurnID: "turn-live", ItemID: "tool-recovered", Text: "Inspecting the project"},
	} {
		t.Run(resumed.Kind, func(t *testing.T) {
			a, runtime, _, cleanup := testAgent(t)
			defer cleanup()
			actor := publicTurnErrorActor(t, a, runtime)
			actor.event(codexadapter.Event{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "An earlier attempt failed"})
			actor.event(resumed)
			actor.event(codexadapter.Event{Kind: "turn_failed", TurnID: "turn-live"})
			events, err := a.store.OutboxAfter(0)
			if err != nil || len(events) != 2 {
				t.Fatalf("resumed output sequence: %+v %v", events, err)
			}
			var result protocol.Result
			if events[1].Kind != "turn_failed" || json.Unmarshal(events[1].Data, &result) != nil || result.Error != nil {
				t.Fatalf("recovered notice became a later failure reason: %+v", events[1])
			}
		})
	}
}

func TestTurnFailurePublicNoticeRedactsBeforeClipping(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t, `private-token-x+`)
	defer cleanup()
	actor := publicTurnErrorActor(t, a, runtime)
	text := strings.Repeat("界", 7980) + "private-token-" + strings.Repeat("x", 80) + "\n" + strings.Repeat("tail", 30)
	actor.event(codexadapter.Event{Kind: "turn_failed", TurnID: "turn-live", PublicErrorMessage: text})
	result := publicTurnErrorResult(t, a)
	if result.Error == nil || !utf8.ValidString(result.Error.Message) || len([]rune(result.Error.Message)) != 8000 || !strings.HasSuffix(result.Error.Message, "…") || strings.Contains(result.Error.Message, "private-token") || !strings.Contains(result.Error.Message, "[REDACTED]") {
		t.Fatal("public turn error was not bounded after complete secret redaction")
	}
}

func TestTurnFailurePublicNoticeNormalizesInvalidText(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	actor := publicTurnErrorActor(t, a, runtime)
	actor.event(codexadapter.Event{Kind: "turn_failed", TurnID: "turn-live", PublicErrorMessage: "Bad\x00UTF8\xff reason"})
	result := publicTurnErrorResult(t, a)
	if result.Error == nil || result.Error.Message != "Bad\uFFFDUTF8\uFFFD reason" {
		t.Fatalf("unsafe public error text: %+v", result.Error)
	}
}

func TestTurnFailurePublicNoticeDoesNotCarryAcrossSuccessfulTurns(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	actor := publicTurnErrorActor(t, a, runtime)
	actor.event(codexadapter.Event{Kind: "turn_error", TurnID: "turn-live", PublicErrorMessage: "An earlier attempt failed"})
	actor.event(codexadapter.Event{Kind: "turn_completed", TurnID: "turn-live"})
	actor.event(codexadapter.Event{Kind: "turn_started", TurnID: "turn-next"})
	actor.event(codexadapter.Event{Kind: "turn_failed", TurnID: "turn-next"})
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 3 {
		t.Fatalf("turn sequence: %+v %v", events, err)
	}
	for _, event := range []protocol.Event{events[0], events[2]} {
		var result protocol.Result
		if json.Unmarshal(event.Data, &result) != nil || result.Error != nil {
			t.Fatalf("old notice survived a successful terminal boundary: %+v", event)
		}
	}
}
