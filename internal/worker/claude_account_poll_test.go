package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/claudeadapter"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestClaudeAccountPollerHonorsCooldownAndKeepsLastBudgetUntilRecovery(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), enterpriseBudgetFixture()); err != nil {
		t.Fatal(err)
	}
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: a.cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	calls := 0
	var date time.Time
	client := &http.Client{Transport: creditsTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		status, body := 200, budgetResponse(`"50000"`, "10000")
		header := make(http.Header)
		switch calls {
		case 2:
			status = 429
			header.Set("Retry-After", "3600")
		case 3:
			status = 503
			date = now.Add(2 * time.Hour)
			header.Set("Retry-After", date.Format(http.TimeFormat))
		case 4:
			body = budgetResponse(`"50000"`, "20000")
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	poller := newClaudeAccountPoller(a.manager, bridge, client)
	poller.now = func() time.Time { return now }
	poller.jitter = func(time.Duration) time.Duration { return 0 }
	poller.readUsage = func(context.Context) (*claudeadapter.NativeUsage, error) {
		return nil, errors.New("native unavailable")
	}
	poller.poll(t.Context())
	original, _ := bridge.ReadClaudeStatus(t.Context(), thread.ID)
	for range 10 {
		poller.poll(t.Context())
	}
	if calls != 1 {
		t.Fatal("cached interval sent repeated budget requests", calls)
	}
	now = now.Add(5 * time.Minute)
	poller.poll(t.Context())
	status, _ := bridge.ReadClaudeStatus(t.Context(), thread.ID)
	refresh := status.AccountRefresh["enterpriseBudget"]
	if calls != 2 || !refresh.Failed || !refresh.RateLimited || refresh.NextAttemptAt != now.Add(time.Hour).Unix() || status.EnterpriseBudget == nil || *status.EnterpriseBudget.Remaining != 400 || status.EnterpriseBudget.ObservedAt != original.EnterpriseBudget.ObservedAt {
		t.Fatalf("429 lost the value or server cooldown: %+v %+v calls=%d", status.EnterpriseBudget, refresh, calls)
	}
	now = now.Add(time.Hour - time.Second)
	poller.poll(t.Context())
	if calls != 2 {
		t.Fatal("retried before Retry-After seconds", calls)
	}
	now = now.Add(time.Second)
	poller.poll(t.Context())
	status, _ = bridge.ReadClaudeStatus(t.Context(), thread.ID)
	if calls != 3 || status.AccountRefresh["enterpriseBudget"].NextAttemptAt != date.Unix() || *status.EnterpriseBudget.Remaining != 400 {
		t.Fatal("HTTP-date cooldown or retained value was lost", status)
	}
	now = date.Add(-time.Second)
	poller.poll(t.Context())
	if calls != 3 {
		t.Fatal("retried before HTTP-date cooldown")
	}
	now = date
	poller.poll(t.Context())
	status, _ = bridge.ReadClaudeStatus(t.Context(), thread.ID)
	if calls != 4 || status.AccountRefresh["enterpriseBudget"].Failed || *status.EnterpriseBudget.Remaining != 300 || status.AccountRefresh["enterpriseBudget"].NextAttemptAt != now.Add(5*time.Minute).Unix() {
		t.Fatal("successful retry did not restore normal polling", status)
	}
}

func TestClaudeAccountPollerNativeEmptyRepliesRetainBudgetAndSingleFlight(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: a.cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	dollars := func(v float64) *float64 { return &v }
	count := 0
	poller := newClaudeAccountPoller(a.manager, bridge, &http.Client{})
	poller.now = func() time.Time { return now }
	poller.jitter = func(time.Duration) time.Duration { return 0 }
	poller.readUsage = func(context.Context) (*claudeadapter.NativeUsage, error) {
		count++
		value := &claudeadapter.NativeUsage{Plan: "enterprise", ObservedAt: now.Unix()}
		if count != 2 {
			value.APISpend = &claudeadapter.UsageSpend{Enabled: true, Currency: "USD", Period: "monthly", Limit: dollars(500), Spent: dollars(125), Remaining: dollars(375)}
		}
		return value, nil
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() { poller.poll(t.Context()) })
	}
	workers.Wait()
	if count != 1 {
		t.Fatal("concurrent refreshes bypassed the shared schedule", count)
	}
	original, _ := bridge.ReadClaudeStatus(t.Context(), thread.ID)
	now = now.Add(5 * time.Minute)
	poller.poll(t.Context())
	status, _ := bridge.ReadClaudeStatus(t.Context(), thread.ID)
	if count != 2 || status.Usage == nil || status.Usage.APISpend == nil || *status.Usage.APISpend.Remaining != 375 || status.Usage.ObservedAt != original.Usage.ObservedAt || !status.AccountRefresh["usage"].Failed {
		t.Fatal("empty SDK reply wiped or falsely refreshed the old budget", status)
	}
	now = now.Add(5 * time.Minute)
	poller.poll(t.Context())
	status, _ = bridge.ReadClaudeStatus(t.Context(), thread.ID)
	if count != 3 || status.AccountRefresh["usage"].Failed || status.Usage.ObservedAt != now.Unix() {
		t.Fatal("native usage did not recover", status)
	}
	if err := bridge.ReloadSettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ = bridge.ReadClaudeStatus(t.Context(), thread.ID)
	if status.Usage != nil || len(status.AccountRefresh) != 0 {
		t.Fatal("account generation change retained an old observation")
	}
	poller.poll(t.Context())
	if count != 4 {
		t.Fatal("new settings inherited the old cooldown")
	}
}

