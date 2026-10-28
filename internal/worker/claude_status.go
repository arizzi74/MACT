package worker

import (
	"fmt"
	"math"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func claudeStatusLines(status codexadapter.ClaudeSessionStatus) []string {
	model, permissions := "unavailable", "unavailable"
	if status.Model != nil && *status.Model != "" {
		model = *status.Model
	}
	if status.PermissionMode != nil && *status.PermissionMode != "" {
		permissions = *status.PermissionMode
	}
	lines := []string{"Model: " + model, "Permissions: " + permissions}
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
	lines = append(lines, "Reported session cost: "+cost(status.Cost.SessionUSD), "Reported last turn cost: "+cost(status.Cost.LastTurnUSD))
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
	return lines
}
