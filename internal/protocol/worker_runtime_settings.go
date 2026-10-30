package protocol

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// WorkerRuntimeSettings is the public projection of a worker's local runtime
// configuration. It must never contain credentials, even in error responses.
type WorkerRuntimeSettings struct {
	Provider                 string                          `json:"provider"`
	Model                    string                          `json:"model"`
	ReasoningEffort          string                          `json:"reasoning_effort"`
	SupportedPermissionModes []string                        `json:"supported_permission_modes,omitempty"`
	BaseURL                  string                          `json:"base_url"`
	AuthMode                 string                          `json:"auth_mode"`
	PermissionMode           string                          `json:"permission_mode"`
	EnterpriseBudget         *ClaudeEnterpriseBudgetSettings `json:"enterprise_budget,omitempty"`
	APIKeyConfigured         bool                            `json:"api_key_configured"`
	CodexDefaults            *CodexSessionDefaults           `json:"codex_defaults,omitempty"`
}

// CodexSessionDefaults apply to a newly created thread (including /clear),
// never to an existing thread merely because it is selected or resumed.
// Empty model and effort inherit native Codex configuration. Empty permissions
// retain MACT's workspace-write/on-request default for new sessions.
type CodexSessionDefaults struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	PermissionLevel string `json:"permission_level"`
}

func (s CodexSessionDefaults) Validate() error {
	if s.Model != "" && (!ModelMenuToken(s.Model) || strings.HasPrefix(s.Model, "-")) {
		return errors.New("invalid default model")
	}
	switch s.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "ultra":
	default:
		return errors.New("invalid default reasoning effort")
	}
	switch s.PermissionLevel {
	case "", "ask-for-approval", "read-only", "full-access":
	default:
		return errors.New("invalid default permission level")
	}
	return nil
}

// WorkerRuntimeSettingsUpdate travels only over the authenticated live worker
// connection. A nil APIKey retains the worker's key; a pointer to "" removes it.
// Never put this payload in a durable command, event, audit log, or response.
type WorkerRuntimeSettingsUpdate struct {
	Model            string                        `json:"model"`
	ReasoningEffort  string                        `json:"reasoning_effort"`
	BaseURL          string                        `json:"base_url"`
	AuthMode         string                        `json:"auth_mode"`
	PermissionMode   string                        `json:"permission_mode"`
	EnterpriseBudget *ClaudeEnterpriseBudgetUpdate `json:"enterprise_budget,omitempty"`
	APIKey           *string                       `json:"api_key,omitempty"`
	CodexDefaults    *CodexSessionDefaults         `json:"codex_defaults,omitempty"`
}

// ClaudeEnterpriseBudgetSettings exposes enrollment state without the separate
// Enterprise Admin API credential. It is independent of conversation API costs.
type ClaudeEnterpriseBudgetSettings struct {
	Enabled          bool   `json:"enabled"`
	UserID           string `json:"user_id"`
	APIKeyConfigured bool   `json:"api_key_configured"`
}

type ClaudeEnterpriseBudgetUpdate struct {
	Enabled bool    `json:"enabled"`
	UserID  string  `json:"user_id"`
	APIKey  *string `json:"api_key,omitempty"`
}

var claudeEnterpriseUserID = regexp.MustCompile(`^user_[A-Za-z0-9]+$`)

func ValidClaudeEnterpriseBudgetUserID(value string) bool {
	return len(value) <= 128 && claudeEnterpriseUserID.MatchString(value)
}

func (s ClaudeEnterpriseBudgetUpdate) Validate() error {
	if s.UserID != "" && !ValidClaudeEnterpriseBudgetUserID(s.UserID) {
		return errors.New("invalid Enterprise budget user")
	}
	if s.Enabled && s.UserID == "" {
		return errors.New("Enterprise budget requires a user")
	}
	if s.APIKey != nil && (!validRuntimeSettingText(*s.APIKey, 8192) || strings.IndexFunc(*s.APIKey, unicode.IsSpace) >= 0) {
		return errors.New("invalid Enterprise budget credential")
	}
	return nil
}

func validRuntimeSettingText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validateWorkerRuntimeSettings(model, effort, baseURL, authMode, permissionMode string) error {
	if !ValidClaudeReasoningEffort(effort) {
		return errors.New("invalid runtime reasoning effort")
	}
	if !validRuntimeSettingText(model, 256) || (model != "" && !ModelMenuToken(model)) || strings.HasPrefix(model, "-") || !validRuntimeSettingText(baseURL, 2048) {
		return errors.New("invalid runtime settings")
	}
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return errors.New("runtime endpoint must be HTTPS without credentials, query or fragment")
		}
	}
	if authMode != "login" && authMode != "api_key" && authMode != "auth_token" {
		return errors.New("invalid runtime authentication mode")
	}
	switch permissionMode {
	case "default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions":
	default:
		return errors.New("invalid runtime permission mode")
	}
	return nil
}

