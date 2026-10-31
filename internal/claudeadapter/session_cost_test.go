package claudeadapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeSessionCostDecodesOnlyNativeSummaryMoney(t *testing.T) {
	for _, tc := range []struct {
		text string
		want float64
		ok   bool
	}{
		{"Total cost:            $1.25\nPRIVATE OTHER SUMMARY", 1.25, true},
		{"Total cost: $0.0000", 0, true},
		{"Total cost: $0.1255 (at your organization's configured rates)\r\n", .1255, true},
		{"Total cost: $2.50 (costs may be inaccurate due to usage of unknown models)", 2.5, true},
		{"Total cost: $-1", 0, false}, {"Total cost: $NaN", 0, false}, {"Total cost: $1e4", 0, false},
		{"Total cost: €12.5", 0, false}, {"Total cost: $1.2 PRIVATE", 0, false},
		{"Some model: $100\nTotal cost: $10", 0, false}, {"Total cost: $0.123456789", 0, false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"text": tc.text})
			value, rounding, ok := decodeSessionCost(raw)
			if ok != tc.ok || ok && (value != tc.want || rounding <= 0) {
				t.Fatalf("decoded %v %v %v", value, rounding, ok)
			}
		})
	}
	for _, raw := range []string{`null`, `{}`, `{"text":12}`, `{"total_cost_usd":5}`, `{"text":"` + strings.Repeat("x", 64<<10) + `"}`} {
		if _, _, ok := decodeSessionCost([]byte(raw)); ok {
			t.Fatal("unstructured or oversized summary accepted")
		}
	}
}

func TestClaudeSessionCostResultKeepsMovingBelowHistoricalHighWater(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	p := nativeStatusProcess(r, thread.ID)
	for i, value := range []string{"23.85", "0.25", "1.5"} {
		acceptStatusTurn(t, r, thread.ID)
		feedStatus(p, `{"type":"result","uuid":"cost-`+value+`","subtype":"success","total_cost_usd":`+value+`}`)
		status := readStatus(t, r, thread.ID)
		wantNumber(t, status.Cost.ReportedSessionUSD, []float64{23.85, .25, 1.5}[i])
		if status.Cost.ObservedAt <= 0 {
			t.Fatal("reported cost has no observation timestamp")
		}
		if i > 0 {
			wantNumber(t, status.Cost.KnownSessionUSD, 23.85)
			if !status.Cost.Incomplete || status.Cost.SessionUSD != nil || status.Cost.LastTurnUSD != nil {
				t.Fatal("overlapping native counters were added or lost history became exact")
			}
		}
	}
	acceptStatusTurn(t, r, thread.ID)
	feedStatus(p, `{"type":"result","uuid":"crash","subtype":"error_during_execution","is_error":true,"total_cost_usd":0}`)
	wantNumber(t, readStatus(t, r, thread.ID).Cost.ReportedSessionUSD, 1.5)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = startTest(t, cfg)
	status := readStatus(t, r, thread.ID)
	wantNumber(t, status.Cost.ReportedSessionUSD, 1.5)
	wantNumber(t, status.Cost.KnownSessionUSD, 23.85)
	if !status.Cost.Incomplete {
		t.Fatal("restart turned a partial native counter into a full total")
	}
}

// A deterministic native-control transport allows the precise-result and
// retired-process races to happen while a cost response is in flight.
type sessionCostWriter struct {
	p      *process
	before func()
	raw    string
	fail   bool
	calls  int
}

