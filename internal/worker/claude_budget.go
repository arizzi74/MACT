package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/iaia/telegramgw/internal/claudeadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

var errClaudeBudgetUnavailable = errors.New("Claude Enterprise budget is unavailable")

func supportsClaudeEnterpriseBudget(settings claudeSettingsFile) bool {
	return settings.EnterpriseBudgetEnabled && settings.AuthMode == "login" && settings.BaseURL == "" &&
		settings.EnterpriseBudgetAPIKey != "" && protocol.ValidClaudeEnterpriseBudgetUserID(settings.EnterpriseBudgetUserID)
}

// Enterprise spend limits are available through the documented Admin API, not
// the inference API key or a Claude session's locally estimated USD total.
// https://support.claude.com/en/articles/15330651-claude-enterprise-admin-api-reference-guide
func fetchClaudeEnterpriseBudget(ctx context.Context, settings claudeSettingsFile, client *http.Client) (*claudeadapter.EnterpriseBudget, error) {
	if !supportsClaudeEnterpriseBudget(settings) || client == nil {
		return nil, errClaudeBudgetUnavailable
	}
	query := url.Values{"user_ids[]": {settings.EnterpriseBudgetUserID}, "limit": {"1"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/organizations/spend_limits/effective?"+query.Encode(), nil)
	if err != nil {
		return nil, errClaudeBudgetUnavailable
	}
	request.Header.Set("x-api-key", settings.EnterpriseBudgetAPIKey)
	request.Header.Set("Accept", "application/json")
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errClaudeBudgetUnavailable }
	response, err := secureClient.Do(request)
	if err != nil {
		return nil, errClaudeBudgetUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errClaudeBudgetUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, errClaudeBudgetUnavailable
	}
	return decodeClaudeEnterpriseBudget(data, settings.EnterpriseBudgetUserID, time.Now().UTC().Unix())
}

// Monetary values are decimal strings in minor units; subtract exactly before
// converting the public display projection to USD. A null limit is unlimited,
// whereas a zero limit disables spending. Unknown currencies/periods fail
// closed rather than guessing their minor-unit scale or billing reset date.
func decodeClaudeEnterpriseBudget(data []byte, userID string, observedAt int64) (*claudeadapter.EnterpriseBudget, error) {
	var payload struct {
		Data []struct {
			Scope struct {
				Type   string `json:"type"`
				UserID string `json:"user_id"`
			} `json:"scope"`
			Amount   json.RawMessage `json:"amount"`
			Currency string          `json:"currency"`
			Period   string          `json:"period"`
			Spend    string          `json:"period_to_date_spend"`
		} `json:"data"`
		NextPage *string `json:"next_page"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Data) != 1 || payload.NextPage != nil {
		return nil, errClaudeBudgetUnavailable
	}
	row := payload.Data[0]
	if row.Scope.Type != "user" || row.Scope.UserID != userID || row.Currency != "USD" || row.Period != "monthly" {
		return nil, errClaudeBudgetUnavailable
	}
	spent, ok := claudeBudgetMinorUnits(row.Spend)
	if !ok {
		return nil, errClaudeBudgetUnavailable
	}
	spentUSD, _ := new(big.Rat).Quo(spent, big.NewRat(100, 1)).Float64()
	value := &claudeadapter.EnterpriseBudget{Spent: spentUSD, Currency: row.Currency, Period: row.Period, ObservedAt: observedAt}
	if string(row.Amount) == "null" {
		value.Unlimited = true
		return value, nil
	}
	var amount string
	if json.Unmarshal(row.Amount, &amount) != nil {
		return nil, errClaudeBudgetUnavailable
	}
	limit, ok := claudeBudgetMinorUnits(amount)
	if !ok {
		return nil, errClaudeBudgetUnavailable
	}
	remaining := new(big.Rat).Sub(limit, spent)
	if remaining.Sign() < 0 {
		remaining.SetInt64(0)
	}
	limitUSD, _ := new(big.Rat).Quo(limit, big.NewRat(100, 1)).Float64()
	remainingUSD, _ := remaining.Quo(remaining, big.NewRat(100, 1)).Float64()
	value.Limit, value.Remaining = &limitUSD, &remainingUSD
	return value, nil
}

func claudeBudgetMinorUnits(text string) (*big.Rat, bool) {
	if text == "" || len(text) > 32 || text[0] == '.' || text[len(text)-1] == '.' {
		return nil, false
	}
	dot := false
	for _, r := range text {
		if r == '.' && !dot {
			dot = true
			continue
		}
		if r < '0' || r > '9' {
			return nil, false
		}
	}
	value, ok := new(big.Rat).SetString(text)
	// Bound the display projection well below float64's integer precision
	// boundary; exact arithmetic above still preserves sub-cent accrued usage.
	return value, ok && value.Sign() >= 0 && value.Cmp(big.NewRat(100_000_000_000_000, 1)) <= 0
}
