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

	"github.com/iaia/telegramgw/internal/codexadapter"
)

const nativeUsageFixture = `{"subscription_type":"enterprise","rate_limits_available":true,"rate_limits":{"extra_usage":{"is_enabled":true,"monthly_limit":50000,"used_credits":12345,"currency":"USD","decimal_places":2}},"session":{"total_cost_usd":99999},"behaviors":{"private":"PRIVATE_TRANSCRIPT"},"account":{"key":"PRIVATE_IDENTITY"}}`

func TestNativeUsageProjectsMinorUnitsWithoutSessionCostOrPrivateData(t *testing.T) {
	usage, err := decodeNativeUsage(json.RawMessage(nativeUsageFixture), time.Now())
	if err != nil || usage.Plan != "enterprise" || usage.APISpend == nil {
		t.Fatal(usage, err)
	}
	spend := usage.APISpend
	if *spend.Limit != 500 || *spend.Spent != 123.45 || math.Abs(*spend.Remaining-376.55) > 1e-8 {
		t.Fatalf("minor-unit conversion: %+v", spend)
	}
	wire, _ := json.Marshal(usage)
	for _, private := range []string{"99999", "PRIVATE_TRANSCRIPT", "PRIVATE_IDENTITY", "behaviors", "session"} {
		if strings.Contains(string(wire), private) {
			t.Fatalf("projection includes private or unrelated data: %s", wire)
		}
	}
}

func TestNativeUsageReusesLoadedSessionWithoutInterruptingIt(t *testing.T) {
	for _, scenario := range []string{"success", "timeout", "busy write"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_USAGE="+nativeUsageFixture)
			if scenario == "timeout" {
				cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_HANG_USAGE=1")
			}
			r := startTest(t, cfg)
			thread := startThread(t, r)
			if _, err := r.StartTurn(t.Context(), thread.ID, "approval"); err != nil {
				t.Fatal(err)
			}
			approval := awaitRequest(t, r) // Initialization and input submission finished.
			r.server.mu.Lock()
			process := r.server.runners[thread.ID]
			r.server.mu.Unlock()
			if scenario == "busy write" {
				process.writeMu.Lock()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			usage, err := r.ReadNativeUsage(ctx)
			cancel()
			if scenario == "busy write" {
				process.writeMu.Unlock()
			}
			if scenario == "success" && (err != nil || usage == nil || usage.APISpend == nil) {
				t.Fatal("loaded usage read failed", usage, err)
			}
			if scenario != "success" && err == nil {
				t.Fatal("failed read was not reported")
			}
			select {
			case <-process.done:
				t.Fatal("account read killed the user's process")
			default:
			}
			if err := r.ReplyApproval(t.Context(), approval.RequestID, codexadapter.ApprovalResponse{Decision: "accept"}); err != nil {
				t.Fatal(err)
			}
			awaitEvent(t, r, "turn_completed")
			actions, err := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(actions), `"launch":true`) != 1 || strings.Count(string(actions), `"user":`) != 1 {
				t.Fatal("usage launched another CLI or submitted another prompt", string(actions))
			}
			if scenario == "success" && !strings.Contains(string(actions), `"foreground":true`) {
				t.Fatal("usage did not reuse the native cache")
			}
		})
	}
}

func TestNativeUsageDistinguishesUnknownZeroExceededDisabledAndUnsupportedCurrency(t *testing.T) {
	for _, tc := range []struct {
		name, limit, spent, currency string
		enabled                      bool
		want, left                   bool
	}{
		{"zero", "0", "0", "USD", true, true, true},
		{"exceeded", "100", "200", "USD", true, true, true},
		{"disabled", "100", "20", "USD", false, true, true},
		{"unknown limit", "null", "20", "USD", true, true, false},
		{"unknown spend", "100", "null", "USD", true, true, false},
		{"missing currency", "100", "20", "", true, false, false},
		{"other currency", "100", "20", "EUR", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(nativeUsageFixture, "50000", tc.limit), "12345", tc.spent), `"currency":"USD"`, `"currency":"`+tc.currency+`"`)
			if !tc.enabled {
				raw = strings.Replace(raw, `"is_enabled":true`, `"is_enabled":false`, 1)
			}
			u, err := decodeNativeUsage(json.RawMessage(raw), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if (u.APISpend != nil) != tc.want {
				t.Fatalf("unsupported currency became USD: %+v", u)
			}
			if !tc.want {
				return
			}
			if u.APISpend.Enabled != tc.enabled || (u.APISpend.Remaining != nil) != tc.left {
				t.Fatalf("unknown was treated as zero: %+v", u.APISpend)
			}
			if tc.name == "exceeded" && *u.APISpend.Remaining != 0 {
				t.Fatal("negative remaining balance")
			}
		})
	}
	for _, raw := range []string{`null`, `[]`, `{bad`, `{"rate_limits":"invalid"}`} {
		if _, err := decodeNativeUsage(json.RawMessage(raw), time.Now()); err == nil {
			t.Fatalf("malformed native reply accepted: %s", raw)
		}
	}
}

