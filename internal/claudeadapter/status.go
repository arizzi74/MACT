package claudeadapter

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// ClaudeStatus contains only display-safe session observations. Nil numbers
// mean unknown; a non-nil zero is an explicitly reported zero. Cost is the
// CLI's reported USD estimate, not an account quota or billing statement.
type ClaudeStatus struct {
	Provider         string            `json:"provider"`
	Model            *string           `json:"model"`
	ModelDisplayName *string           `json:"modelDisplayName,omitempty"`
	ReasoningEffort  *string           `json:"reasoningEffort"`
	PermissionMode   *string           `json:"permissionMode"`
	Context          ContextUsage      `json:"context"`
	Cost             ReportedCost      `json:"cost"`
	Balance          *CreditBalance    `json:"balance"`
	Usage            *NativeUsage      `json:"usage,omitempty"`
	EnterpriseBudget *EnterpriseBudget `json:"enterpriseBudget,omitempty"`
}

type ContextUsage struct {
	UsedTokens *int64 `json:"usedTokens"`
	MaxTokens  *int64 `json:"maxTokens"`
}

type ReportedCost struct {
	SessionUSD  *float64 `json:"sessionUSD"`
	LastTurnUSD *float64 `json:"lastTurnUSD"`
	// Only snapshots expose this lower bound when a native accounting reset
	// prevents presenting a complete session total.
	KnownSessionUSD *float64 `json:"knownSessionUSD,omitempty"`
	// Incomplete survives restarts after a native accounting reset or a
	// zeroed crash report. The saved SessionUSD then retains only internal
	// high-water evidence; snapshots must not present it as a current total.
	Incomplete bool `json:"incomplete,omitempty"`
}

// CreditBalance is an optional provider-reported account observation supplied
// by the worker. ObservedAt is UTC Unix seconds. The adapter never fetches an
// account endpoint and never persists this value with a conversation.
type CreditBalance struct {
	Remaining  float64 `json:"remaining"`
	Unit       string  `json:"unit"`
	ObservedAt int64   `json:"observedAt"`
}

func (r *Runtime) SetBalance(balance *CreditBalance) error {
	return r.setBalance(nil, balance)
}

func (r *Runtime) BalanceGeneration() uint64 {
	if r == nil || r.server == nil {
		return 0
	}
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balanceGeneration
}

// SetBalanceAt rejects an observation fetched before an account/settings
// change. Callers capture BalanceGeneration before reading private settings
// and performing their read-only network request.
func (r *Runtime) SetBalanceAt(generation uint64, balance *CreditBalance) error {
	return r.setBalance(&generation, balance)
}

func (r *Runtime) setBalance(generation *uint64, balance *CreditBalance) error {
	if r == nil || r.server == nil {
		return errors.New("Claude runtime is unavailable")
	}
	if balance != nil && (!validUSD(balance.Remaining) || (balance.Unit != "USD" && balance.Unit != "credits") || balance.ObservedAt < 0 || balance.ObservedAt > 253402300799) {
		return errors.New("Invalid Claude provider balance")
	}
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("Claude runtime is closed")
	}
	if generation != nil && *generation != s.balanceGeneration {
		return errors.New("Claude provider balance observation expired")
	}
	s.balance = cloneBalance(balance)
	for _, thread := range s.threads {
		s.broadcastStatus(thread)
	}
	return nil
}

