package codexadapter

import (
	"context"
	"errors"
)

// ClaudeSessionStatus contains only public runtime observations. Account
// credentials and provider response bodies never form part of this projection.
type ClaudeSessionStatus struct {
	Provider        string  `json:"provider"`
	Model           *string `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	PermissionMode  *string `json:"permissionMode"`
	Context         struct {
		UsedTokens *int64 `json:"usedTokens"`
		MaxTokens  *int64 `json:"maxTokens"`
	} `json:"context"`
	Cost struct {
		SessionUSD  *float64 `json:"sessionUSD"`
		LastTurnUSD *float64 `json:"lastTurnUSD"`
	} `json:"cost"`
	EnterpriseBudget *ClaudeEnterpriseBudget `json:"enterpriseBudget,omitempty"`
	Balance          *struct {
		Remaining  float64 `json:"remaining"`
		Unit       string  `json:"unit"`
		ObservedAt int64   `json:"observedAt"`
	} `json:"balance,omitempty"`
}

func (c *Client) ReadClaudeStatus(ctx context.Context, threadID string) (ClaudeSessionStatus, error) {
	var result struct {
		ThreadID string               `json:"threadId"`
		Status   *ClaudeSessionStatus `json:"claudeStatus"`
	}
	if err := c.request(ctx, "thread/status/read", map[string]any{"threadId": threadID}, &result, false); err != nil {
		return ClaudeSessionStatus{}, err
	}
	if result.ThreadID != threadID || result.Status == nil || result.Status.Provider != "claude" {
		return ClaudeSessionStatus{}, errors.New("Claude session status is unavailable")
	}
	return *result.Status, nil
}

// ClaudeEnterpriseBudget is the account's Enterprise monthly allocation,
// separate from per-conversation API cost or prepaid credit.
type ClaudeEnterpriseBudget struct {
	Limit      *float64 `json:"limit"`
	Spent      float64  `json:"spent"`
	Remaining  *float64 `json:"remaining"`
	Currency   string   `json:"currency"`
	Period     string   `json:"period"`
	Unlimited  bool     `json:"unlimited"`
	ObservedAt int64    `json:"observedAt"`
}
