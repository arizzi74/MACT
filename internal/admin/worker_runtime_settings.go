package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
)

type WorkerRuntimeSettingsManager interface {
	WorkerRuntimeSettings(context.Context, uuid.UUID, *protocol.WorkerRuntimeSettingsUpdate) (protocol.WorkerRuntimeSettings, error)
}

func (s *Server) workerRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		method(w)
		return
	}
	if r.Method == http.MethodPut {
		if _, ok := s.requireFreshAuth(w, r); !ok {
			return
		}
	} else if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || id == uuid.Nil || id.String() != r.PathValue("id") {
		bad(w)
		return
	}
	var update *protocol.WorkerRuntimeSettingsUpdate
	if r.Method == http.MethodPut {
		update = &protocol.WorkerRuntimeSettingsUpdate{}
		if !decode(w, r, update) {
			return
		}
		if update.Validate() != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_runtime_settings", "message": "Check the model, endpoint, authentication and permission settings."})
			return
		}
	}
	workers, err := s.store.ListWorkers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	found := false
	for _, worker := range workers {
		if worker.ID == id && worker.Enabled {
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if s.runtimeSettings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "runtime_settings_unavailable", "message": "Reconnect this worker to manage its Claude Code settings."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	settings, err := s.runtimeSettings.WorkerRuntimeSettings(ctx, id, update)
	if err != nil {
		code, message, status := "runtime_settings_unavailable", "Settings could not be confirmed. Reconnect the worker and reload its settings before trying again.", http.StatusServiceUnavailable
		switch {
		case errors.Is(err, gateway.ErrWorkerRuntimeSettingsUnsupported):
			code, message, status = "runtime_settings_unsupported", "Use a dedicated Claude Code worker with runtime settings support.", http.StatusConflict
		case errors.Is(err, gateway.ErrWorkerRuntimeSettingsBusy):
			code, message, status = "runtime_settings_busy", "The worker is busy. Wait for its current work to finish and try again.", http.StatusConflict
		case errors.Is(err, gateway.ErrWorkerRuntimeSettingsInvalid):
			code, message, status = "invalid_runtime_settings", "The worker rejected these settings. Check the credential and configuration, then try again.", http.StatusBadRequest
		}
		// Worker errors may contain credentials. Return only fixed local text.
		writeJSON(w, status, map[string]string{"code": code, "message": message})
		return
	}
	if settings.Validate() != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "runtime_settings_unavailable", "message": "The worker returned invalid runtime settings."})
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