func cloneBalance(value *CreditBalance) *CreditBalance {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (t *savedThread) ensureStatus() *ClaudeStatus {
	if t.ClaudeStatus == nil {
		t.ClaudeStatus = &ClaudeStatus{Provider: "claude"}
	}
	return t.ClaudeStatus
}

func (t *savedThread) statusSnapshot(balance *CreditBalance, budget *EnterpriseBudget, usage *NativeUsage) *ClaudeStatus {
	copy := *t.ensureStatus()
	copy.Provider = "claude"
	copy.Usage = cloneNativeUsage(usage)
	copy.Balance = cloneBalance(balance)
	copy.EnterpriseBudget = cloneEnterpriseBudget(budget)
	copy.Cost.KnownSessionUSD = nil
	if copy.Cost.Incomplete {
		copy.Cost.KnownSessionUSD = copy.Cost.SessionUSD
		copy.Cost.SessionUSD = nil
		copy.Cost.LastTurnUSD = nil
	}
	return &copy
}

func (s *server) threadMetadata(t *savedThread, includeTurns bool) map[string]any {
	value := t.metadata(includeTurns)
	value["claudeStatus"] = s.statusSnapshot(t)
	value["threadSettings"] = t.settingsSnapshot()
	return value
}

func (s *server) statusSnapshot(t *savedThread) *ClaudeStatus {
	status := t.statusSnapshot(s.balance, s.enterpriseBudget, s.nativeUsage)
	status.ModelDisplayName = nil
	if status.Model != nil {
		for _, model := range s.capabilities.models {
			if model.Value == *status.Model {
				status.ModelDisplayName = optionalString(model.DisplayName)
				return status
			}
		}
		for _, model := range s.capabilities.models {
			if model.Value != "default" && model.ResolvedModel == *status.Model {
				status.ModelDisplayName = optionalString(model.DisplayName)
				break
			}
		}
	}
	return status
}

func (s *server) broadcastStatus(t *savedThread) {
	if s.deletions[t.ID] != nil {
		return
	}
	s.broadcast("thread/claudeStatus/updated", map[string]any{"threadId": t.ID, "claudeStatus": s.statusSnapshot(t)})
}

func (p *process) publishStatus(t *savedThread) {
	if err := p.server.save(t); err != nil {
		p.stop()
		return
	}
	p.server.broadcastStatus(t)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func sameString(pointer *string, value string) bool { return pointer != nil && *pointer == value }

func permissionMode(value string) bool {
	switch value {
	case "default", "acceptEdits", "plan", "bypassPermissions", "dontAsk", "auto":
		return true
	}
	return false
}

// Settings are safe preference fields only. Updating an idle runtime replaces
// stale native model/permission observations; unknown preferences remain nil.
func (s *server) applyStatusSettings(t *savedThread, settings LaunchSettings, refresh bool) {
	status := t.ensureStatus()
	settings = t.effectiveSettings(settings)
	model := settings.Model
	if model != "" && validateModel(model) != nil {
		model = ""
	}
	mode := settings.PermissionMode
	if !permissionMode(mode) {
		mode = ""
	}
	changed := t.StatusConfigModel != model
	if refresh || changed || status.Model == nil {
		status.Model = optionalString(model)
		t.Model = model
	}
	if changed || refresh {
		status.Context = ContextUsage{}
	}
	if refresh || t.StatusConfigPermission != mode || status.PermissionMode == nil {
		status.PermissionMode = optionalString(mode)
	}
	status.ReasoningEffort = optionalString(settings.ReasoningEffort)
	if t.RequestedEffort != nil && *t.RequestedEffort == "" {
		value := ""
		status.ReasoningEffort = &value
	}
	t.StatusConfigEffort = settings.ReasoningEffort
	t.StatusConfigModel = model
	t.StatusConfigPermission = mode
	status.Balance = nil
	status.EnterpriseBudget = nil
	status.Usage = nil
	status.ModelDisplayName = nil
}

func validUSD(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 }

const maxSafeTokens = int64(9007199254740991)

func rawNumber(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || !validUSD(value) {
		return nil
	}
	return &value
}

func rawTokens(raw json.RawMessage, positive bool) *int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 || value > maxSafeTokens || (positive && value == 0) {
		return nil
	}
	return &value
}

func contextTokens(raw json.RawMessage) *int64 {
	var usage map[string]json.RawMessage
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	input := rawTokens(usage["input_tokens"], false)
	if input == nil {
		return nil
	}
	total := *input
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if value, exists := usage[key]; exists {
			tokens := rawTokens(value, false)
			if tokens == nil || *tokens > maxSafeTokens-total {
				return nil
			}
			total += *tokens
		}
	}
	return &total
}

