package worker

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

// Assemble deliberately repetitive, synthetic format fixtures at runtime.
// These values never authenticate or leave the local test transport.
func syntheticCredentialSamples() map[string]string {
	return map[string]string{
		"worker":               "cwk_" + strings.Repeat("a", 64),
		"API key":              "sk-" + strings.Repeat("a", 32),
		"Telegram":             "123456789:" + strings.Repeat("a", 35),
		"GitHub classic":       "ghp_" + strings.Repeat("a", 36),
		"GitHub fine grained":  "github_pat_" + strings.Repeat("A", 22) + "_" + strings.Repeat("b", 59),
		"AWS":                  "AKIA" + strings.Repeat("A", 16),
		"AWS temporary":        "ASIA" + strings.Repeat("B", 16),
		"Google":               "AIza" + strings.Repeat("a", 35),
		"Google hyphen suffix": "AIza" + strings.Repeat("a", 34) + "-",
		"Slack bot":            "xoxb-" + strings.Repeat("1", 10) + "-" + strings.Repeat("2", 12) + "-" + strings.Repeat("a", 24),
		"Slack user":           "xoxp-" + strings.Repeat("1", 10) + "-" + strings.Repeat("2", 12) + "-" + strings.Repeat("3", 12) + "-" + strings.Repeat("b", 32),
		"JWT":                  "eyJ" + strings.Repeat("h", 17) + "." + strings.Repeat("p", 24) + "." + strings.Repeat("s", 32),
		"JWT hyphen suffix":    "eyJ" + strings.Repeat("h", 17) + "." + strings.Repeat("p", 24) + "." + strings.Repeat("s", 31) + "-",
	}
}

func TestDefaultCredentialRedactionFormatsBoundariesAndCustomRules(t *testing.T) {
	r, err := newWorkerRedactor([]string{`CUSTOM_[A-Z]+`})
	if err != nil {
		t.Fatal(err)
	}
	for family, token := range syntheticCredentialSamples() {
		t.Run(family, func(t *testing.T) {
			if r.Redact(token) != "[REDACTED]" {
				t.Fatal("synthetic token was not completely redacted")
			}
			for _, text := range []string{token, "Bearer " + token, "first\nkey=\"" + token + "\"\nlast", token + " " + token} {
				if got := r.Redact(text); strings.Contains(got, token) || !strings.Contains(got, "[REDACTED]") {
					t.Fatal("matching synthetic token escaped redaction")
				}
			}
			obscured := token[:7] + "\u200b\u200d\u2060" + token[7:]
			if got := r.Redact(obscured); got != "[REDACTED]" {
				t.Fatal("invisible separator evaded matching")
			}
		})
	}
	for _, plain := range []string{
		"ghp_short", "github_pat_example", "xoxb-example-in-documentation", "AKIA" + strings.Repeat("A", 15),
		"akia" + strings.Repeat("A", 16), "AIza" + strings.Repeat("a", 34),
		"prefixghp_" + strings.Repeat("a", 36), "ghp_" + strings.Repeat("a", 37),
		"eyJheader.payload.signature", "emoji 👩\u200d💻 and zero\u200bwidth prose", string([]byte{'x', 0xff, 0xe2, 0x80, 0x8b}),
	} {
		if got := r.Redact(plain); got != plain {
			t.Fatal("ordinary text or partial identifier was redacted")
		}
	}
	if r.Redact("CUSTOM_VALUE") != "[REDACTED]" {
		t.Fatal("custom rules replaced defaults instead of extending them")
	}
}

func TestDefaultCredentialsAreSanitizedBeforeOutboxPersistenceAndReplay(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "new event", true: "legacy replay"}[upgrade], func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			r, _ := newWorkerRedactor(nil)
			if !upgrade {
				if err := store.configureRedactor(r); err != nil {
					t.Fatal(err)
				}
			}
			var tokens []string
			for _, token := range syntheticCredentialSamples() {
				tokens = append(tokens, token)
			}
			text := strings.Join(tokens, "\n")
			data, _ := json.Marshal(protocol.Result{CommandID: uuid.NewString(), Text: text, Error: &protocol.Error{Code: "stable", Message: text}})
			before, err := store.AppendEvent(protocol.Event{Kind: "command_completed", SessionID: uuid.NewString(), Data: data})
			if err != nil {
				t.Fatal(err)
			}
			if !upgrade {
				for _, token := range tokens {
					if strings.Contains(string(before.Data), token) {
						t.Fatal("credential was not redacted before the initial durable write")
					}
				}
			}
			if err := store.configureRedactor(r); err != nil {
				t.Fatal(err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil || len(events) != 1 {
				t.Fatal("outbox replay failed", err)
			}
			after := events[0]
			if before.ID != after.ID || before.Seq != after.Seq || before.Kind != after.Kind || before.SessionID != after.SessionID || !before.OccurredAt.Equal(after.OccurredAt) {
				t.Fatal("redaction changed replay identity")
			}
			if err := store.db.View(func(tx *workerdb.Tx) error {
				return tx.Bucket(bucketOutbox).ForEach(func(_, value []byte) error {
					for _, token := range tokens {
						if strings.Contains(string(value), token) {
							t.Error("synthetic credential persisted in outbound queue")
						}
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
			// This is the remote native Web UI projection, including fields
			// unknown to the normalized Telegram event vocabulary.
			raw, _ := json.Marshal(map[string]any{"id": "stable-id", "item": map[string]any{"id": "stable-item", "newNativeField": text, "text": text}})
			projected, err := redactWebUIJSON(r, raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, token := range tokens {
				if strings.Contains(string(projected), token) {
					t.Error("synthetic credential reached Web UI projection")
				}
			}
			if !strings.Contains(string(projected), "stable-id") || !strings.Contains(string(projected), "stable-item") {
				t.Fatal("Web UI identity changed")
			}
		})
	}
}
