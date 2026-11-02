package admin

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebUIQuestionAnswersAuthAndValidation(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, query, token string
		want                       int
	}{
		{"unauthenticated", "GET", "?session_id=" + uuid.NewString(), "", 401},
		{"invalid login", "GET", "?session_id=" + uuid.NewString(), "bad", 401},
		{"missing", "GET", "", token, 400},
		{"invalid", "GET", "?session_id=invalid", token, 400},
		{"extra", "GET", "?session_id=" + uuid.NewString() + "&x=1", token, 400},
		{"duplicate", "GET", "?session_id=" + uuid.NewString() + "&session_id=" + uuid.NewString(), token, 400},
		{"unknown session", "GET", "?session_id=" + uuid.NewString(), token, 404},
		{"mutation forbidden", "POST", "?session_id=" + uuid.NewString(), token, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/tgw/api/v1/webui/question-answers"+tc.query, nil)
			if tc.token != "" {
				r.AddCookie(cookie(adminCookie, tc.token, true))
			}
			w := httptest.NewRecorder()
			server.webuiQuestionAnswers(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("cacheable answers")
			}
		})
	}
}

func TestWebUIQuestionAnswerHistoryIsScopedAndRedacted(t *testing.T) {
	f := newWebUITelegramBotFixture(t)
	token := webuiTestLogin(t, f.store)
	redactor, err := auth.NewRedactor([]string{"configured-private-value"}, "")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(f.store, Config{Origin: passkeyTestOrigin, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "request", ThreadID: "selector-thread-0", TurnID: "turn", ItemID: "question", Type: "user_input", State: "pending", Questions: []protocol.Question{{ID: "first", Prompt: "Choose a configured-private-value?"}, {ID: "secret", Prompt: "Private?", Secret: true}}}
	f.emit(f.sessions[0], "user_input_requested", approval)
	approval.Answers = map[string][]string{"first": {"configured-private-value answer"}, "secret": {"secret-text"}}
	f.emit(f.sessions[0], "user_input_answered", approval)
	approval.State = "approved"
	f.emit(f.sessions[0], "approval_resolved", approval)
	for i, session := range f.sessions {
		r := httptest.NewRequest("GET", "/tgw/api/v1/webui/question-answers?session_id="+session.String(), nil)
		r.AddCookie(cookie(adminCookie, token, true))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("history status %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, "configured-private-value") || strings.Contains(body, "secret-text") {
			t.Fatal("unredacted history", body)
		}
		var out struct {
			Answers []registry.WebUIQuestionAnswer `json:"answers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if len(out.Answers) != 1 || len(out.Answers[0].Fields) != 2 || out.Answers[0].Fields[1].Answer != "[REDACTED]" {
				t.Fatalf("missing answer %+v", out)
			}
		} else if len(out.Answers) != 0 {
			t.Fatal("answer crossed sessions", body)
		}
	}
}
