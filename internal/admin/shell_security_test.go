package admin

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/httpguard"
)

func TestPublicShellRejectsMalformedCookieWithoutDatabaseLookup(t *testing.T) {
	// No store: any attempted optional database lookup would panic.
	s := &Server{shellReads: httpguard.NewLimiter(1, 1, time.Minute)}
	for _, value := range []string{"", "invalid", strings.Repeat("a", 10_000), strings.Repeat("!", 43)} {
		r := httptest.NewRequest(http.MethodGet, "/tgw/webui/", nil)
		r.AddCookie(cookie(adminCookie, value, true))
		w := httptest.NewRecorder()
		s.setRequestCSRF(w, r)
		if len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].Name != csrfCookie {
			t.Fatal("public shell did not issue its independent CSRF cookie")
		}
	}
}

func TestPublicShellBoundsUnauthenticatedCookieLookups(t *testing.T) {
	s := &Server{shellReads: httpguard.NewLimiter(1, 1, time.Minute)}
	if !s.shellReads.Allow("192.0.2.1", time.Now()) {
		t.Fatal("could not fill lookup budget")
	}
	r := httptest.NewRequest(http.MethodGet, "/tgw/webui/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.AddCookie(cookie(adminCookie, base64.RawURLEncoding.EncodeToString(make([]byte, 32)), true))
	w := httptest.NewRecorder()
	// A syntactically plausible attacker cookie still cannot force a query
	// after its budget is exhausted. A lookup on this nil store would panic.
	s.setRequestCSRF(w, r)
	if len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != 0 {
		t.Fatal("budget exhaustion did not safely serve the public shell")
	}
}
