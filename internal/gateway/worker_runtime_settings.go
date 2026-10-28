package gateway

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

var (
	ErrWorkerRuntimeSettingsUnavailable = errors.New("worker runtime settings unavailable")
	ErrWorkerRuntimeSettingsUnsupported = errors.New("worker runtime settings unsupported")
	ErrWorkerRuntimeSettingsInvalid     = errors.New("worker runtime settings invalid")
	ErrWorkerRuntimeSettingsBusy        = errors.New("worker runtime settings request already pending")
)

type workerRuntimeSettingsPending struct {
	peer   *peer
	result chan protocol.WorkerRuntimeSettingsResult
}

// WorkerRuntimeSettings exchanges configuration with the current live worker.
// Requests (including credentials) never enter a registry table or outbox.
// An interrupted write is deliberately not retried: it may already be applied.
func (h *Hub) WorkerRuntimeSettings(ctx context.Context, workerID uuid.UUID, update *protocol.WorkerRuntimeSettingsUpdate) (protocol.WorkerRuntimeSettings, error) {
	request := protocol.WorkerRuntimeSettingsRequest{RequestID: uuid.NewString(), WorkerID: workerID.String(), Action: "get", Settings: update}
	if update != nil {
		request.Action = "put"
	}
	if err := request.Validate(); err != nil {
		return protocol.WorkerRuntimeSettings{}, err
	}
	h.mu.Lock()
	p := h.peers[request.WorkerID]
	if p == nil || p.ctx.Err() != nil {
		h.mu.Unlock()
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
	}
	if !p.supportsWorkerRuntimeSettings {
		h.mu.Unlock()
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnsupported
	}
	if len(h.runtimeSettings) >= 64 {
		h.mu.Unlock()
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsBusy
	}
	for _, pending := range h.runtimeSettings {
		if pending.peer == p {
			h.mu.Unlock()
			return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsBusy
		}
	}
	pending := workerRuntimeSettingsPending{peer: p, result: make(chan protocol.WorkerRuntimeSettingsResult, 1)}
	if h.runtimeSettings == nil {
		h.runtimeSettings = make(map[string]workerRuntimeSettingsPending)
	}
	h.runtimeSettings[request.RequestID] = pending
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.runtimeSettings, request.RequestID)
		h.mu.Unlock()
	}()
	if err := h.store.CheckConnection(ctx, p.workerID, p.connectionID); err != nil {
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
	}
	if err := p.send(ctx, "worker_runtime_settings_request", request); err != nil {
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
	}
	select {
	case result := <-pending.result:
		h.mu.RLock()
		current := h.peers[request.WorkerID] == p
		h.mu.RUnlock()
		if !current || h.store.CheckConnection(ctx, p.workerID, p.connectionID) != nil {
			return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
		}
		switch result.ErrorCode {
		case "":
			return *result.Settings, nil
		case "unsupported":
			return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnsupported
		case "invalid_settings":
			return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsInvalid
		case "busy":
			return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsBusy
		default:
			return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
		}
	case <-ctx.Done():
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
	case <-p.ctx.Done():
		return protocol.WorkerRuntimeSettings{}, ErrWorkerRuntimeSettingsUnavailable
	}
}

func (h *Hub) handleWorkerRuntimeSettings(p *peer, envelope protocol.Envelope) error {
	result, err := protocol.Payload[protocol.WorkerRuntimeSettingsResult](envelope)
	if err != nil || result.Validate() != nil {
		// Never return decoder errors: their text can quote credential input.
		return errors.New("invalid worker runtime settings result")
	}
	h.mu.RLock()
	pending, found := h.runtimeSettings[result.RequestID]
	current := h.peers[p.workerID.String()] == p
	h.mu.RUnlock()
	if !found || pending.peer != p || !current {
		return nil
	}
	select {
	case pending.result <- result:
	default:
	}
	return nil
}
