package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type runtimeSettingsFixture struct {
	id       uuid.UUID
	update   *protocol.WorkerRuntimeSettingsUpdate
	calls    int
	err      error
	settings *protocol.WorkerRuntimeSettings
}

func (f *runtimeSettingsFixture) WorkerRuntimeSettings(_ context.Context, id uuid.UUID, update *protocol.WorkerRuntimeSettingsUpdate) (protocol.WorkerRuntimeSettings, error) {
	f.id, f.update, f.calls = id, update, f.calls+1
	if f.settings != nil {
		return *f.settings, f.err
	}
	return protocol.WorkerRuntimeSettings{Provider: "claude", Model: "custom[1m]", BaseURL: "https://provider.example", AuthMode: "auth_token", PermissionMode: "default", APIKeyConfigured: true}, f.err
}

func TestCodexWorkerDefaultsHTTPAuthAndValidation(t *testing.T) {
	s, store, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	id := uuid.New()
	if _, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{ID: id, Name: "Codex", OS: "linux", Arch: "amd64", TokenHash: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	defaults := protocol.CodexSessionDefaults{Model: "custom-model", ReasoningEffort: "xhigh", PermissionLevel: "full-access"}
	manager := &runtimeSettingsFixture{settings: &protocol.WorkerRuntimeSettings{Provider: "codex", CodexDefaults: &defaults}}
	s.runtimeSettings = manager
	endpoint := "/tgw/api/v1/admin/workers/" + id.String() + "/runtime-settings"
	const payload = `{"codex_defaults":{"model":"custom-model","reasoning_effort":"xhigh","permission_level":"full-access"}}`
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		got := webUICommandRequestForTest(s, method, endpoint, payload, "", passkeyTestOrigin, "csrf", "csrf")
		if got.Code != http.StatusUnauthorized || manager.calls != 0 {
			t.Fatal("unauthenticated Codex defaults access")
		}
	}
	for _, origin := range []string{"https://evil.example", ""} {
		got := webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, token, origin, "csrf", "csrf")
		if got.Code != http.StatusForbidden || manager.calls != 0 {
			t.Fatal("cross-origin Codex defaults update")
		}
	}
	write := webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, token, passkeyTestOrigin, "csrf", "csrf")
	if write.Code != http.StatusOK || manager.update == nil || manager.update.CodexDefaults == nil || *manager.update.CodexDefaults != defaults || manager.id != id {
		t.Fatalf("Codex defaults did not reach selected worker: %d %s", write.Code, write.Body.String())
	}
	read := webUICommandRequestForTest(s, http.MethodGet, endpoint, "", token, "", "", "")
	if read.Code != http.StatusOK || read.Header().Get("Cache-Control") != "no-store" || !strings.Contains(read.Body.String(), `"provider":"codex"`) || !strings.Contains(read.Body.String(), `"permission_level":"full-access"`) {
		t.Fatalf("Codex defaults not projected: %d %s", read.Code, read.Body.String())
	}
	for _, invalid := range []string{
		`{"codex_defaults":{"model":"x","reasoning_effort":"invented","permission_level":""}}`,
		`{"codex_defaults":{"model":"x","permission_level":"bypassPermissions"}}`,
		`{"codex_defaults":{},"api_key":"private"}`,
		`{"codex_defaults":{"shell":"sudo"}}`,
	} {
		before := manager.calls
		got := webUICommandRequestForTest(s, http.MethodPut, endpoint, invalid, token, passkeyTestOrigin, "csrf", "csrf")
		if got.Code != http.StatusBadRequest || manager.calls != before {
			t.Fatal("invalid defaults reached worker")
		}
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	before := manager.calls
	stale := webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, token, passkeyTestOrigin, "csrf", "csrf")
	if stale.Code != http.StatusForbidden || !strings.Contains(stale.Body.String(), "reauthentication_required") || manager.calls != before {
		t.Fatal("permission defaults changed without fresh administrator authentication")
	}
	if err := store.RevokeWorker(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	missing := webUICommandRequestForTest(s, http.MethodGet, endpoint, "", token, "", "", "")
	if missing.Code != http.StatusNotFound || manager.calls != before {
		t.Fatal("revoked worker defaults remained accessible")
	}
}

