package worker

import (
	"strings"

	"github.com/iaia/telegramgw/internal/auth"
)

const maxPublicTurnErrorRunes = 8000

// Public turn failures can include the full explanation and recovery URL shown
// by Codex. Preserve that text separately from shorter command/RPC summaries,
// with the same secret redaction boundary as other user-visible turn output.
func publicTurnErrorMessage(redactor *auth.Redactor, message string) string {
	message = redactor.Redact(strings.TrimSpace(message))
	message = strings.ReplaceAll(strings.ToValidUTF8(message, "\uFFFD"), "\x00", "\uFFFD")
	if runes := []rune(message); len(runes) > maxPublicTurnErrorRunes {
		message = string(runes[:maxPublicTurnErrorRunes-1]) + "…"
	}
	return message
}
