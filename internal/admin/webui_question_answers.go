package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Server) webuiQuestionAnswers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	query, err := urlParseQuestionQuery(r)
	if err != nil {
		bad(w)
		return
	}
	answers, err := s.store.WebUIQuestionAnswers(r.Context(), query)
	if errors.Is(err, registry.ErrEventTarget) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := json.Marshal(map[string]any{"answers": answers})
	if err == nil {
		raw, err = s.redactWebUI(raw)
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}
func urlParseQuestionQuery(r *http.Request) (uuid.UUID, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["session_id"]) != 1 {
		return uuid.Nil, registry.ErrEventTarget
	}
	id, err := uuid.Parse(query.Get("session_id"))
	if err != nil || id == uuid.Nil || id.String() != query.Get("session_id") {
		return uuid.Nil, registry.ErrEventTarget
	}
	return id, nil
}
