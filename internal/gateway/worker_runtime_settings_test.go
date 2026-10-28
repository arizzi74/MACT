package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func runtimeSettingsHub(t *testing.T) (*Hub, *peer, *registry.Store) {
	t.Helper()
	store := testRegistry(t)
	token, _ := auth.GenerateWorkerToken()
	id, connectionID := uuid.New(), uuid.New()
	if _, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{ID: id, Name: "Claude", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterConnection(t.Context(), token, connectionID, protocol.Hello{WorkerID: id.String(), WorkerName: "Claude", OS: "linux", Arch: "arm64", SupportsWorkerRuntimeSettings: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	p := &peer{workerID: id, connectionID: connectionID, supportsWorkerRuntimeSettings: true, ctx: ctx, cancel: cancel, writes: make(chan writeRequest, 8)}
	h := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Minute)
	h.peers[id.String()] = p
	return h, p, store
}

func receiveRuntimeSettingsRequest(t *testing.T, p *peer) protocol.WorkerRuntimeSettingsRequest {
	t.Helper()
	select {
	case write := <-p.writes:
		if write.envelope.Type != "worker_runtime_settings_request" {
			t.Fatal("runtime settings used a durable command envelope")
		}
		request, err := protocol.Payload[protocol.WorkerRuntimeSettingsRequest](write.envelope)
		if err != nil || request.Validate() != nil {
			t.Fatal("invalid runtime settings request")
		}
		write.finish(nil)
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("runtime settings request was not dispatched")
		return protocol.WorkerRuntimeSettingsRequest{}
	}
}

func runtimeSettingsResult(t *testing.T, requestID string) protocol.Envelope {
	t.Helper()
	envelope, err := protocol.NewEnvelope("worker_runtime_settings_result", protocol.WorkerRuntimeSettingsResult{RequestID: requestID,
		Settings: &protocol.WorkerRuntimeSettings{Provider: "claude", Model: "custom[1m]", BaseURL: "http://localhost:8080", AuthMode: "auth_token", PermissionMode: "default", APIKeyConfigured: true}})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestWorkerRuntimeSettingsTransientRPCAndPeerFencing(t *testing.T) {
	h, p, store := runtimeSettingsHub(t)
	key := "private-runtime-key"
	finished := make(chan error, 1)
	go func() {
		settings, err := h.WorkerRuntimeSettings(t.Context(), p.workerID, &protocol.WorkerRuntimeSettingsUpdate{Model: "custom[1m]", BaseURL: "http://localhost:8080", AuthMode: "auth_token", PermissionMode: "default", APIKey: &key})
		if err == nil && (!settings.APIKeyConfigured || settings.Provider != "claude") {
			err = errors.New("incorrect public runtime settings")
		}
		finished <- err
	}()
	request := receiveRuntimeSettingsRequest(t, p)
	if request.Action != "put" || request.Settings.APIKey == nil || *request.Settings.APIKey != key {
		t.Fatal("ephemeral RPC lost the credential")
	}
	if _, err := h.WorkerRuntimeSettings(t.Context(), p.workerID, nil); !errors.Is(err, ErrWorkerRuntimeSettingsBusy) {
		t.Fatal("concurrent configuration operation was not serialized")
	}
	foreign := &peer{workerID: uuid.New()}
	if err := h.handleWorkerRuntimeSettings(foreign, runtimeSettingsResult(t, request.RequestID)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		t.Fatal("foreign peer satisfied a worker settings request")
	default:
	}
	if err := h.handle(t.Context(), p, runtimeSettingsResult(t, request.RequestID)); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	dashboard, err := store.AdminDashboardSnapshot(t.Context())
	if err != nil || dashboard.QueuedCommands != 0 {
		t.Fatal("configuration entered the durable command queue")
	}
	watermark, err := store.EventWatermark(t.Context(), p.workerID)
	if err != nil || watermark != 0 || len(h.runtimeSettings) != 0 {
		t.Fatal("configuration entered the event journal or retained its request")
	}
}

func TestWorkerRuntimeSettingsCancellationAndRevocation(t *testing.T) {
	h, p, store := runtimeSettingsHub(t)
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { _, err := h.WorkerRuntimeSettings(ctx, p.workerID, nil); finished <- err }()
	request := receiveRuntimeSettingsRequest(t, p)
	cancel()
	if err := <-finished; !errors.Is(err, ErrWorkerRuntimeSettingsUnavailable) || len(h.runtimeSettings) != 0 {
		t.Fatal("cancelled configuration retained its request")
	}
	if err := h.handleWorkerRuntimeSettings(p, runtimeSettingsResult(t, request.RequestID)); err != nil {
		t.Fatal("late response disrupted the worker connection")
	}
	go func() { _, err := h.WorkerRuntimeSettings(t.Context(), p.workerID, nil); finished <- err }()
	request = receiveRuntimeSettingsRequest(t, p)
	if err := store.RevokeWorker(t.Context(), p.workerID); err != nil {
		t.Fatal(err)
	}
	if err := h.handleWorkerRuntimeSettings(p, runtimeSettingsResult(t, request.RequestID)); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, ErrWorkerRuntimeSettingsUnavailable) {
		t.Fatal("revoked worker supplied an accepted configuration response")
	}
}

func TestWorkerRuntimeSettingsMalformedErrorNeverQuotesInput(t *testing.T) {
	h, p, _ := runtimeSettingsHub(t)
	envelope, err := protocol.NewEnvelope("worker_runtime_settings_result", map[string]any{"request_id": uuid.NewString(), "error_code": "private-runtime-key"})
	if err != nil {
		t.Fatal(err)
	}
	err = h.handleWorkerRuntimeSettings(p, envelope)
	if err == nil || strings.Contains(err.Error(), "private-runtime-key") {
		t.Fatal("runtime settings error retained secret input")
	}
	p.supportsWorkerRuntimeSettings = false
	if _, err := h.WorkerRuntimeSettings(t.Context(), p.workerID, nil); !errors.Is(err, ErrWorkerRuntimeSettingsUnsupported) {
		t.Fatal("old worker received an unsupported settings frame")
	}
}
