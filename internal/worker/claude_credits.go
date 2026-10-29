package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/iaia/telegramgw/internal/claudeadapter"
)

var errClaudeCreditsUnavailable = errors.New("provider credits are unavailable")

func supportsClaudeCredits(settings claudeSettingsFile) bool {
	u, err := url.Parse(settings.BaseURL)
	return err == nil && u.Scheme == "https" && u.Host == "api.abliteration.ai" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (settings.AuthMode == "api_key" || settings.AuthMode == "auth_token") && settings.Credential != ""
}

// Inference credentials stay on the worker. Only the numeric USD projection is
// broadcast; no provider error, billing identity, or raw response is relayed.
func fetchClaudeCredits(ctx context.Context, settings claudeSettingsFile, client *http.Client) (*claudeadapter.CreditBalance, error) {
	if !supportsClaudeCredits(settings) {
		return nil, errClaudeCreditsUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.abliteration.ai/v1/credits", nil)
	if err != nil {
		return nil, errClaudeCreditsUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+settings.Credential)
	request.Header.Set("Accept", "application/json")
	// Enforce this boundary even when a caller supplies a reusable client.
	// A provider redirect must never forward account credentials elsewhere.
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errClaudeCreditsUnavailable }
	response, err := secureClient.Do(request)
	if err != nil {
		return nil, errClaudeCreditsUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errClaudeCreditsUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, errClaudeCreditsUnavailable
	}
	var payload struct {
		Data *struct {
			Credits *float64 `json:"total_credits"`
			Usage   *float64 `json:"total_usage"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Data == nil || payload.Data.Credits == nil || payload.Data.Usage == nil {
		return nil, errClaudeCreditsUnavailable
	}
	credits, usage := *payload.Data.Credits, *payload.Data.Usage
	if credits < 0 || usage < 0 || math.IsNaN(credits) || math.IsNaN(usage) || math.IsInf(credits, 0) || math.IsInf(usage, 0) {
		return nil, errClaudeCreditsUnavailable
	}
	// The account owner confirmed this against the provider billing page:
	// total_credits is the credit allocation, total_usage is finalized spend.
	return &claudeadapter.CreditBalance{Remaining: math.Max(0, credits-usage), Unit: "USD", ObservedAt: time.Now().UTC().Unix()}, nil
}

// One poller per dedicated runtime, regardless of browser/tab count. A settings
// generation fences an in-flight response after an account change. Failures
// clear the projection rather than showing an old balance as current.
func (m *RuntimeManager) claudeCreditsLoop(bridge *claudeadapter.Runtime) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-bridge.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errClaudeCreditsUnavailable }}
	defer client.CloseIdleConnections()
	poll := func() {
		generation := bridge.BalanceGeneration()
		settings, err := readClaudeSettings(claudeSettingsPath(m.cfg))
		if err != nil || !supportsClaudeCredits(settings) {
			_ = bridge.SetBalanceAt(generation, nil)
			return
		}
		balance, err := fetchClaudeCredits(ctx, settings, client)
		latest, settingsErr := readClaudeSettings(claudeSettingsPath(m.cfg))
		if err != nil || settingsErr != nil || latest != settings {
			balance = nil
		}
		_ = bridge.SetBalanceAt(generation, balance)
	}
	poll()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
