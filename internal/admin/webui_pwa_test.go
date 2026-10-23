package admin

import (
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUINotificationAssetsAreScopedAndUncached(t *testing.T) {
	s, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, contentType string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), contentType) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status=%d headers=%v", path, w.Code, w.Header())
		}
		return w
	}
	w := request("/tgw/webui/manifest.webmanifest", "application/manifest+json")
	var manifest struct {
		ID, Scope, Display string
		Name, Description  string
		ShortName          string `json:"short_name"`
		StartURL           string `json:"start_url"`
		Icons              []struct{ Src string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "/tgw/webui/" || manifest.Scope != manifest.ID || manifest.StartURL != manifest.ID || manifest.Display != "standalone" {
		t.Fatalf("unexpected install scope: %+v", manifest)
	}
	if manifest.Name != "MACT · Multi Agent Control Tower" || manifest.ShortName != "MACT" || manifest.Description != "A control hub for coding agents across sessions and machines." {
		t.Fatalf("unexpected install branding: %+v", manifest)
	}
	if len(manifest.Icons) == 0 {
		t.Fatal("install manifest has no icons")
	}
	for _, icon := range manifest.Icons {
		if !strings.HasPrefix(icon.Src, "/tgw/webui/static/webui-icon") {
			t.Fatalf("icon escaped the public UI assets: %q", icon.Src)
		}
	}
	w = request("/tgw/webui/sw.js", "application/javascript")
	if w.Header().Get("Service-Worker-Allowed") != "/tgw/webui/" {
		t.Fatal("notification worker scope is not restricted to the web UI")
	}
	for _, icon := range []struct {
		path string
		size int
	}{{"/tgw/webui/static/webui-icon-192.png", 192}, {"/tgw/webui/static/webui-icon-512.png", 512}, {"/tgw/webui/static/webui-icon-180.png", 180}} {
		w := request(icon.path, "image/png")
		cfg, err := png.DecodeConfig(w.Body)
		if err != nil || cfg.Width != icon.size || cfg.Height != icon.size {
			t.Fatalf("invalid install icon %s: %+v %v", icon.path, cfg, err)
		}
	}
}

func TestInstalledConsoleStaysInAppScopeAndSharesAuthentication(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	s, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, auth string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if auth != "" {
			r.AddCookie(&http.Cookie{Name: adminCookie, Value: auth})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	manifestResponse := request(http.MethodGet, "/tgw/webui/manifest.webmanifest", "")
	var manifest struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(manifestResponse.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	const consolePath = "/tgw/webui/admin/"
	if manifest.Scope != "/tgw/webui/" || !strings.HasPrefix(consolePath, manifest.Scope) {
		t.Fatal("console escaped the existing installed-app scope")
	}
	const nextQuery = "?next=%2Ftgw%2Fwebui%2F"
	redirect := request(http.MethodGet, strings.TrimSuffix(consolePath, "/")+nextQuery, token)
	if redirect.Code != http.StatusTemporaryRedirect || redirect.Header().Get("Location") != consolePath+nextQuery {
		t.Fatalf("console slash redirect lost scope or return destination: %d %q", redirect.Code, redirect.Header().Get("Location"))
	}
	canonical := request(http.MethodGet, "/tgw/admin/", token)
	console := request(http.MethodGet, consolePath, token)
	if console.Code != http.StatusOK || console.Header().Get("Location") != "" || console.Body.String() != canonical.Body.String() {
		t.Fatalf("in-app console redirected or differs from the canonical shell: %d", console.Code)
	}
	if console.Header().Get("Cache-Control") != "no-store" || console.Header().Get("Content-Security-Policy") != canonical.Header().Get("Content-Security-Policy") {
		t.Fatal("in-app console changed shell security headers")
	}
	if !strings.Contains(console.Body.String(), `rel="manifest" href="/tgw/webui/manifest.webmanifest"`) || !strings.Contains(console.Body.String(), `href="/tgw/webui/"`) {
		t.Fatal("console lost installed app identity or return link")
	}
	for _, c := range console.Result().Cookies() {
		if c.Name == adminCookie {
			t.Fatal("console navigation changed the existing login")
		}
	}
	if request(http.MethodGet, "/tgw/api/v1/admin/session", token).Code != http.StatusOK || request(http.MethodGet, "/tgw/api/v1/admin/session", "").Code != http.StatusUnauthorized {
		t.Fatal("in-app console does not share the existing authenticated API")
	}
	if request(http.MethodPost, consolePath, token).Code != http.StatusMethodNotAllowed {
		t.Fatal("in-app shell accepted a mutation")
	}
	page := request(http.MethodGet, "/tgw/webui/", token)
	if !strings.Contains(page.Body.String(), `href="/tgw/webui/admin/"`) || !strings.Contains(page.Body.String(), `href="/tgw/webui/admin/?next=/tgw/webui/"`) {
		t.Fatal("console or sign-in management link still leaves the app scope")
	}
}