func (p *process) observeUsage(t *savedThread, model string, usage json.RawMessage) bool {
	status := t.ensureStatus()
	changed := false
	if model != "" && validateModel(model) == nil && !sameString(status.Model, model) {
		status.Model = optionalString(model)
		t.Model = model
		status.Context.MaxTokens = nil
		changed = true
	}
	if tokens := contextTokens(usage); tokens != nil {
		if status.Context.UsedTokens == nil || *status.Context.UsedTokens != *tokens {
			status.Context.UsedTokens = tokens
			changed = true
		}
	}
	return changed
}

// Native result total_cost_usd and modelUsage are cumulative across streamed
// turns and, since Claude Code 2.1.277, include restored spend on --resume.
// Read the latest total rather than adding it to earlier results:
// https://code.claude.com/docs/en/agent-sdk/cost-tracking
func (p *process) observeResult(t *savedThread, raw json.RawMessage, subtype string) {
	var result struct {
		Total      json.RawMessage            `json:"total_cost_usd"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return
	}
	status := t.ensureStatus()
	status.Cost.LastTurnUSD = nil
	if total := rawNumber(result.Total); total != nil {
		prior := status.Cost.SessionUSD
		crashZero := subtype == "error_during_execution" && *total == 0
		if prior != nil && (crashZero || *total < *prior) {
			status.Cost.Incomplete = true
		}
		if !crashZero && (prior == nil || *total >= *prior) {
			if !status.Cost.Incomplete && prior != nil {
				delta := *total - *prior
				status.Cost.LastTurnUSD = &delta
			} else if !status.Cost.Incomplete && len(t.Turns) == 1 {
				delta := *total
				status.Cost.LastTurnUSD = &delta
			}
			status.Cost.SessionUSD = total
		}
	}
	var modelUsage json.RawMessage
	if status.Model != nil {
		modelUsage = result.ModelUsage[*status.Model]
	}
	var model struct {
		ContextWindow json.RawMessage `json:"contextWindow"`
	}
	if json.Unmarshal(modelUsage, &model) == nil {
		if limit := rawTokens(model.ContextWindow, true); limit != nil {
			status.Context.MaxTokens = limit
		}
	}
}

func sanitizeSavedStatus(t *savedThread) {
	status := t.ensureStatus()
	status.Cost.KnownSessionUSD = nil // Derived from validated accounting evidence.
	status.Provider = "claude"
	status.Balance = nil
	status.EnterpriseBudget = nil
	status.Usage = nil
	if status.Model != nil && validateModel(*status.Model) != nil {
		status.Model = nil
	}
	if status.ReasoningEffort != nil && !reasoningEffort(*status.ReasoningEffort) &&
		!(*status.ReasoningEffort == "" && t.RequestedEffort != nil && *t.RequestedEffort == "") {
		status.ReasoningEffort = nil
	}
	if t.RequestedEffort != nil && *t.RequestedEffort != "" && !reasoningEffort(*t.RequestedEffort) {
		t.RequestedEffort = nil
	}
	if t.RequestedPermissionMode != "" && !permissionMode(t.RequestedPermissionMode) {
		t.RequestedPermissionMode = ""
	}
	if status.PermissionMode != nil && !permissionMode(*status.PermissionMode) {
		status.PermissionMode = nil
	}
	for _, value := range []**int64{&status.Context.UsedTokens, &status.Context.MaxTokens} {
		if *value != nil && (**value < 0 || **value > maxSafeTokens) {
			*value = nil
		}
	}
	if status.Context.MaxTokens != nil && *status.Context.MaxTokens == 0 {
		status.Context.MaxTokens = nil
	}
	for _, value := range []**float64{&status.Cost.SessionUSD, &status.Cost.LastTurnUSD} {
		if *value != nil && !validUSD(**value) {
			*value = nil
		}
	}
	if strings.TrimSpace(t.RequestedModel) != t.RequestedModel || (t.RequestedModel != "" && validateModel(t.RequestedModel) != nil) {
		t.RequestedModel = ""
	}
}
