package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestTerminalErrorDeliveryPreservesPublicReason(t *testing.T) {
	const cyberReason = "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. If you’re doing authorized security work that requires more cyber permissive safeguards, apply for Daybreak access via https://platform.openai.com/settings/organization/status-and-access before retrying."
	for _, test := range []struct {
		kind, code, reason, fallback string
	}{
		{"turn_failed", "cyber_policy", cyberReason, "The turn failed."},
		{"turn_failed", "usage_limit_reached", "You have reached your usage limit. Try again later.", "The turn failed."},
		{"runtime_failed", "codex_unavailable", "Codex could not start. Sign in before retrying.", "The runtime stopped unexpectedly."},
		{"command_failed", "codex_command_invalid", "Choose a supported model before retrying.", "The command failed."},
	} {
		t.Run(test.kind+"/"+test.code, func(t *testing.T) {
			sender := testSender(renderFixture(), nil)
			row := eventRow(t, test.kind, protocol.Result{TurnID: "turn-a", Error: &protocol.Error{Code: test.code, Message: test.reason}}, testSessionID.String())
			chunks, err := sender.renderDeliveryMessages(t.Context(), row)
			if err != nil || len(chunks) != 1 {
				t.Fatalf("chunks=%d err=%v", len(chunks), err)
			}
			message, err := sender.deliveryMessageForSend(t.Context(), row, chunks[0])
			if err != nil {
				t.Fatal(err)
			}
			want := "❌ MacBook / Primary / auth-fix\n\n" + test.fallback + "\n\n" + test.reason + " (" + test.code + ")"
			if message.Text != want || message.ChatID != row.ChatID || message.TopicID != row.TopicID {
				t.Fatalf("public terminal error or routing changed: %+v", message)
			}
			if message.Keyboard != nil || message.DisableNotification {
				t.Fatalf("terminal error acquired reply controls or was silenced: %+v", message)
			}
		})
	}
}

func TestTerminalErrorDeliveryFallsBackWithoutPublicReason(t *testing.T) {
	for _, test := range []struct{ kind, fallback string }{
		{"turn_failed", "The turn failed."},
		{"runtime_failed", "The runtime stopped unexpectedly."},
		{"command_failed", "The command failed."},
	} {
		for _, failure := range []*protocol.Error{nil, {}, {Code: "internal_error", Message: " \n\t"}} {
			row := eventRow(t, test.kind, protocol.Result{Error: failure}, testSessionID.String())
			message, keyboard, err := testSender(renderFixture(), nil).render(t.Context(), row)
			want := "❌ MacBook / Primary / auth-fix\n\n" + test.fallback
			if err != nil || message != want || keyboard != nil {
				t.Fatalf("%s fallback: message=%q keyboard=%+v err=%v", test.kind, message, keyboard, err)
			}
		}
	}
}

func TestTerminalErrorDeliveryRedactsBeforeCheckpointingAndChunking(t *testing.T) {
	const secret = "PRIVATE_PROVIDER_TOKEN"
	redactor, err := auth.NewRedactor([]string{secret}, "[hidden]")
	if err != nil {
		t.Fatal(err)
	}
	store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: "Error session", Marker: "🟦", MultiSession: true}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	reason := strings.Repeat("Failure detail 🧪. ", 240) + secret + strings.Repeat(" More public detail 🧪.", 240)
	row := eventRow(t, "turn_failed", protocol.Result{TurnID: "turn-a", Error: &protocol.Error{Code: "provider_" + secret, Message: reason}}, testSessionID.String())
	chunks, err := sender.renderDeliveryMessages(t.Context(), row)
	if err != nil || len(chunks) < 2 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	var joined strings.Builder
	for _, raw := range chunks {
		if strings.Contains(string(raw), secret) {
			t.Fatal("terminal error secret persisted in a delivery checkpoint")
		}
		var checkpoint sessionDeliveryMessage
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			t.Fatal(err)
		}
		joined.WriteString(checkpoint.SessionBody)
		message, err := sender.deliveryMessageForSend(t.Context(), row, raw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(message.Text, "🟦 Error session\n\n") || !utf8.ValidString(message.Text) || telegramTextLength(message.Text) > 4000 {
			t.Fatalf("invalid terminal error chunk: %+v", message)
		}
		if strings.Contains(message.Text, secret) || message.Keyboard != nil {
			t.Fatalf("terminal error leaked a secret or acquired reply controls: %+v", message)
		}
	}
	want := "❌ MacBook / Primary / auth-fix\n\nThe turn failed.\n\n" + strings.ReplaceAll(reason, secret, "[hidden]") + " (provider_[hidden])"
	if joined.String() != want {
		t.Fatal("long terminal error was truncated, reformatted, or lost during chunking")
	}
}
