package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestClaudeProfileStatusReportsVersionAndScopesNativeUpdater(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	root := a.cfg.AllowedWorkspaceRoots[0]
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "native"))
	t.Setenv("MACT_WORKER_CLAUDE_HELPER", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "0")
	logPath := filepath.Join(root, "launch-env.jsonl")
	t.Setenv("MACT_WORKER_CLAUDE_ENV_LOG", logPath)
	privatePaths := []string{filepath.Join(root, ".claude.json"), filepath.Join(root, ".claude", "settings.json")}
	privateBytes := []byte(`{"preserve":"credentials-and-settings"}`)
	for _, path := range privatePaths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, privateBytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	profile := a.cfg.Runtimes[0]
	runtime, client, err := a.manager.startProfile(t.Context(), profile)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a.manager.install(runtime, client)
	if err := a.writeStatus(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.cfg.StateFile + ".status.json")
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil || len(status.Runtimes) != 1 || status.Runtimes[0].ProfileID != profile.ID || status.Runtimes[0].CodexVersion != "9.8.7" {
		t.Fatalf("Claude startup version is absent from maintenance status: %+v %v", status.Runtimes, err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(runtime.Metadata, &metadata); err != nil || metadata["agent_kind"] != "claude" || metadata["agent_version"] != "9.8.7" {
		t.Fatalf("Claude metadata changed: %s %v", runtime.Metadata, err)
	}
	thread, err := client.StartThread(t.Context(), codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartTurn(t.Context(), thread.ID, "updater-scope"); err != nil {
		t.Fatal(err)
	}
	var launch struct {
		DisableAutoupdater string `json:"disable_autoupdater"`
	}
	waitFor(t, func() bool {
		data, err := os.ReadFile(logPath)
		return err == nil && json.Unmarshal(bytes.TrimSpace(data), &launch) == nil
	})
	if launch.DisableAutoupdater != "1" || os.Getenv("DISABLE_AUTOUPDATER") != "0" {
		t.Fatalf("native update override escaped the worker child: child=%q parent=%q", launch.DisableAutoupdater, os.Getenv("DISABLE_AUTOUPDATER"))
	}
	if !reflect.DeepEqual(profile, a.cfg.Runtimes[0]) {
		t.Fatal("starting the profile changed its configuration or identity")
	}
	for _, path := range privatePaths {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, privateBytes) {
			t.Fatalf("native private file changed: %v", err)
		}
	}
}

func TestClaudeUpdateLeaseChecksWorkOutsideLoadedRuntimeInventory(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := bridge.StartTurn(t.Context(), thread.ID, "block")
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := a.prepareUpdate(t.Context(), time.Minute); err == nil || !strings.Contains(err.Error(), "Claude runtime has an active turn") {
		if err == nil {
			_ = a.abortUpdate(lease.Token)
		}
		t.Fatalf("native work absent from shared loaded inventory allowed update: %v", err)
	}
	if a.update != nil || !a.manager.lifecycle.TryLock() {
		t.Fatal("failed native idle check retained admission fences")
	}
	a.manager.lifecycle.Unlock()
	if err := bridge.Interrupt(t.Context(), thread.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bridge.CheckUpdateIdle() == nil })
	lease, err := a.prepareUpdate(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.ReadThread(t.Context(), thread.ID, true); err != nil {
		t.Fatalf("maintenance observation lost native history: %v", err)
	}
}

func TestClaudeUpdateLeaseRejectsMissingOwnedBridge(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	a.manager.install(protocol.Runtime{ID: "runtime", ProfileID: "primary", State: "running"}, bridge.Client)
	a.manager.mu.Lock()
	delete(a.manager.claude, "primary")
	a.manager.mu.Unlock()
	if err := a.manager.verifyUpdateIdle(t.Context()); err == nil || !strings.Contains(err.Error(), "Claude runtime is unavailable") {
		t.Fatalf("missing authoritative bridge accepted: %v", err)
	}
}
