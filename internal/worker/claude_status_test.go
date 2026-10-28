package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestClaudeStatusKeepsUnknownDifferentFromZero(t *testing.T) {
	for _, test := range []struct{ wire, want, absent string }{
		{`{"cost":{"sessionUSD":null}}`, "Reported session cost: unavailable", "$0.000000"},
		{`{"cost":{"sessionUSD":0},"balance":{"remaining":0,"unit":"USD","observedAt":1770000000}}`, "Account credit remaining: $0.0000", "Weekly"},
		{`{"context":{"usedTokens":25000,"maxTokens":100000},"cost":{"sessionUSD":0.125}}`, "Context: 25,000 / 100,000 tokens (25% used)", "Rate limits"},
		{`{"cost":{"sessionUSD":-1}}`, "Reported session cost: unavailable", "$-1"},
	} {
		var status codexadapter.ClaudeSessionStatus
		if err := json.Unmarshal([]byte(test.wire), &status); err != nil {
			t.Fatal(err)
		}
		text := strings.Join(claudeStatusLines(status), "\n")
		if !strings.Contains(text, test.want) || strings.Contains(text, test.absent) {
			t.Fatalf("status unexpectedly reports unknown/zero: %s", text)
		}
	}
}
