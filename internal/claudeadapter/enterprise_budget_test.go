package claudeadapter

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnterpriseBudgetIsEphemeralClonedAndFencedByAccountSettings(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	generation := r.BalanceGeneration()
	limit, remaining := 500.0, 185.975
	budget := &EnterpriseBudget{Limit: &limit, Spent: 314.025, Remaining: &remaining, Currency: "USD", Period: "monthly", ObservedAt: 1800000000}
	if err := r.SetEnterpriseBudgetAt(generation, budget); err != nil {
		t.Fatal(err)
	}
	limit, remaining = 999, 999
	status := readStatus(t, r, thread.ID)
	if status.EnterpriseBudget == nil || status.EnterpriseBudget.Limit == nil || *status.EnterpriseBudget.Limit != 500 || *status.EnterpriseBudget.Remaining != 185.975 || status.EnterpriseBudget.Spent != 314.025 {
		t.Fatalf("budget was not defensively copied: %+v", status.EnterpriseBudget)
	}
	// A public snapshot also cannot mutate the runtime's subsequent projection.
	*status.EnterpriseBudget.Limit = 1
	if got := readStatus(t, r, thread.ID).EnterpriseBudget; *got.Limit != 500 {
		t.Fatal("snapshot mutated runtime budget")
	}
	r.server.mu.Lock()
	err := r.server.save(r.server.threads[thread.ID])
	r.server.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, thread.ID+".json"))
	if err != nil || strings.Contains(string(data), "185.975") || strings.Contains(string(data), "314.025") {
		t.Fatal("account budget was persisted with the conversation")
	}
	if err := r.WithIdleSettingsUpdate(nil); err != nil {
		t.Fatal(err)
	}
	if got := readStatus(t, r, thread.ID).EnterpriseBudget; got != nil {
		t.Fatal("account settings switch retained the previous budget")
	}
	if err := r.SetEnterpriseBudgetAt(generation, &EnterpriseBudget{Spent: 0, Currency: "USD", Period: "monthly", Unlimited: true, ObservedAt: 1800000000}); err == nil {
		t.Fatal("an old account observation repopulated the budget")
	}
	if err := r.SetEnterpriseBudgetAt(r.BalanceGeneration(), &EnterpriseBudget{Spent: 0, Currency: "USD", Period: "monthly", Unlimited: true, ObservedAt: 1800000000}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	if got := readStatus(t, r2, thread.ID).EnterpriseBudget; got != nil {
		t.Fatal("a previous account budget survived runtime restart")
	}
}

func TestEnterpriseBudgetRejectsInvalidValues(t *testing.T) {
	zero, fifty, hundred, nan := 0.0, 50.0, 100.0, math.NaN()
	for _, value := range []*EnterpriseBudget{
		{Limit: &hundred, Remaining: &fifty, Spent: 75, Currency: "USD", Period: "monthly", ObservedAt: 1800000000},
		{Limit: &hundred, Remaining: &zero, Spent: -1, Currency: "USD", Period: "monthly", ObservedAt: 1800000000},
		{Limit: &nan, Remaining: &zero, Spent: 0, Currency: "USD", Period: "monthly", ObservedAt: 1800000000},
		{Unlimited: true, Limit: &hundred, Currency: "USD", Period: "monthly", ObservedAt: 1800000000},
		{Unlimited: true, Currency: "JPY", Period: "monthly", ObservedAt: 1800000000},
		{Unlimited: true, Currency: "USD", Period: "weekly", ObservedAt: 1800000000},
		{Unlimited: true, Currency: "USD", Period: "monthly"},
	} {
		if validEnterpriseBudget(value) {
			t.Fatal("accepted an invalid public budget")
		}
	}
}
