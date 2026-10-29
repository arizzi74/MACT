package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

func TestControlPlaneRedactedQuestionChoicePreservesNativeValue(t *testing.T) {
	e := newSessionModeControlConfig(t, func(cfg *config.WorkerConfig) {
		cfg.RedactPatterns = []string{testPrivatePattern}
		cfg.Name, cfg.Runtimes[0].Name = "PRIVATE_WORKER", "PRIVATE_RUNTIME"
	})
	session := e.sessions["thread-beta"]
	selectControlSession(t, e.gw, e.telegram, 300, e.runtime, 2)
	e.prompt(t, 302, "Start work")
	var turn string
	waitControl(t, func() bool {
		turn = currentActive(t, e.local, e.runtime, session.ThreadID)
		return turn != ""
	})
	if err := e.codex.Emit("item/completed", map[string]any{
		"threadId": session.ThreadID, "turnId": turn,
		"item": map[string]any{"id": "private-question", "type": "agentMessage", "status": "completed", "phase": "final_answer", "delivery": "async", "text": "PRIVATE_SUMMARY", "questions": []map[string]any{{"title": "PRIVATE_PROMPT", "options": []string{"PRIVATE_FIRST", "PRIVATE_SECOND"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	var callback string
	waitControl(t, func() bool {
		callback = wizardButton(e.telegram, "[REDACTED]", "[REDACTED] (option 2)")
		return callback != ""
	})
	var raw []byte
	if err := e.probe.QueryRowContext(e.ctx, `SELECT request_payload FROM approvals WHERE session_id=? AND codex_request_id='async:private-question'`, session.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	assertNoPrivateText(t, raw)
	postCallback(t, e.gw.server.Client(), e.gw.server.URL, "secret", 303, callback)
	waitControl(t, func() bool {
		for _, call := range e.codex.Calls() {
			if call.Method != "turn/steer" {
				continue
			}
			var input struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			if json.Unmarshal(call.Params, &input) == nil && input.ThreadID == session.ThreadID && len(input.Input) == 1 && input.Input[0].Text == "> PRIVATE_PROMPT\n\nPRIVATE_SECOND" {
				return true
			}
		}
		return false
	})
	var leaked int
	if err := e.probe.QueryRowContext(e.ctx, `SELECT count(*) FROM events WHERE instr(payload, 'PRIVATE_') > 0`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("private display text persisted at gateway: count=%d, %v", leaked, err)
	}
	e.telegram.mu.Lock()
	defer e.telegram.mu.Unlock()
	for _, action := range e.telegram.actions {
		if strings.Contains(action.text, "PRIVATE_") {
			t.Fatalf("private worker text reached Telegram: %s", action.text)
		}
	}
}

func TestControlPlaneDefaultCredentialRedactionReachesTelegramAndGateway(t *testing.T) {
	e := newSessionModeControl(t)
	session := e.sessions["thread-beta"]
	selectControlSession(t, e.gw, e.telegram, 700, e.runtime, 2)
	e.prompt(t, 702, "Start synthetic credential boundary test")
	var turn string
	waitControl(t, func() bool { turn = currentActive(t, e.local, e.runtime, session.ThreadID); return turn != "" })
	var tokens []string
	for _, token := range syntheticCredentialSamples() {
		tokens = append(tokens, token)
	}
	text := "Synthetic boundary result: " + strings.Join(tokens, "\n")
	if err := e.codex.Emit("item/completed", map[string]any{
		"threadId": session.ThreadID, "turnId": turn,
		"item": map[string]any{"id": "synthetic-boundary", "type": "agentMessage", "status": "completed", "phase": "final_answer", "text": text},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.codex.Emit("turn/completed", map[string]any{"threadId": session.ThreadID, "turnId": turn, "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	waitControl(t, func() bool {
		return e.telegram.hasText("Synthetic boundary result:") && e.telegram.hasText("[REDACTED]")
	})
	for _, token := range tokens {
		var count int
		if err := e.probe.QueryRowContext(e.ctx, `SELECT count(*) FROM events WHERE instr(payload, ?) > 0`, token).Scan(&count); err != nil || count != 0 {
			t.Fatal("credential entered gateway event persistence", err)
		}
	}
	e.telegram.mu.Lock()
	defer e.telegram.mu.Unlock()
	for _, action := range e.telegram.actions {
		for _, token := range tokens {
			if strings.Contains(action.text, token) {
				t.Fatal("credential reached Telegram delivery")
			}
		}
	}
}