func TestClaudeAccountBackoffIsBoundedAndNeverShortensServerGuidance(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s := &claudeAccountSchedule{}
	for index := range 20 {
		refresh := s.complete(now, 5*time.Minute, errors.New("unavailable"), func(time.Duration) time.Duration { return 0 })
		want := min(5*time.Minute*time.Duration(1<<min(index, 4)), time.Hour)
		if refresh.NextAttemptAt != now.Add(want).Unix() || !refresh.Failed {
			t.Fatal("incorrect failure backoff", index, refresh)
		}
	}
	err := claudeAccountHTTPError(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"18446744073709551615"}}}, errClaudeBudgetUnavailable)
	refresh := s.complete(now, 5*time.Minute, err, func(time.Duration) time.Duration { return 0 })
	if refresh.NextAttemptAt != 253402300799 || !refresh.RateLimited {
		t.Fatal("overflow shortened a server cooldown", refresh)
	}
	refresh = s.complete(now, 5*time.Minute, nil, func(time.Duration) time.Duration { return 0 })
	if refresh.Failed || refresh.NextAttemptAt != now.Add(5*time.Minute).Unix() {
		t.Fatal("success did not reset backoff", refresh)
	}
}

func TestClaudeCreditsPollFailureRetainsOriginalObservation(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	settings := claudeSettingsFile{BaseURL: "https://api.abliteration.ai", AuthMode: "auth_token", PermissionMode: "default", Credential: "synthetic-credit-poll-key"}
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), settings); err != nil {
		t.Fatal(err)
	}
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: a.cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: creditsTransport(func(*http.Request) (*http.Response, error) {
		calls++
		code := 200
		if calls > 1 {
			code = 503
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Retry-After": {"3600"}}, Body: io.NopCloser(strings.NewReader(`{"data":{"total_credits":45,"total_usage":20}}`))}, nil
	})}
	poller := newClaudeAccountPoller(a.manager, bridge, client)
	now := time.Now()
	poller.now = func() time.Time { return now }
	poller.jitter = func(time.Duration) time.Duration { return 0 }
	poller.poll(t.Context())
	original, _ := bridge.ReadClaudeStatus(t.Context(), thread.ID)
	now = now.Add(time.Minute)
	poller.poll(t.Context())
	cached, _ := bridge.ReadClaudeStatus(t.Context(), thread.ID)
	if original.Balance == nil || cached.Balance == nil || cached.Balance.Remaining != 25 || cached.Balance.ObservedAt != original.Balance.ObservedAt || !cached.AccountRefresh["balance"].Failed || cached.AccountRefresh["balance"].NextAttemptAt != now.Add(time.Hour).Unix() {
		t.Fatal("credit refresh lost its value, age or cooldown", cached)
	}
}