func (s WorkerRuntimeSettings) Validate() error {
	if s.Provider == "codex" {
		if s.CodexDefaults == nil || s.Model != "" || s.ReasoningEffort != "" || len(s.SupportedPermissionModes) != 0 || s.EnterpriseBudget != nil || s.BaseURL != "" || s.AuthMode != "" || s.PermissionMode != "" || s.APIKeyConfigured {
			return errors.New("invalid Codex runtime settings")
		}
		return s.CodexDefaults.Validate()
	}
	if s.Provider != "claude" {
		return errors.New("unsupported runtime settings provider")
	}
	if s.CodexDefaults != nil {
		return errors.New("unexpected Codex defaults")
	}
	if s.EnterpriseBudget != nil {
		budget := s.EnterpriseBudget
		if err := (ClaudeEnterpriseBudgetUpdate{Enabled: budget.Enabled, UserID: budget.UserID}).Validate(); err != nil {
			return err
		}
		if budget.Enabled && (s.AuthMode != "login" || s.BaseURL != "" || !budget.APIKeyConfigured) {
			return errors.New("Enterprise budget requires the official login account and Admin API credential")
		}
	}
	if len(s.SupportedPermissionModes) > 6 {
		return errors.New("invalid supported permission modes")
	}
	seenModes := map[string]bool{}
	for _, mode := range s.SupportedPermissionModes {
		if !ValidClaudePermissionMode(mode) || seenModes[mode] {
			return errors.New("invalid supported permission mode")
		}
		seenModes[mode] = true
	}
	return validateWorkerRuntimeSettings(s.Model, s.ReasoningEffort, s.BaseURL, s.AuthMode, s.PermissionMode)
}

func (s WorkerRuntimeSettingsUpdate) Validate() error {
	if s.CodexDefaults != nil {
		if s.Model != "" || s.ReasoningEffort != "" || s.BaseURL != "" || s.AuthMode != "" || s.PermissionMode != "" || s.APIKey != nil || s.EnterpriseBudget != nil {
			return errors.New("mixed runtime settings providers")
		}
		return s.CodexDefaults.Validate()
	}
	if err := validateWorkerRuntimeSettings(s.Model, s.ReasoningEffort, s.BaseURL, s.AuthMode, s.PermissionMode); err != nil {
		return err
	}
	if s.EnterpriseBudget != nil {
		if err := s.EnterpriseBudget.Validate(); err != nil {
			return err
		}
		if s.EnterpriseBudget.Enabled && (s.AuthMode != "login" || s.BaseURL != "") {
			return errors.New("Enterprise budget requires the official login account")
		}
	}
	if s.APIKey != nil && (!validRuntimeSettingText(*s.APIKey, 8192) || strings.IndexFunc(*s.APIKey, unicode.IsSpace) >= 0) {
		return errors.New("invalid runtime credential")
	}
	return nil
}

// These frames are transient RPCs addressed to a worker, independent of any
// session. They are intentionally separate from the durable command protocol.
type WorkerRuntimeSettingsRequest struct {
	RequestID string                       `json:"request_id"`
	WorkerID  string                       `json:"worker_id"`
	Action    string                       `json:"action"`
	Settings  *WorkerRuntimeSettingsUpdate `json:"settings,omitempty"`
}

func (r WorkerRuntimeSettingsRequest) Validate() error {
	for _, value := range []string{r.RequestID, r.WorkerID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return errors.New("invalid runtime settings identity")
		}
	}
	switch r.Action {
	case "get":
		if r.Settings != nil {
			return errors.New("unexpected runtime settings")
		}
	case "put":
		if r.Settings == nil {
			return errors.New("missing runtime settings")
		}
		return r.Settings.Validate()
	default:
		return errors.New("invalid runtime settings action")
	}
	return nil
}

type WorkerRuntimeSettingsResult struct {
	RequestID string                 `json:"request_id"`
	Settings  *WorkerRuntimeSettings `json:"settings,omitempty"`
	ErrorCode string                 `json:"error_code,omitempty"`
}

func (r WorkerRuntimeSettingsResult) Validate() error {
	id, err := uuid.Parse(r.RequestID)
	if err != nil || id == uuid.Nil || id.String() != r.RequestID {
		return errors.New("invalid runtime settings result identity")
	}
	if r.ErrorCode != "" {
		if r.Settings != nil {
			return errors.New("unexpected runtime settings result")
		}
		switch r.ErrorCode {
		case "unsupported", "invalid_settings", "busy", "save_failed", "unavailable":
			return nil
		default:
			return errors.New("invalid runtime settings error")
		}
	}
	if r.Settings == nil {
		return errors.New("missing runtime settings result")
	}
	return r.Settings.Validate()
}

// ValidClaudeReasoningEffort validates native Claude effort names. Per-session
// choices must also be advertised by the selected model. Empty inherits default.
func ValidClaudeReasoningEffort(effort string) bool {
	switch effort {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func ValidClaudePermissionMode(mode string) bool {
	switch mode {
	case "default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions":
		return true
	}
	return false
}
