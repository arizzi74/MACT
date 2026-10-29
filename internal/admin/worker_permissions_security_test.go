package admin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestClaudeBypassRequiresFreshAuthenticatedSameOriginAdministrator(t *testing.T) {
	s, store, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	id := uuid.New()
	if _, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{ID: id, Name: "Claude fixture", OS: "linux", Arch: "arm64", TokenHash: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	manager := &runtimeSettingsFixture{}
	s.runtimeSettings = manager
	endpoint := "/tgw/api/v1/admin/workers/" + id.String() + "/runtime-settings"
	const payload = `{"model":"fixture","base_url":"https://provider.example","auth_mode":"login","permission_mode":"bypassPermissions"}`
	for _, tc := range []struct {
		token, origin, csrf string
		status              int
	}{
		{"", passkeyTestOrigin, "csrf", http.StatusUnauthorized},
		{token, "https://untrusted.example", "csrf", http.StatusForbidden},
		{token, passkeyTestOrigin, "wrong", http.StatusForbidden},
	} {
		response := webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, tc.token, tc.origin, tc.csrf, "csrf")
		if response.Code != tc.status || manager.calls != 0 {
			t.Fatal("unauthorized bypass update reached worker")
		}
	}
	response := webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, token, passkeyTestOrigin, "csrf", "csrf")
	if response.Code != http.StatusOK || manager.calls != 1 || manager.update.PermissionMode != "bypassPermissions" {
		t.Fatal("explicit authorized bypass no longer supported")
	}
	if strings.Contains(response.Body.String(), `"permission_mode":"bypassPermissions"`) {
		t.Fatal("default settings unexpectedly enabled bypass")
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	response = webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, token, passkeyTestOrigin, "csrf", "csrf")
	if response.Code != http.StatusForbidden || manager.calls != 1 {
		t.Fatal("stale authentication changed bypass mode")
	}
	if err := store.RevokeAdminSession(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	response = webUICommandRequestForTest(s, http.MethodPut, endpoint, payload, token, passkeyTestOrigin, "csrf", "csrf")
	if response.Code != http.StatusUnauthorized || manager.calls != 1 {
		t.Fatal("revoked authentication changed bypass mode")
	}
}
