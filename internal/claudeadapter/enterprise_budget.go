package claudeadapter

import (
	"errors"
	"math"
)

// EnterpriseBudget is a read-only observation of the configured Enterprise
// member's effective monthly spend limit and accrued usage. It is independent
// of a session's estimated cost and of any organization-wide shared pool.
// Nil Limit and Remaining mean unlimited only when Unlimited is true.
type EnterpriseBudget struct {
	Limit      *float64 `json:"limit"`
	Spent      float64  `json:"spent"`
	Remaining  *float64 `json:"remaining"`
	Currency   string   `json:"currency"`
	Period     string   `json:"period"`
	Unlimited  bool     `json:"unlimited"`
	ObservedAt int64    `json:"observedAt"`
}

func validEnterpriseBudget(value *EnterpriseBudget) bool {
	if value == nil {
		return true
	}
	if value.Currency != "USD" || value.Period != "monthly" || !validUSD(value.Spent) || value.ObservedAt <= 0 || value.ObservedAt > 253402300799 {
		return false
	}
	if value.Unlimited {
		return value.Limit == nil && value.Remaining == nil
	}
	if value.Limit == nil || value.Remaining == nil || !validUSD(*value.Limit) || !validUSD(*value.Remaining) {
		return false
	}
	expected := math.Max(0, *value.Limit-value.Spent)
	return math.Abs(*value.Remaining-expected) <= math.Max(1e-8, *value.Limit*1e-12)
}

func cloneEnterpriseBudget(value *EnterpriseBudget) *EnterpriseBudget {
	if value == nil {
		return nil
	}
	copy := *value
	if value.Limit != nil {
		limit := *value.Limit
		copy.Limit = &limit
	}
	if value.Remaining != nil {
		remaining := *value.Remaining
		copy.Remaining = &remaining
	}
	return &copy
}

// SetEnterpriseBudgetAt discards responses fetched before an account/settings
// change. No identity, Admin API key, or raw response enters session history.
func (r *Runtime) SetEnterpriseBudgetAt(generation uint64, value *EnterpriseBudget) error {
	if r == nil || r.server == nil {
		return errors.New("Claude runtime is unavailable")
	}
	if !validEnterpriseBudget(value) {
		return errors.New("Invalid Claude Enterprise budget")
	}
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("Claude runtime is closed")
	}
	if generation != s.balanceGeneration {
		return errors.New("Claude Enterprise budget observation expired")
	}
	s.enterpriseBudget = cloneEnterpriseBudget(value)
	for _, thread := range s.threads {
		s.broadcastStatus(thread)
	}
	return nil
}
