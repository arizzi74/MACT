package claudeadapter

import "strings"

func EffortLabel(value string) string {
	switch value {
	case "", "auto", "default":
		return "Auto"
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "Extra high"
	case "max":
		return "Max"
	default:
		return strings.TrimSpace(value)
	}
}

func PermissionLabel(value string) string {
	switch value {
	case "default":
		return "Default"
	case "manual":
		return "Manual"
	case "acceptEdits":
		return "Accept edits"
	case "plan":
		return "Plan mode"
	case "dontAsk":
		return "Don't ask"
	case "auto":
		return "Auto"
	case "bypassPermissions":
		return "Bypass permissions"
	default:
		return value
	}
}