func TestNativeUsageWindowsUseNativePercentAndResetWithoutFabricatingUnavailableLimits(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(time.Hour).Format(time.RFC3339Nano)
	raw := `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":24,"resets_at":"` + reset + `"},"seven_day":{"utilization":0,"resets_at":null},"seven_day_opus":{"utilization":12,"resets_at":"2000-01-01T00:00:00Z"},"seven_day_sonnet":null}}`
	u, err := decodeNativeUsage(json.RawMessage(raw), now)
	if err != nil || len(u.Windows) != 2 || u.Windows[0].UsedPercent != 24 || *u.Windows[0].ResetsAt != now.Add(time.Hour).Unix() || u.Windows[1].ResetsAt != nil || u.Windows[1].UsedPercent != 0 {
		t.Fatalf("window projection: %+v %v", u, err)
	}
	u, err = decodeNativeUsage(json.RawMessage(strings.Replace(raw, `"rate_limits_available":true`, `"rate_limits_available":false`, 1)), now)
	if err != nil || len(u.Windows) != 0 || u.APISpend != nil {
		t.Fatal("invented API-account quota", u, err)
	}
}

func TestNativeUsageRejectsInconsistentUnitsAndPublicProjection(t *testing.T) {
	raw := strings.Replace(nativeUsageFixture, `"decimal_places":2`, `"decimal_places":0`, 1)
	u, err := decodeNativeUsage(json.RawMessage(raw), time.Now())
	if err != nil || u.APISpend != nil {
		t.Fatal("inconsistent minor-unit exponent became a dollar budget", u, err)
	}
	u, err = decodeNativeUsage(json.RawMessage(nativeUsageFixture), time.Now())
	if err != nil || !validNativeUsage(u) {
		t.Fatal(u, err)
	}
	*u.APISpend.Remaining = 499
	if validNativeUsage(u) {
		t.Fatal("incorrect remaining allowance was accepted")
	}
	*u.APISpend.Remaining = math.NaN()
	if validNativeUsage(u) {
		t.Fatal("non-finite allowance was accepted")
	}
}

func TestNativeUsageUnsupportedControlHidesProviderErrorAndKeepsRuntimeUsable(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_USAGE_ERROR=1")
	r := startTest(t, cfg)
	if _, err := r.ReadNativeUsage(t.Context()); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("unsupported control succeeded or exposed native error", err)
	}
	thread := startThread(t, r)
	if thread.ID == "" {
		t.Fatal("usage failure disabled session creation")
	}
}

func TestNativeUsageProbeNeverSubmitsPromptPersistsAccountOrReplacesConversationCost(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_USAGE="+nativeUsageFixture)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	generation := r.BalanceGeneration()
	u, err := r.ReadNativeUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.SetNativeUsageAt(generation, u); err != nil {
		t.Fatal(err)
	}
	*u.APISpend.Remaining = 0 // Caller ownership cannot mutate the published snapshot.
	status, err := r.ReadClaudeStatus(t.Context(), thread.ID)
	if err != nil || status.Usage == nil || math.Abs(*status.Usage.APISpend.Remaining-376.55) > 1e-8 || status.Cost.SessionUSD != nil {
		t.Fatalf("snapshot/cost isolation: %+v %v", status, err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.StateDirectory, thread.ID+".json"))
	if strings.Contains(string(data), "123.45") || strings.Contains(string(data), "apiSpend") {
		t.Fatal("account spend persisted with a conversation")
	}
	actions, _ := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if strings.Contains(string(actions), `"launch":true`) || strings.Contains(string(actions), `"user":`) || !strings.Contains(string(actions), `"skipBehaviors":true`) {
		t.Fatalf("usage poll submitted input or scanned history: %s", actions)
	}
	if err = r.ReloadSettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = r.SetNativeUsageAt(generation, u); err == nil {
		t.Fatal("stale account observation accepted after settings change")
	}
	status, _ = r.ReadClaudeStatus(t.Context(), thread.ID)
	if status.Usage != nil {
		t.Fatal("account switch retained spend")
	}
}

func TestNativeUsageProbeFailureAndTimeoutDoNotStopActiveTurn(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_HANG_USAGE=1")
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if _, err := r.StartTurn(t.Context(), thread.ID, "block"); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_started")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := r.ReadNativeUsage(ctx); err == nil {
		t.Fatal("hung usage request succeeded")
	}
	r.server.mu.Lock()
	p := r.server.runners[thread.ID]
	active := r.server.threads[thread.ID].active() != nil
	r.server.mu.Unlock()
	if p == nil || !active {
		t.Fatal("usage timeout stopped the user's turn")
	}
	select {
	case <-p.done:
		t.Fatal("usage timeout killed conversation")
	default:
	}
}
