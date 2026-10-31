package worker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestClaudeEnterpriseBudgetPollFailureRetainsPreviouslyPublishedStatus(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	settings := enterpriseBudgetFixture()
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), settings); err != nil {
		t.Fatal(err)
	}
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: a.cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: creditsTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "api.anthropic.com" || request.URL.Query().Get("user_ids[]") != settings.EnterpriseBudgetUserID {
			t.Error("poller requested the wrong Enterprise member")
		}
		status, body := http.StatusOK, budgetResponse(`"50000"`, "31402.5")
		if calls == 2 {
			status, body = http.StatusServiceUnavailable, `{"error":"provider unavailable"}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	a.manager.pollClaudeAccountStatus(t.Context(), bridge, client)
	published, err := bridge.Client.ReadClaudeStatus(t.Context(), thread.ID)
	if err != nil || published.EnterpriseBudget == nil || published.EnterpriseBudget.Remaining == nil || *published.EnterpriseBudget.Remaining != 185.975 {
		t.Fatalf("successful poll was not visible in native session status: %+v %v", published.EnterpriseBudget, err)
	}
	a.manager.pollClaudeAccountStatus(t.Context(), bridge, client)
	failed, err := bridge.Client.ReadClaudeStatus(t.Context(), thread.ID)
	if err != nil || calls != 2 || failed.EnterpriseBudget == nil || *failed.EnterpriseBudget.Remaining != 185.975 || failed.EnterpriseBudget.ObservedAt != published.EnterpriseBudget.ObservedAt || !failed.AccountRefresh["enterpriseBudget"].Failed {
		t.Fatalf("HTTP failure discarded or refreshed the last successful budget: %+v %v calls=%d", failed.EnterpriseBudget, err, calls)
	}
}

func TestClaudeEnterpriseBudgetPollDiscardsInFlightResponseAfterAccountSwitch(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	oldSettings := enterpriseBudgetFixture()
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), oldSettings); err != nil {
		t.Fatal(err)
	}
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: a.cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	nextSettings := oldSettings
	nextSettings.EnterpriseBudgetUserID = "user_nextBudgetMember"
	nextSettings.EnterpriseBudgetAPIKey = "synthetic-next-member-budget-key"
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &http.Client{Transport: creditsTransport(func(request *http.Request) (*http.Response, error) {
		member := request.URL.Query().Get("user_ids[]")
		body := budgetResponse(`"50000"`, "31402.5")
		switch member {
		case oldSettings.EnterpriseBudgetUserID:
			if request.Header.Get("x-api-key") != oldSettings.EnterpriseBudgetAPIKey {
				t.Error("old member request used the wrong Admin API credential")
			}
			close(started)
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		case nextSettings.EnterpriseBudgetUserID:
			if request.Header.Get("x-api-key") != nextSettings.EnterpriseBudgetAPIKey {
				t.Error("new member request used the old Admin API credential")
			}
			body = strings.Replace(budgetResponse(`"50000"`, "10000"), oldSettings.EnterpriseBudgetUserID, nextSettings.EnterpriseBudgetUserID, 1)
		default:
			t.Error("poller requested an unexpected Enterprise member")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	generation := bridge.BalanceGeneration()
	go func() { defer close(done); a.manager.pollClaudeAccountStatus(ctx, bridge, client) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("old member poll did not start")
	}
	if err := bridge.WithIdleSettingsUpdate(func() error { return saveClaudeSettings(claudeSettingsPath(a.cfg), nextSettings) }); err != nil {
		t.Fatal(err)
	}
	if bridge.BalanceGeneration() == generation {
		t.Fatal("account switch did not advance the observation generation")
	}
	switched, err := bridge.Client.ReadClaudeStatus(t.Context(), thread.ID)
	if err != nil || switched.EnterpriseBudget != nil {
		t.Fatalf("account switch retained the old member observation: %+v %v", switched.EnterpriseBudget, err)
	}
	// Publish the new member before releasing the delayed old response. Its
	// completion must neither expose the old amount nor clear this newer value.
	a.manager.pollClaudeAccountStatus(t.Context(), bridge, client)
	current, err := bridge.Client.ReadClaudeStatus(t.Context(), thread.ID)
	if err != nil || current.EnterpriseBudget == nil || current.EnterpriseBudget.Remaining == nil || *current.EnterpriseBudget.Remaining != 400 {
		t.Fatalf("new member budget was not published: %+v %v", current.EnterpriseBudget, err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old member poll did not finish")
	}
	final, err := bridge.Client.ReadClaudeStatus(t.Context(), thread.ID)
	if err != nil || final.EnterpriseBudget == nil || final.EnterpriseBudget.Remaining == nil || *final.EnterpriseBudget.Remaining != 400 {
		t.Fatalf("delayed old member response replaced the new account status: %+v %v", final.EnterpriseBudget, err)
	}
}
