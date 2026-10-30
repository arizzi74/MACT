package worker

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

func enterpriseBudgetFixture() claudeSettingsFile {
	return claudeSettingsFile{AuthMode: "login", PermissionMode: "default", EnterpriseBudgetEnabled: true,
		EnterpriseBudgetUserID: "user_budgetFixture", EnterpriseBudgetAPIKey: "synthetic-read-only-budget-key", Credential: "unused-inference-key"}
}

func budgetResponse(amount, spent string) string {
	return fmt.Sprintf(`{"data":[{"scope":{"type":"user","user_id":"user_budgetFixture"},"amount":%s,"currency":"USD","period":"monthly","period_to_date_spend":%q,"source":{"type":"rbac_group","rbac_group_id":"private-group"}}],"next_page":null}`, amount, spent)
}

func TestClaudeEnterpriseBudgetReadsOnlySelectedMemberAndProjectsMinorUnits(t *testing.T) {
	settings := enterpriseBudgetFixture()
	for _, tc := range []struct {
		name, amount, spend string
		limit, remaining    float64
		unlimited           bool
	}{
		{"inherited monthly limit", `"50000"`, "31402.5", 500, 185.975, false},
		{"fractional accrued usage", `"50000"`, "41280.125", 500, 87.19875, false},
		{"disabled spending", `"0"`, "0", 0, 0, false},
		{"over limit", `"50000"`, "50100", 500, 0, false},
		{"unlimited", "null", "31402.5", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: creditsTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" || r.URL.Path != "/v1/organizations/spend_limits/effective" {
					t.Fatal("unexpected budget endpoint")
				}
				if q := r.URL.Query(); len(q) != 2 || q.Get("limit") != "1" || len(q["user_ids[]"]) != 1 || q.Get("user_ids[]") != settings.EnterpriseBudgetUserID {
					t.Fatal("request did not select only the configured member")
				}
				if r.Header.Get("x-api-key") != settings.EnterpriseBudgetAPIKey || r.Header.Get("Authorization") != "" || strings.Contains(r.URL.String(), settings.EnterpriseBudgetAPIKey) {
					t.Fatal("incorrect credential boundary")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(budgetResponse(tc.amount, tc.spend)))}, nil
			})}
			budget, err := fetchClaudeEnterpriseBudget(t.Context(), settings, client)
			if err != nil || calls != 1 || budget == nil || budget.Unlimited != tc.unlimited || budget.Currency != "USD" || budget.Period != "monthly" || budget.ObservedAt <= 0 {
				t.Fatalf("unexpected budget: %+v %v calls=%d", budget, err, calls)
			}
			if tc.unlimited {
				if budget.Limit != nil || budget.Remaining != nil {
					t.Fatal("unlimited budget was presented as a finite amount")
				}
			} else if budget.Limit == nil || budget.Remaining == nil || math.Abs(*budget.Limit-tc.limit) > 1e-9 || math.Abs(*budget.Remaining-tc.remaining) > 1e-9 {
				t.Fatalf("incorrect minor-unit conversion: %+v", budget)
			}
		})
	}
}

func TestClaudeEnterpriseBudgetRejectsAmbiguousOrUnsupportedResponses(t *testing.T) {
	valid := budgetResponse(`"50000"`, "31402.5")
	for _, body := range []string{
		`{"data":[]}`, `{"data":null}`, `{"error":"synthetic-read-only-budget-key"}`,
		strings.Replace(valid, "user_budgetFixture", "user_otherMember", 1),
		strings.Replace(valid, `"type":"user"`, `"type":"organization"`, 1),
		strings.Replace(valid, `"currency":"USD"`, `"currency":"JPY"`, 1),
		strings.Replace(valid, `"period":"monthly"`, `"period":"weekly"`, 1),
		strings.Replace(valid, `"next_page":null`, `"next_page":"another-page"`, 1),
		strings.Replace(valid, `"amount":"50000",`, "", 1),
		strings.Replace(valid, `"amount":"50000"`, `"amount":50000`, 1),
		budgetResponse(`"-1"`, "0"), budgetResponse(`"5e4"`, "0"),
		budgetResponse(`"50000"`, "-1"), budgetResponse(`"50000"`, "NaN"),
		budgetResponse(`"50000"`, "1e3"), budgetResponse(`"50000"`, ""),
		budgetResponse(`"99999999999999999999999999999999"`, "0"),
		valid + `{}`, strings.Repeat(" ", 65537),
	} {
		client := &http.Client{Transport: creditsTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if budget, err := fetchClaudeEnterpriseBudget(t.Context(), enterpriseBudgetFixture(), client); err == nil || budget != nil || strings.Contains(err.Error(), "synthetic-read-only-budget-key") {
			t.Fatal("accepted an invalid budget or exposed the Admin API key")
		}
	}
}

func TestClaudeEnterpriseBudgetNeverContactsAnotherAccountOrProvider(t *testing.T) {
	for _, scenario := range []string{"disabled", "api key auth", "token auth", "custom endpoint", "missing key", "invalid member"} {
		t.Run(scenario, func(t *testing.T) {
			settings := enterpriseBudgetFixture()
			switch scenario {
			case "disabled":
				settings.EnterpriseBudgetEnabled = false
			case "api key auth":
				settings.AuthMode = "api_key"
			case "token auth":
				settings.AuthMode = "auth_token"
			case "custom endpoint":
				settings.BaseURL = "https://another-provider.example"
			case "missing key":
				settings.EnterpriseBudgetAPIKey = ""
			case "invalid member":
				settings.EnterpriseBudgetUserID = "user_someone&user_ids[]=user_other"
			}
			client := &http.Client{Transport: creditsTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("unsupported account sent a billing request")
				return nil, nil
			})}
			if _, err := fetchClaudeEnterpriseBudget(context.Background(), settings, client); err == nil {
				t.Fatal("unsupported account accepted")
			}
		})
	}
}

func TestClaudeEnterpriseBudgetRejectsRedirectsAndAuthenticationFailures(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308, 400, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: creditsTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 || r.URL.Host != "api.anthropic.com" {
					t.Fatal("redirect forwarded an Admin API credential")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://another-provider.example"}}, Request: r,
					Body: io.NopCloser(strings.NewReader(`{"error":"synthetic-read-only-budget-key"}`))}, nil
			})}
			if _, err := fetchClaudeEnterpriseBudget(t.Context(), enterpriseBudgetFixture(), client); err == nil || calls != 1 || strings.Contains(err.Error(), "synthetic-read-only-budget-key") {
				t.Fatal("redirect or API error accepted")
			}
		})
	}
}
