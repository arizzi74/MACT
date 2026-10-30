package claudeadapter

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

func readStatus(t *testing.T, r *Runtime, threadID string) ClaudeStatus {
	t.Helper()
	thread, err := r.ReadThread(context.Background(), threadID, false)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Status ClaudeStatus `json:"claudeStatus"`
	}
	if err := json.Unmarshal(thread.Raw, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Status
}

func nativeStatusProcess(r *Runtime, threadID string) *process {
	return &process{server: r.server, threadID: threadID, tools: make(map[string]map[string]any), streamItem: make(map[int]string)}
}

func acceptStatusTurn(t *testing.T, r *Runtime, threadID string) string {
	t.Helper()
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.threads[threadID]
	if thread.active() != nil {
		t.Fatal("test attempted an overlapping turn")
	}
	turn := &savedTurn{ID: uuid.NewString(), Status: "inProgress", StartedAt: time.Now().Unix(), Items: []map[string]any{}}
	thread.Turns = append(thread.Turns, turn)
	thread.ensureStatus().Cost.LastTurnUSD = nil
	if err := s.save(thread); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

func feedStatus(p *process, raw string) {
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if thread := p.server.threads[p.threadID]; thread.active() != nil && thread.active().ID != p.lifecycle.turnID {
		p.beginLifecycle(thread, thread.active().ID)
		p.lifecycle.submitted = true
	}
	p.handle([]byte(raw))
}

func wantNumber(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-12 {
		t.Fatalf("number=%v want=%g", got, want)
	}
}
func wantTokens(t *testing.T, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("tokens=%v want=%d", got, want)
	}
}

func TestClaudeStatusTracksReportedSessionTotalsAndLatestContextAcrossRestart(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	p := nativeStatusProcess(r, thread.ID)
	initial := readStatus(t, r, thread.ID)
	if initial.Provider != "claude" || initial.Model != nil || initial.PermissionMode != nil || initial.Cost.SessionUSD != nil || initial.Context.MaxTokens != nil {
		t.Fatalf("invented initial status: %+v", initial)
	}
	config, err := r.ReadEffectiveConfig(context.Background(), "")
	if err != nil || config.Model != "" || config.ApprovalPolicy != "" {
		t.Fatalf("invented effective config: %+v %v", config, err)
	}
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"system","subtype":"init","model":"reported-model","permissionMode":"acceptEdits"}`)
	feedStatus(p, `{"type":"assistant","message":{"id":"step-one","model":"reported-model","usage":{"input_tokens":120,"cache_read_input_tokens":30,"cache_creation_input_tokens":50},"content":[{"type":"text","text":"done"}]}}`)
	feedStatus(p, `{"type":"result","uuid":"result-one","subtype":"success","total_cost_usd":0.2,"usage":{"input_tokens":999999},"modelUsage":{"reported-model":{"contextWindow":1000}}}`)
	status := readStatus(t, r, thread.ID)
	wantNumber(t, status.Cost.SessionUSD, .2)
	wantNumber(t, status.Cost.LastTurnUSD, .2)
	wantTokens(t, status.Context.UsedTokens, 200)
	wantTokens(t, status.Context.MaxTokens, 1000)
	if status.Model == nil || *status.Model != "reported-model" || status.PermissionMode == nil || *status.PermissionMode != "acceptEdits" {
		t.Fatalf("native identity=%+v", status)
	}
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"assistant","message":{"id":"step-two","model":"reported-model","usage":{"input_tokens":250,"cache_read_input_tokens":50,"cache_creation_input_tokens":0},"content":[]}}`)
	feedStatus(p, `{"type":"result","uuid":"result-two","subtype":"success","total_cost_usd":0.35}`)
	status = readStatus(t, r, thread.ID)
	wantNumber(t, status.Cost.SessionUSD, .35)
	wantNumber(t, status.Cost.LastTurnUSD, .15)
	wantTokens(t, status.Context.UsedTokens, 300)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	p2 := nativeStatusProcess(r2, thread.ID)
	status = readStatus(t, r2, thread.ID)
	wantNumber(t, status.Cost.SessionUSD, .35)
	wantNumber(t, status.Cost.LastTurnUSD, .15)
	wantTokens(t, status.Context.UsedTokens, 300)
	active := acceptStatusTurn(t, r2, thread.ID)
	feedStatus(p2, `{"type":"result","uuid":"result-two","subtype":"success","total_cost_usd":0.35}`)
	r2.server.mu.Lock()
	if got := r2.server.threads[thread.ID].active(); got == nil || got.ID != active {
		t.Fatal("a replayed result completed the successor turn")
	}
	r2.server.mu.Unlock()
	feedStatus(p2, `{"type":"result","uuid":"result-three","subtype":"success","total_cost_usd":0.5}`)
	status = readStatus(t, r2, thread.ID)
	wantNumber(t, status.Cost.SessionUSD, .5)
	wantNumber(t, status.Cost.LastTurnUSD, .15)
	other := startThread(t, r2)
	if otherStatus := readStatus(t, r2, other.ID); otherStatus.Cost.SessionUSD != nil || otherStatus.Context.UsedTokens != nil {
		t.Fatal("session status crossed thread identities")
	}
}

