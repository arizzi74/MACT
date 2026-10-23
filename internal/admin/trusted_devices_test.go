package admin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wa "github.com/go-webauthn/webauthn/webauthn"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestTrustedDevicePasskeyHTTPChoiceCookiesAndFreshness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentialID, handle := []byte("trusted-device-test-passkey"), []byte("trusted-device-test-owner-32bytes")
	raw, err := json.Marshal(wa.Credential{ID: credentialID, PublicKey: coseP256(&key.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := store.BootstrapAdmin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registration, err := store.NewAdminCeremony(t.Context(), "registration", handle, []byte(`{"challenge":"dHJ1c3RlZC1kZXZpY2U"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteBootstrapCredential(t.Context(), registration.ID, registration.Binding, bootstrap, registry.AdminCredential{ID: credentialID, UserHandle: handle, CredentialJSON: raw}); err != nil {
		t.Fatal(err)
	}
	admin, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(admin)
	t.Cleanup(server.Close)
	client := passkeyHTTPClient(t, server)
	requireHTTPStatus(t, adminRequest(t, client, passkeyTestOrigin, "GET", "/tgw/admin/", nil, "", ""), 200)
	csrf := cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	type login struct {
		CeremonyID string `json:"ceremony_id"`
		PublicKey  struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	begin := func(body any) login {
		t.Helper()
		var c login
		decodeHTTPJSON(t, adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/login/begin", body, passkeyTestOrigin, csrf), 200, &c)
		return c
	}
	finish := func(c login, flags byte, extra map[string]any) *http.Response {
		t.Helper()
		body := map[string]any{"ceremony_id": c.CeremonyID, "credential": browserAssertion(t, key, credentialID, handle, c.PublicKey.Challenge, passkeyTestOrigin, "gateway.example.com", flags)}
		for k, v := range extra {
			body[k] = v
		}
		return adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/login/finish", body, passkeyTestOrigin, csrf)
	}
	// Existing empty-body clients remain ordinary eight-hour logins.
	c := begin(nil)
	response := finish(c, 0x05, nil)
	assertTrustedCookieAttributes(t, response, time.Time{})
	requireHTTPStatus(t, response, 200)
	csrf = cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	ordinary := cookieValue(t, client, passkeyTestOrigin, adminCookie)
	info, err := store.AdminSessionInfo(t.Context(), ordinary)
	if err != nil || info.TrustedDevice || info.ExpiresAt.Sub(info.CreatedAt) != 8*time.Hour {
		t.Fatalf("ordinary passkey login: %+v %v", info, err)
	}
	for _, body := range []map[string]any{{"trust_device": true, "ttl": 1}, {"trust_device": "true"}, {"device_name": strings.Repeat("x", 81)}, {"device_name": "<b>phone</b>"}} {
		requireHTTPStatus(t, adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/login/begin", body, passkeyTestOrigin, csrf), 400)
	}
	c = begin(map[string]any{"trust_device": true, "device_name": "  My phone  "})
	// Trust requires a verified assertion, and FINISH cannot alter BEGIN's choice.
	requireHTTPStatus(t, finish(c, 0x01, nil), 400)
	response = finish(c, 0x05, map[string]any{"trust_device": false})
	if len(response.Cookies()) != 0 {
		t.Fatal("invalid finish changed authentication cookies")
	}
	requireHTTPStatus(t, response, 400)
	response = finish(c, 0x05, nil)
	token := cookieValue(t, client, passkeyTestOrigin, adminCookie)
	trusted, err := store.AdminSessionInfo(t.Context(), token)
	if err != nil || !trusted.TrustedDevice || trusted.DeviceName != "My phone" || trusted.ExpiresAt.Sub(trusted.CreatedAt) != 30*24*time.Hour {
		t.Fatalf("trusted passkey login: %+v %v", trusted, err)
	}
	assertTrustedCookieAttributes(t, response, trusted.ExpiresAt)
	requireHTTPStatus(t, response, 200)
	csrf = cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	var metadata registry.AdminBrowserSession
	decodeHTTPJSON(t, adminRequest(t, client, passkeyTestOrigin, "GET", "/tgw/api/v1/admin/session", nil, "", ""), 200, &metadata)
	if !metadata.TrustedDevice || metadata.DeviceName != trusted.DeviceName || metadata.DeviceID != trusted.DeviceID || metadata.BrowserLabel == "" || !metadata.ExpiresAt.Equal(trusted.ExpiresAt) {
		t.Fatalf("missing trusted device metadata: %+v", metadata)
	}
	for _, shell := range []string{"/tgw/admin/", "/tgw/webui/"} {
		response = adminRequest(t, client, passkeyTestOrigin, "GET", shell, nil, "", "")
		cookies := response.Cookies()
		if len(cookies) != 1 || cookies[0].Name != csrfCookie || !cookies[0].Expires.Equal(trusted.ExpiresAt.Truncate(time.Second)) || cookies[0].MaxAge <= 0 {
			t.Fatalf("shell shortened trusted CSRF lifetime: %+v", cookies)
		}
		requireHTTPStatus(t, response, 200)
	}
	csrf = cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z' WHERE revoked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	response = adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/workers", map[string]string{"name": "new-worker"}, passkeyTestOrigin, csrf)
	var gate struct {
		Code string `json:"code"`
	}
	decodeHTTPJSON(t, response, 403, &gate)
	if gate.Code != "reauthentication_required" {
		t.Fatal("trust bypassed recent authentication")
	}
	c = begin(struct{}{})
	response = finish(c, 0x05, nil)
	assertTrustedCookieAttributes(t, response, trusted.ExpiresAt)
	requireHTTPStatus(t, response, 200)
	csrf = cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	renewed, err := store.AdminSessionInfo(t.Context(), cookieValue(t, client, passkeyTestOrigin, adminCookie))
	if err != nil || !renewed.TrustedDevice || !renewed.ExpiresAt.Equal(trusted.ExpiresAt) || renewed.DeviceID != trusted.DeviceID {
		t.Fatalf("sensitive reauthentication changed trust deadline: %+v %v", renewed, err)
	}
	requireHTTPStatus(t, adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/workers", map[string]string{"name": "new-worker"}, passkeyTestOrigin, csrf), 201)
	c = begin(map[string]any{"trust_device": false})
	requireHTTPStatus(t, finish(c, 0x05, map[string]any{"trust_device": true}), 400)
	response = finish(c, 0x05, nil)
	assertTrustedCookieAttributes(t, response, time.Time{})
	requireHTTPStatus(t, response, 200)
	csrf = cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	untrusted, err := store.AdminSessionInfo(t.Context(), cookieValue(t, client, passkeyTestOrigin, adminCookie))
	if err != nil || untrusted.TrustedDevice || untrusted.ExpiresAt.Sub(untrusted.CreatedAt) != 8*time.Hour || untrusted.DeviceID != trusted.DeviceID {
		t.Fatalf("explicit opt-out retained trust: %+v %v", untrusted, err)
	}
	// A stale inventory's session ID still revokes this rotated current device.
	response = adminRequest(t, client, passkeyTestOrigin, "DELETE", "/tgw/api/v1/admin/sessions/"+trusted.ID.String(), nil, passkeyTestOrigin, csrf)
	cleared, signInCSRF := false, false
	for _, c := range response.Cookies() {
		cleared = cleared || c.Name == adminCookie && c.MaxAge < 0
		signInCSRF = signInCSRF || c.Name == csrfCookie && c.Value != "" && c.MaxAge == 0
	}
	if !cleared || !signInCSRF {
		t.Fatal("stale current-device revocation did not restore sign-in cookies")
	}
	requireHTTPStatus(t, response, 204)
	requireHTTPStatus(t, adminRequest(t, client, passkeyTestOrigin, "GET", "/tgw/api/v1/admin/session", nil, "", ""), 401)
}

func assertTrustedCookieAttributes(t *testing.T, response *http.Response, deadline time.Time) {
	t.Helper()
	if len(response.Cookies()) != 3 {
		t.Fatalf("missing session/CSRF/ceremony cookies: %+v", response.Cookies())
	}
	for _, c := range response.Cookies() {
		if c.Name == ceremonyCookie {
			continue
		}
		if !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Domain != "" || c.Path != "/" || c.HttpOnly != (c.Name == adminCookie) {
			t.Fatalf("insecure auth cookie: %+v", c)
		}
		if deadline.IsZero() {
			if c.MaxAge != 0 || !c.Expires.IsZero() {
				t.Fatalf("ordinary cookie persisted: %+v", c)
			}
		} else if !c.Expires.Equal(deadline.Truncate(time.Second)) || c.MaxAge <= 0 || c.MaxAge > int(time.Until(deadline)/time.Second)+1 {
			t.Fatalf("trusted cookie deadline differs from server: %+v vs %s", c, deadline)
		}
	}
}

func TestLoginOptionsRestoresMissingCSRFOnly(t *testing.T) {
	store := adminIntegrationStore(t)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"", "existing-valid-token"} {
		r := httptest.NewRequest("GET", "/tgw/api/v1/admin/login/options", nil)
		if existing != "" {
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: existing})
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("login options: %d %s", w.Code, w.Body.String())
		}
		cookies := w.Result().Cookies()
		if existing != "" {
			if len(cookies) != 0 {
				t.Fatal("options rotated an existing CSRF cookie")
			}
			continue
		}
		if len(cookies) != 1 || cookies[0].Name != csrfCookie || cookies[0].Value == "" || !cookies[0].Secure || cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
			t.Fatalf("missing sign-in CSRF bootstrap: %+v", cookies)
		}
		request := httptest.NewRequest("POST", "/tgw/api/v1/admin/login/begin", strings.NewReader(`{}`))
		request.Header.Set("Origin", passkeyTestOrigin)
		request.Header.Set("X-CSRF-Token", cookies[0].Value)
		request.AddCookie(cookies[0])
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("restored CSRF could not begin authentication: %d %s", response.Code, response.Body.String())
		}
	}
	// The bootstrap endpoint does not relax authentication mutation checks.
	response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/login/begin", `{}`, "", passkeyTestOrigin, "", "")
	if response.Code != 403 {
		t.Fatal("authentication accepted missing CSRF")
	}
}
