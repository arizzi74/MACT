package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Server) webuiCapacityRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	var in struct {
		SessionID     string `json:"session_id"`
		ErrorRevision string `json:"error_revision"`
		Action        string `json:"action"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, err := uuid.Parse(in.SessionID)
	if err != nil || id == uuid.Nil || in.ErrorRevision == "" || len(in.ErrorRevision) > 256 || (in.Action != "schedule" && in.Action != "cancel") {
		bad(w)
		return
	}
	retry, err := s.store.SetCapacityRetry(r.Context(), id, in.ErrorRevision, in.Action, time.Now().UTC())
	if errors.Is(err, registry.ErrCapacityRetryUnavailable) {
		writeJSON(w, http.StatusConflict, map[string]string{"error_code": "capacity_retry_unavailable", "message": "This capacity retry is no longer available. The session may have received new work."})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retry": retry, "server_time_ms": time.Now().UTC().UnixMilli()})
}
