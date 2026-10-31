package worker

import (
	"fmt"
	"math"
	"time"

	"github.com/iaia/telegramgw/internal/claudeadapter"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

func claudeStatusLines(status codexadapter.ClaudeSessionStatus) []string {
	model, permissions := "unavailable", "unavailable"
	if status.Model != nil && *status.Model != "" {
		model = *status.Model
		if status.ModelDisplayName != nil && *status.ModelDisplayName != "" && *status.ModelDisplayName != model {
			model = *status.ModelDisplayName + " (" + model + ")"
		}
	}
	if status.PermissionMode != nil && *status.PermissionMode != "" {
		permissions = claudeadapter.PermissionLabel(*status.PermissionMode)
	}
	lines := []string{"Model: " + model, "Permission mode: " + permissions}
	if status.ReasoningEffort != nil {
		lines = append(lines, "Effort: "+claudeadapter.EffortLabel(*status.ReasoningEffort))
	}
	context := "Context: unavailable (not reported by Claude Code)"
	if used := status.Context.UsedTokens; used != nil && *used >= 0 {
		context = fmt.Sprintf("Context: %s tokens", formatInt(*used))
		if maximum := status.Context.MaxTokens; maximum != nil && *maximum > 0 {
			context = fmt.Sprintf("Context: %s / %s tokens (%.0f%% used)", formatInt(*used), formatInt(*maximum), float64(*used)/float64(*maximum)*100)
		}
	}
	lines = append(lines, context)
	cost := func(value *float64) string {
		if value == nil || *value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return "unavailable (not reported by Claude Code)"
		}
		return fmt.Sprintf("$%.6f", *value)
	}
	if status.Cost.Incomplete {
		line := "Session cost: incomplete (native accounting reset)"
		if known := status.Cost.KnownSessionUSD; known != nil && validClaudeKnownCost(*known) {
			line = fmt.Sprintf("Session cost: at least $%.6f (incomplete native accounting)", *known)
		}
		lines = append(lines, line, "Last turn cost: unavailable (incomplete native accounting)")
	} else {
		lines = append(lines, "Session cost: "+cost(status.Cost.SessionUSD), "Last turn cost: "+cost(status.Cost.LastTurnUSD))
	}
	lines = append(lines, claudeNativeUsageLines(status.Usage)...)
	if balance := status.Balance; balance != nil && (balance.Unit == "USD" || balance.Unit == "credits") && balance.Remaining >= 0 && !math.IsNaN(balance.Remaining) && !math.IsInf(balance.Remaining, 0) {
		value := fmt.Sprintf("%.4f credits", balance.Remaining)
		if balance.Unit == "USD" {
			value = fmt.Sprintf("$%.4f", balance.Remaining)
		}
		lines = append(lines, "Account credit remaining: "+value)
		if balance.ObservedAt > 0 {
			lines = append(lines, "Balance checked: "+time.Unix(balance.ObservedAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
	}
	if budget := status.EnterpriseBudget; budget != nil && (status.Usage == nil || status.Usage.APISpend == nil) && budget.Currency == "USD" && budget.Period == "monthly" && budget.Spent >= 0 && !math.IsNaN(budget.Spent) && !math.IsInf(budget.Spent, 0) {
		line := fmt.Sprintf("Enterprise monthly budget: $%.2f spent", budget.Spent)
		if budget.Unlimited {
			line += " (unlimited)"
		} else if budget.Limit != nil && budget.Remaining != nil && *budget.Limit >= 0 && *budget.Remaining >= 0 && !math.IsNaN(*budget.Limit) && !math.IsInf(*budget.Limit, 0) && !math.IsNaN(*budget.Remaining) && !math.IsInf(*budget.Remaining, 0) {
			line = fmt.Sprintf("Enterprise monthly budget: $%.2f remaining / $%.2f ($%.2f spent)", *budget.Remaining, *budget.Limit, budget.Spent)
		}
		lines = append(lines, line)
		if budget.ObservedAt > 0 {
			lines = append(lines, "Enterprise budget checked: "+time.Unix(budget.ObservedAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
	}
	for _, source := range []string{"usage", "enterpriseBudget", "balance"} {
		refresh, ok := status.AccountRefresh[source]
		if !ok {
			continue
		}
		label := map[string]string{"usage": "Claude usage", "enterpriseBudget": "Enterprise budget", "balance": "Account credit"}[source]
		if refresh.Failed {
			lines = append(lines, label+" refresh unavailable; showing the last successful report when available.")
		}
		if refresh.NextAttemptAt > 0 {
			lines = append(lines, label+" next refresh: "+time.Unix(refresh.NextAttemptAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
		}
	}
	return lines
}

func validClaudeKnownCost(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func claudeAuthenticationLabel(value string) string {
	switch value {
	case "login":
		return "Claude Code account"
	case "api_key":
		return "API key"
	case "auth_token":
		return "Auth token"
	default:
		return "unavailable"
	}
}

func claudeNativeUsageLines(usage *codexadapter.ClaudeNativeUsage) []string {
	if usage == nil {
		return nil
	}
	var lines []string
	if plan := map[string]string{"pro": "Pro", "max": "Max", "team": "Team", "enterprise": "Enterprise"}[usage.Plan]; plan != "" {
		lines = append(lines, "Plan: Claude "+plan)
	}
	for _, window := range usage.Windows {
		if window.UsedPercent < 0 || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %.0f%% used", window.Name, window.UsedPercent))
		if window.ResetsAt != nil && *window.ResetsAt > 0 && *window.ResetsAt < 253402300799 {
			lines = append(lines, window.Name+" resets: "+usageTimestamp(*window.ResetsAt))
		}
	}
	if spend := usage.APISpend; spend != nil && spend.Currency == "USD" && spend.Period == "monthly" {
		valid := func(v *float64) bool { return v != nil && *v >= 0 && !math.IsNaN(*v) && !math.IsInf(*v, 0) }
		switch {
		case valid(spend.Limit) && valid(spend.Spent) && valid(spend.Remaining):
			lines = append(lines, fmt.Sprintf("API spend: $%.2f remaining / $%.2f monthly limit ($%.2f spent)", *spend.Remaining, *spend.Limit, *spend.Spent))
		case valid(spend.Spent):
			lines = append(lines, fmt.Sprintf("API spend: $%.2f this month (limit not reported by Claude Code)", *spend.Spent))
		default:
			lines = append(lines, "API spend: unavailable (not reported by Claude Code)")
		}
		if !spend.Enabled {
			lines = append(lines, "Extra usage: disabled")
		}
	}
	if len(lines) > 0 && usage.ObservedAt > 0 && usage.ObservedAt < 253402300799 {
		lines = append(lines, "Usage checked: "+time.Unix(usage.ObservedAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
	}
	return lines
}