func TestClaudeStatusDistinguishesReportedZeroAndRejectsInvalidMetrics(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	p := nativeStatusProcess(r, thread.ID)
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"assistant","message":{"id":"zero","usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"content":[]}}`)
	feedStatus(p, `{"type":"result","uuid":"zero","subtype":"success","total_cost_usd":0}`)
	status := readStatus(t, r, thread.ID)
	wantNumber(t, status.Cost.SessionUSD, 0)
	wantNumber(t, status.Cost.LastTurnUSD, 0)
	wantTokens(t, status.Context.UsedTokens, 0)
	if status.Context.MaxTokens != nil {
		t.Fatal("guessed context window")
	}
	for i, raw := range []string{`-1`, `"NaN"`, `1e309`, `null`, `"0.12"`} {
		acceptStatusTurn(t, r, thread.ID)
		feedStatus(p, `{"type":"assistant","message":{"id":"invalid-`+string(rune('a'+i))+`","usage":{"input_tokens":-1,"cache_read_input_tokens":1},"content":[]}}`)
		feedStatus(p, `{"type":"result","subtype":"success","total_cost_usd":`+raw+`,"modelUsage":{"x":{"contextWindow":-50}}}`)
		status = readStatus(t, r, thread.ID)
		wantNumber(t, status.Cost.SessionUSD, 0)
		wantTokens(t, status.Context.UsedTokens, 0)
		if status.Cost.LastTurnUSD != nil || status.Context.MaxTokens != nil {
			t.Fatalf("invalid metrics accepted: %+v", status)
		}
	}
	for _, raw := range []string{`{"input_tokens":1.5}`, `{"input_tokens":9007199254740991,"cache_read_input_tokens":1}`, `{"cache_read_input_tokens":8}`, `{"input_tokens":5,"cache_creation_input_tokens":null}`} {
		if contextTokens(json.RawMessage(raw)) != nil {
			t.Fatalf("invalid context accepted: %s", raw)
		}
	}
}

func TestClaudeStatusMarksCostUnknownAfterCrashOrResetAndNeverGuessesCatchup(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	p := nativeStatusProcess(r, thread.ID)
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"result","uuid":"paid","subtype":"success","total_cost_usd":0.75}`)
	for _, raw := range []string{`{"type":"result","uuid":"crashed","subtype":"error_during_execution","is_error":true,"total_cost_usd":0}`, `{"type":"result","uuid":"older","subtype":"success","total_cost_usd":0.25}`, `{"type":"result","uuid":"caught-up","subtype":"success","total_cost_usd":1.25}`} {
		acceptStatusTurn(t, r, thread.ID)
		feedStatus(p, raw)
		status := readStatus(t, r, thread.ID)
		if status.Cost.SessionUSD != nil || status.Cost.LastTurnUSD != nil || !status.Cost.Incomplete {
			t.Fatal("incomplete native accounting became a precise session/turn cost")
		}
	}
	r.server.mu.Lock()
	wantNumber(t, r.server.threads[thread.ID].ClaudeStatus.Cost.SessionUSD, 1.25)
	r.server.mu.Unlock()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = startTest(t, cfg)
	if status := readStatus(t, r, thread.ID); status.Cost.SessionUSD != nil || !status.Cost.Incomplete {
		t.Fatal("accounting uncertainty did not survive restart")
	}
	other := startThread(t, r)
	p2 := nativeStatusProcess(r, other.ID)
	acceptStatusTurn(t, r, other.ID)
	feedStatus(p2, `{"type":"result","subtype":"error_during_execution","is_error":true,"total_cost_usd":0}`)
	if status := readStatus(t, r, other.ID); status.Cost.SessionUSD != nil {
		t.Fatal("first crash report became a known zero charge")
	}
}

func TestClaudeStatusDetectsSuccessfulNativeCostResetWithoutPriorCrash(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	p := nativeStatusProcess(r, thread.ID)
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"result","uuid":"before-reset","subtype":"success","total_cost_usd":0.75}`)
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"result","uuid":"after-reset","subtype":"success","total_cost_usd":0.1}`)
	status := readStatus(t, r, thread.ID)
	if status.Cost.SessionUSD != nil || status.Cost.LastTurnUSD != nil || !status.Cost.Incomplete {
		t.Fatal("a successful lower native total exposed a stale session cost")
	}
	wantNumber(t, status.Cost.KnownSessionUSD, .75)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = startTest(t, r.server.cfg)
	restored := readStatus(t, r, thread.ID)
	wantNumber(t, restored.Cost.KnownSessionUSD, .75)
	if restored.Cost.SessionUSD != nil || !restored.Cost.Incomplete {
		t.Fatal("partial accounting became an exact cost after restart")
	}
}

func TestClaudeStatusBalanceIsClonedEphemeralAndFencedBySettings(t *testing.T) {
	cfg := testConfig(t)
	settings := LaunchSettings{Model: "configured", PermissionMode: "plan", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "PRIVATE_STATUS_TOKEN"}}
	cfg.LoadSettings = func() (LaunchSettings, error) { return settings, nil }
	r := startTest(t, cfg)
	thread := startThread(t, r)
	generation := r.BalanceGeneration()
	balance := &CreditBalance{Remaining: 12.5, Unit: "USD", ObservedAt: 1800000000}
	if err := r.SetBalanceAt(generation, balance); err != nil {
		t.Fatal(err)
	}
	balance.Remaining = 999
	status := readStatus(t, r, thread.ID)
	if status.Balance == nil || status.Balance.Remaining != 12.5 || status.Model == nil || *status.Model != "configured" || status.PermissionMode == nil || *status.PermissionMode != "plan" {
		t.Fatalf("safe status=%+v", status)
	}
	for _, invalid := range []*CreditBalance{{Remaining: -1, Unit: "USD"}, {Remaining: math.Inf(1), Unit: "USD"}, {Remaining: math.NaN(), Unit: "USD"}, {Remaining: 2, Unit: "tokens"}} {
		if err := r.SetBalance(invalid); err == nil {
			t.Fatal("invalid balance accepted")
		}
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, thread.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "12.5") || strings.Contains(string(data), "PRIVATE_STATUS_TOKEN") {
		t.Fatal("account data persisted in conversation status")
	}
	if err := r.WithIdleSettingsUpdate(func() error { settings = LaunchSettings{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBalanceAt(generation, &CreditBalance{Remaining: 10, Unit: "USD"}); err == nil {
		t.Fatal("old-account request repopulated the new account balance")
	}
	status = readStatus(t, r, thread.ID)
	if status.Balance != nil || status.Model != nil || status.PermissionMode != nil {
		t.Fatalf("idle settings switch retained stale fields: %+v", status)
	}
	if err := r.SetBalance(&CreditBalance{Remaining: 0, Unit: "credits"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	if status = readStatus(t, r2, thread.ID); status.Balance != nil {
		t.Fatal("account balance survived adapter restart")
	}
}

func TestClaudeStatusStreamMessageStartUpdatesCurrentContextAndNotifications(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	p := nativeStatusProcess(r, thread.ID)
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"stream_event","event":{"type":"message_start","message":{"id":"stream-status","model":"live-model","usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30}}}}`)
	status := readStatus(t, r, thread.ID)
	wantTokens(t, status.Context.UsedTokens, 60)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-r.Events():
			if event.Method == "thread/claudeStatus/updated" {
				var update struct {
					ThreadID string       `json:"threadId"`
					Status   ClaudeStatus `json:"claudeStatus"`
				}
				if json.Unmarshal(event.Params, &update) != nil || update.ThreadID != thread.ID {
					t.Fatal("status notification escaped session scope")
				}
				wantTokens(t, update.Status.Context.UsedTokens, 60)
				return
			}
		case <-timer.C:
			t.Fatal("no live status notification")
		}
	}
}

