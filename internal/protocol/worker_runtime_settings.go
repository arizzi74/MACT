package protocol

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// WorkerRuntimeSettings is the public projection of a worker's local runtime
// configuration. It must never contain credentials, even in error responses.
type WorkerRuntimeSettings struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	BaseURL          string `json:"base_url"`
	AuthMode         string `json:"auth_mode"`
	PermissionMode   string `json:"permission_mode"`
	APIKeyConfigured bool   `json:"api_key_configured"`
}

// WorkerRuntimeSettingsUpdate travels only over the authenticated live worker
// connection. A nil APIKey retains the worker's key; a pointer to "" removes it.
// Never put this payload in a durable command, event, audit log, or response.
type WorkerRuntimeSettingsUpdate struct {
	Model          string  `json:"model"`
	BaseURL        string  `json:"base_url"`
	AuthMode       string  `json:"auth_mode"`
	PermissionMode string  `json:"permission_mode"`
	APIKey         *string `json:"api_key,omitempty"`
}

func validRuntimeSettingText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validateWorkerRuntimeSettings(model, baseURL, authMode, permissionMode string) error {
	if !validRuntimeSettingText(model, 256) || strings.HasPrefix(model, "-") || !validRuntimeSettingText(baseURL, 2048) {
		return errors.New("invalid runtime settings")
	}
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return errors.New("invalid runtime endpoint")
		}
	}
	if authMode != "login" && authMode != "api_key" && authMode != "auth_token" {
		return errors.New("invalid runtime authentication mode")
	}
	switch permissionMode {
	case "default", "acceptEdits", "plan", "bypassPermissions":
	default:
		return errors.New("invalid runtime permission mode")
	}
	return nil
}

func (s WorkerRuntimeSettings) Validate() error {
	if s.Provider != "claude" {
		return errors.New("unsupported runtime settings provider")
	}
	return validateWorkerRuntimeSettings(s.Model, s.BaseURL, s.AuthMode, s.PermissionMode)
}

func (s WorkerRuntimeSettingsUpdate) Validate() error {
	if err := validateWorkerRuntimeSettings(s.Model, s.BaseURL, s.AuthMode, s.PermissionMode); err != nil {
		return err
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
