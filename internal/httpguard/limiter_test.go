package httpguard

import (
	"testing"
	"time"
)

func TestLimiterDoesNotDoubleBudgetAtWindowBoundary(t *testing.T) {
	l := NewLimiter(3, 4, time.Minute)
	start := time.Unix(1_000, 0)
	if !l.Allow("a", start) || !l.Allow("a", start.Add(59*time.Second)) || !l.Allow("a", start.Add(59*time.Second)) {
		t.Fatal("initial budget rejected")
	}
	// Only the first admission expires. The two just before the old fixed
	// boundary continue to consume both the client and global budgets.
	boundary := start.Add(time.Minute)
	if !l.Allow("a", boundary) || l.Allow("a", boundary) {
		t.Fatal("per-client budget reset at the old boundary")
	}
	if !l.Allow("b", boundary) || l.Allow("c", boundary) {
		t.Fatal("global rolling budget was not enforced")
	}
	if len(l.events) != 4 || len(l.clients) != 2 {
		t.Fatal("rejected clients allocated state")
	}
	if !l.Allow("a", start.Add(119*time.Second)) {
		t.Fatal("expired admissions did not release budget")
	}
}

func TestLimiterClockRollbackAndInvalidBudgetsFailClosed(t *testing.T) {
	start := time.Unix(1_000, 0)
	l := NewLimiter(1, 1, time.Minute)
	if !l.Allow("a", start) || l.Allow("a", start.Add(-time.Hour)) || l.Allow("b", start.Add(time.Second)) {
		t.Fatal("clock rollback released budget")
	}
	for _, l := range []*Limiter{NewLimiter(0, 1, time.Minute), NewLimiter(1, 0, time.Minute), NewLimiter(1, 1, 0)} {
		if l.Allow("a", start) {
			t.Fatal("invalid budget admitted request")
		}
	}
}