func TestClaudeStatusBootstrapRPCAndResumeExposeOnlySafeSnapshot(t *testing.T) {
	cfg := testConfig(t)
	cfg.LoadSettings = func() (LaunchSettings, error) {
		return LaunchSettings{Model: "configured-model", PermissionMode: "default", Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "NEVER_PUBLIC_STATUS_TOKEN", "ANTHROPIC_BASE_URL": "https://private.example/"}}, nil
	}
	r := startTest(t, cfg)
	thread := startThread(t, r)
	peer := r.server.newPeer(nil)
	defer peer.close()
	r.server.receive(peer, []byte(`{"id":"init","method":"initialize","params":{}}`))
	<-peer.queue
	request, _ := json.Marshal(map[string]any{"id": "status", "method": "thread/status/read", "params": map[string]any{"threadId": thread.ID}})
	r.server.receive(peer, request)
	var wire struct {
		Result struct {
			ThreadID string       `json:"threadId"`
			Status   ClaudeStatus `json:"claudeStatus"`
		} `json:"result"`
	}
	response := <-peer.queue
	if json.Unmarshal(response, &wire) != nil || wire.Result.ThreadID != thread.ID || wire.Result.Status.Model == nil || *wire.Result.Status.Model != "configured-model" {
		t.Fatalf("bootstrap=%s", response)
	}
	if strings.Contains(string(response), "NEVER_PUBLIC_STATUS_TOKEN") || strings.Contains(string(response), "private.example") {
		t.Fatal("bootstrap exposed account settings")
	}
	resumed, err := r.ResumeThread(context.Background(), thread.ID, codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Status *ClaudeStatus `json:"claudeStatus"`
	}
	if json.Unmarshal(resumed.Raw, &snapshot) != nil || snapshot.Status == nil || snapshot.Status.PermissionMode == nil || *snapshot.Status.PermissionMode != "default" {
		t.Fatalf("resume snapshot=%s", resumed.Raw)
	}
}
