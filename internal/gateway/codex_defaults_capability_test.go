package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestCodexDefaultsCapabilityHandshakeHeartbeatAndSettingsRPC(t *testing.T) {
	store := testRegistry(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	token, _ := auth.GenerateWorkerToken()
	workerID := uuid.New()
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "Codex", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Minute)
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := protocol.Hello{WorkerID: workerID.String(), WorkerName: "Codex", AgentKind: "codex", OS: "linux", Arch: "arm64", ProtocolMin: 1, ProtocolMax: 1, SupportsCodexSessionDefaults: true}
	sendFrame(t, ctx, conn, "hello", hello)
	if envelope, err := readEnvelope(ctx, conn); err != nil || envelope.Type != "hello_ack" {
		t.Fatalf("Codex defaults hello rejected: %v %v", envelope, err)
	}
	assertCapability := func() {
		t.Helper()
		workers, err := store.ListWorkers(ctx)
		if err != nil || len(workers) != 1 {
			t.Fatalf("worker metadata missing: %v", err)
		}
		var metadata struct {
			Codex  bool `json:"supports_codex_session_defaults"`
			Legacy bool `json:"supports_worker_runtime_settings"`
		}
		if json.Unmarshal(workers[0].HeartbeatMetadata, &metadata) != nil || !metadata.Codex || metadata.Legacy {
			t.Fatalf("Codex defaults reused the legacy Claude capability: %s", workers[0].HeartbeatMetadata)
		}
		dashboard, err := store.AdminDashboardSnapshot(ctx)
		if err != nil || len(dashboard.Workers) != 1 || !dashboard.Workers[0].SupportsWorkerRuntimeSettings || dashboard.Workers[0].AgentKind != "codex" {
			t.Fatalf("Codex defaults absent from dashboard: %+v %v", dashboard, err)
		}
	}
	assertCapability()
	hub.mu.RLock()
	p := hub.peers[workerID.String()]
	capable := p != nil && p.supportsWorkerRuntimeSettings
	hub.mu.RUnlock()
	if !capable {
		t.Fatal("Codex defaults capability did not enable transient settings RPCs")
	}
	finished := make(chan error, 1)
	go func() {
		settings, err := hub.WorkerRuntimeSettings(ctx, workerID, nil)
		if err == nil && (settings.Provider != "codex" || settings.CodexDefaults == nil) {
			err = errors.New("worker returned incorrect Codex defaults")
		}
		finished <- err
	}()
	envelope, err := readEnvelope(ctx, conn)
	if err != nil || envelope.Type != "worker_runtime_settings_request" {
		t.Fatalf("Codex defaults request was not delivered: %v %v", envelope, err)
	}
	request, err := protocol.Payload[protocol.WorkerRuntimeSettingsRequest](envelope)
	if err != nil || request.Validate() != nil || request.Action != "get" {
		t.Fatalf("invalid Codex defaults request: %+v %v", request, err)
	}
	sendFrame(t, ctx, conn, "worker_runtime_settings_result", protocol.WorkerRuntimeSettingsResult{RequestID: request.RequestID,
		Settings: &protocol.WorkerRuntimeSettings{Provider: "codex", CodexDefaults: &protocol.CodexSessionDefaults{}}})
	if err := <-finished; err != nil {
		t.Fatalf("Codex defaults response failed: %v", err)
	}
	heartbeat, err := protocol.NewEnvelope("heartbeat", protocol.Heartbeat{WorkerID: workerID.String(), AgentKind: "codex", SupportsCodexSessionDefaults: true})
	if err != nil {
		t.Fatalf("Codex defaults heartbeat rejected: %v", err)
	}
	if err := hub.handle(ctx, p, heartbeat); err != nil {
		t.Fatalf("Codex defaults heartbeat rejected: %v", err)
	}
	assertCapability()
}

func TestLegacyWorkerPayloadDecoderIgnoresCodexDefaultsCapability(t *testing.T) {
	// Older gateways decode payloads with json.Unmarshal and only know the
	// Claude capability. They must keep it false when a Codex worker upgrades.
	type legacyCapabilities struct {
		SupportsWorkerRuntimeSettings bool `json:"supports_worker_runtime_settings,omitempty"`
	}
	for _, kind := range []string{"hello", "heartbeat"} {
		envelope, err := protocol.NewEnvelope(kind, map[string]any{"agent_kind": "codex", "supports_codex_session_defaults": true})
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := protocol.Payload[legacyCapabilities](envelope)
		if err != nil || legacy.SupportsWorkerRuntimeSettings {
			t.Fatalf("legacy %s decoder opted into Codex settings: %+v %v", kind, legacy, err)
		}
	}
}
