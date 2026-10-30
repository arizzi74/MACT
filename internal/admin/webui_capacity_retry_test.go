package admin

import (
	"testing"

	"github.com/google/uuid"
)

func TestCapacityRetryEndpointRequiresAuthenticationOriginCSRFAndScopedInput(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"session_id":"` + uuid.NewString() + `","error_revision":"` + uuid.NewString() + `","action":"schedule"}`
	for _, tc := range []struct {
		name, token, origin, header, cookie, body string
		status                                    int
	}{
		{"no auth", "", passkeyTestOrigin, "csrf", "csrf", body, 401},
		{"no origin", token, "", "csrf", "csrf", body, 403},
		{"wrong origin", token, "https://other.example", "csrf", "csrf", body, 403},
		{"no csrf", token, passkeyTestOrigin, "", "csrf", body, 403},
		{"mismatched csrf", token, passkeyTestOrigin, "different", "csrf", body, 403},
		{"arbitrary prompt", token, passkeyTestOrigin, "csrf", "csrf", body[:len(body)-1] + `,"text":"Other input"}`, 400},
		{"invalid action", token, passkeyTestOrigin, "csrf", "csrf", `{"session_id":"` + uuid.NewString() + `","error_revision":"r","action":"run"}`, 400},
		{"unknown session", token, passkeyTestOrigin, "csrf", "csrf", body, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/sessions/capacity-retry", tc.body, tc.token, tc.origin, tc.header, tc.cookie)
			if r.Code != tc.status {
				t.Fatalf("got %d want %d: %s", r.Code, tc.status, r.Body.String())
			}
		})
	}
	commands, err := store.PendingCommands(t.Context(), 10)
	if err != nil || len(commands) != 0 {
		t.Fatal("rejected request submitted a prompt", commands, err)
	}
}