func (w *sessionCostWriter) Close() error { return nil }
func (w *sessionCostWriter) Write(data []byte) (int, error) {
	var frame struct {
		ID      string `json:"request_id"`
		Request struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if json.Unmarshal(data, &frame) != nil || frame.Request.Subtype != "get_session_cost" {
		return 0, errors.New("unexpected non-cost control")
	}
	w.calls++
	if w.before != nil {
		w.before()
	}
	w.p.controlMu.Lock()
	reply := w.p.controls[frame.ID]
	w.p.controlMu.Unlock()
	value := controlReply{data: []byte(w.raw)}
	if w.fail {
		value.err = errors.New("PRIVATE native failure")
	}
	reply <- value
	return len(data), nil
}

func TestClaudeSessionCostProbeFencesResultsRetirementAndFailures(t *testing.T) {
	for _, mode := range []string{"fresh", "rounded", "failure", "malformed", "busy", "result-race", "retired", "idle", "before-resume"} {
		t.Run(mode, func(t *testing.T) {
			r := startTest(t, testConfig(t))
			thread := startThread(t, r)
			acceptStatusTurn(t, r, thread.ID)
			p := nativeStatusProcess(r, thread.ID)
			p.done, p.controls = make(chan struct{}), make(map[string]chan controlReply)
			p.costReady = mode != "before-resume"
			writer := &sessionCostWriter{p: p, raw: `{"text":"Total cost: $1.25\nPRIVATE SUMMARY"}`}
			p.in = writer
			s := r.server
			s.mu.Lock()
			target := s.threads[thread.ID]
			prior := 23.85
			if mode == "rounded" {
				prior = 1.254
			}
			target.ClaudeStatus.Cost.SessionUSD = &prior
			old := .5
			target.ClaudeStatus.Cost.ReportedSessionUSD = &old
			s.runners[thread.ID] = p
			if mode == "idle" {
				target.active().Status = "completed"
			}
			s.mu.Unlock()
			t.Cleanup(func() { s.mu.Lock(); delete(s.runners, thread.ID); s.mu.Unlock() })
			switch mode {
			case "failure":
				writer.fail = true
			case "malformed":
				writer.raw = `{"text":"PRIVATE NO MONEY"}`
			case "busy":
				p.writeMu.Lock()
				defer p.writeMu.Unlock()
			case "result-race":
				writer.before = func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					p.costRevision++
					p.observeResult(target, []byte(`{"total_cost_usd":30.125678}`), "success")
				}
			case "retired":
				writer.before = func() { s.mu.Lock(); delete(s.runners, thread.ID); s.mu.Unlock() }
			}
			p.refreshSessionCost()
			status := readStatus(t, r, thread.ID)
			want := .5
			if mode == "fresh" || mode == "rounded" {
				want = 1.25
			}
			if mode == "result-race" {
				want = 30.125678
			}
			wantNumber(t, status.Cost.ReportedSessionUSD, want)
			if mode == "fresh" {
				wantNumber(t, status.Cost.KnownSessionUSD, 23.85)
				if !status.Cost.Incomplete {
					t.Fatal("lost accounting was hidden")
				}
			}
			if mode == "rounded" && status.Cost.Incomplete {
				t.Fatal("native two-decimal rounding was treated as a reset")
			}
			if mode == "idle" || mode == "before-resume" || mode == "busy" {
				if writer.calls != 0 {
					t.Fatal("unready, idle or busy process was probed")
				}
			}
			select {
			case <-p.done:
				t.Fatal("cost refresh stopped the native conversation")
			default:
			}
		})
	}
}

func TestClaudeSessionCostObserverRefreshesActiveTurnWithoutPromptOrBudgetFetch(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if _, err := r.StartTurn(t.Context(), thread.ID, "block"); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_started")
	deadline := time.Now().Add(5 * time.Second)
	for {
		actions, _ := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
		if strings.Contains(string(actions), `"user":"block"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native test query was not submitted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.server.mu.Lock()
	p := r.server.runners[thread.ID]
	prior := 23.85
	r.server.threads[thread.ID].ensureStatus().Cost.SessionUSD = &prior
	r.server.mu.Unlock()
	feedStatus(p, `{"type":"assistant","message":{"id":"cost-step","content":[],"usage":{"input_tokens":10}}}`)
	deadline = time.Now().Add(5 * time.Second)
	for readStatus(t, r, thread.ID).Cost.ReportedSessionUSD == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	status := readStatus(t, r, thread.ID)
	wantNumber(t, status.Cost.ReportedSessionUSD, 1.25)
	wantNumber(t, status.Cost.KnownSessionUSD, 23.85)
	r.server.mu.Lock()
	active := r.server.threads[thread.ID].active() != nil
	r.server.mu.Unlock()
	if !active {
		t.Fatal("observation ended the user's active turn")
	}
	actions, err := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(actions)
	if strings.Count(text, `"costProbe":true`) != 1 || strings.Count(text, `"user":`) != 1 || strings.Contains(text, `"usageProbe"`) || !strings.Contains(text, `"foreground":true`) {
		t.Fatalf("cost observer submitted input, fetched budget, or missed active process: %s", text)
	}
}
