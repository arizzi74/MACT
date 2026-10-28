package worker

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

type creditsTransport func(*http.Request) (*http.Response, error)

func (f creditsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeCreditsProjectsOnlyValidatedProviderUSD(t *testing.T) {
	settings := claudeSettingsFile{BaseURL: "https://api.abliteration.ai", AuthMode: "auth_token", Credential: "private-credit-test-token"}
	for _, test := range []struct {
		name, body string
		status     int
		valid      bool
		remaining  float64
	}{
		{"reported credits", `{"data":{"total_credits":45,"total_usage":23.701081},"organization":"private"}`, 200, true, 21.298919},
		{"exhausted", `{"data":{"total_credits":5,"total_usage":7}}`, 200, true, 0},
		{"zero", `{"data":{"total_credits":0,"total_usage":0}}`, 200, true, 0},
		{"missing credit", `{"data":{"total_usage":5}}`, 200, false, 0},
		{"missing usage", `{"data":{"total_credits":5}}`, 200, false, 0},
		{"negative", `{"data":{"total_credits":-1,"total_usage":0}}`, 200, false, 0},
		{"nonfinite", `{"data":{"total_credits":1e999,"total_usage":0}}`, 200, false, 0},
		{"string", `{"data":{"total_credits":"45","total_usage":0}}`, 200, false, 0},
		{"unauthorized", `{"error":"private-credit-test-token"}`, 401, false, 0},
		{"too large", strings.Repeat(" ", 65537), 200, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: creditsTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.String() != "https://api.abliteration.ai/v1/credits" || r.Header.Get("Authorization") != "Bearer "+settings.Credential {
					t.Fatal("unexpected provider request")
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
			})}
			balance, err := fetchClaudeCredits(context.Background(), settings, client)
			if (err == nil) != test.valid {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), settings.Credential) {
				t.Fatal("credential exposed in provider error")
			}
			if test.valid && (math.Abs(balance.Remaining-test.remaining) > 1e-9 || balance.Unit != "USD" || balance.ObservedAt <= 0) {
				t.Fatalf("balance=%+v", balance)
			}
		})
	}
}

func TestClaudeCreditsNeverSendsSavedCredentialForLoginOrAnotherProvider(t *testing.T) {
	for _, test := range []struct{ endpoint, mode string }{
		{"https://api.abliteration.ai", "login"},
		{"https://example.com", "auth_token"},
		{"http://api.abliteration.ai", "auth_token"},
		{"https://api.abliteration.ai.evil.test", "api_key"},
		{"https://api.abliteration.ai:8443", "api_key"},
	} {
		settings := claudeSettingsFile{BaseURL: test.endpoint, AuthMode: test.mode, Credential: "inactive-secret"}
		client := &http.Client{Transport: creditsTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsupported provider received a credential")
			return nil, nil
		})}
		if _, err := fetchClaudeCredits(context.Background(), settings, client); err == nil {
			t.Fatal("unsupported account accepted")
		}
	}
}