func TestWorkerRuntimeSettingsHTTPAuthBoundariesAndCredentialPrivacy(t *testing.T) {
	s, store, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	id := uuid.New()
	if _, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{ID: id, Name: "Claude", OS: "linux", Arch: "arm64", TokenHash: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	manager := &runtimeSettingsFixture{}
	s.runtimeSettings = manager
	endpoint := "/tgw/api/v1/admin/workers/" + id.String() + "/runtime-settings"
	const secret = "private-runtime-credential"
	const settings = `{"model":"custom[1m]","base_url":"https://provider.example","auth_mode":"auth_token","permission_mode":"default"`
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		got := webUICommandRequestForTest(s, method, endpoint, settings+`}`, "", passkeyTestOrigin, "csrf", "csrf")
		if got.Code != 401 {
			t.Fatalf("unauthenticated runtime settings access: %d", got.Code)
		}
	}
	for _, tc := range []struct{ origin, header, cookie string }{{"https://evil.example", "csrf", "csrf"}, {"", "csrf", "csrf"}, {passkeyTestOrigin, "wrong", "csrf"}} {
		got := webUICommandRequestForTest(s, http.MethodPut, endpoint, settings+`}`, token, tc.origin, tc.header, tc.cookie)
		if got.Code != 403 {
			t.Fatal("unsafe configuration write accepted")
		}
	}
	if manager.calls != 0 {
		t.Fatal("unauthorized request reached the worker")
	}
	for _, suffix := range []string{`}`, `,"api_key":"` + secret + `"}`, `,"api_key":""}`} {
		got := webUICommandRequestForTest(s, http.MethodPut, endpoint, settings+suffix, token, passkeyTestOrigin, "csrf", "csrf")
		if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" || strings.Contains(got.Body.String(), secret) || strings.Contains(got.Body.String(), `"api_key":`) {
			t.Fatalf("runtime credential leaked or update failed: %d", got.Code)
		}
		if manager.id != id || manager.update == nil {
			t.Fatal("runtime settings targeted the wrong worker")
		}
		if suffix == `}` && manager.update.APIKey != nil || suffix == `,"api_key":""}` && (manager.update.APIKey == nil || *manager.update.APIKey != "") || strings.Contains(suffix, secret) && (manager.update.APIKey == nil || *manager.update.APIKey != secret) {
			t.Fatal("credential keep, replace or clear semantics were lost")
		}
	}
	read := webUICommandRequestForTest(s, http.MethodGet, endpoint, "", token, "", "", "")
	if read.Code != 200 || manager.update != nil || strings.Contains(read.Body.String(), secret) || !strings.Contains(read.Body.String(), `"api_key_configured":true`) {
		t.Fatal("read did not return a safe worker-local credential projection")
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{errors.New(secret), 503}, {gateway.ErrWorkerRuntimeSettingsBusy, 409}, {gateway.ErrWorkerRuntimeSettingsUnsupported, 409}, {gateway.ErrWorkerRuntimeSettingsInvalid, 400}} {
		manager.err = tc.err
		got := webUICommandRequestForTest(s, http.MethodPut, endpoint, settings+`,"api_key":"`+secret+`"}`, token, passkeyTestOrigin, "csrf", "csrf")
		if got.Code != tc.status || strings.Contains(got.Body.String(), secret) {
			t.Fatal("runtime failure leaked worker text or returned wrong status")
		}
	}
	manager.err = nil
	for _, invalid := range []string{settings + `,"api_key":"` + secret + `","env":{}}`, settings + `,"api_key":"` + secret + `"} {}`, strings.Replace(settings+`}`, "https://provider.example", "https://user:private-runtime-credential@provider.example", 1)} {
		before := manager.calls
		got := webUICommandRequestForTest(s, http.MethodPut, endpoint, invalid, token, passkeyTestOrigin, "csrf", "csrf")
		if got.Code != 400 || manager.calls != before || strings.Contains(got.Body.String(), secret) {
			t.Fatal("invalid runtime settings reached worker or leaked a credential")
		}
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	before := manager.calls
	stale := webUICommandRequestForTest(s, http.MethodPut, endpoint, settings+`}`, token, passkeyTestOrigin, "csrf", "csrf")
	if stale.Code != 403 || !strings.Contains(stale.Body.String(), "reauthentication_required") || manager.calls != before {
		t.Fatal("stale admin session changed runtime settings")
	}
}
