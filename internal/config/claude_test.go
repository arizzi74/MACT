package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeWorkerConfigUsesDedicatedRuntimeAndPrivateSettings(t *testing.T) {
	dir := t.TempDir()
	writeSecret(t, filepath.Join(dir, "worker.token"), "private-token")
	cfg := WorkerConfig{
		AgentKind: "claude", WorkerID: "00000000-0000-4000-8000-000000000002", Name: "Claude",
		StateFile: "state/worker.db", GatewayURL: "wss://gateway.example.test", TokenFile: "worker.token",
		AllowedWorkspaceRoots: []string{dir},
		Runtimes:              []RuntimeProfile{{ID: "primary", ClaudeBinary: "bin/claude", WorkingDirectory: dir, Autostart: true}},
	}
	path := filepath.Join(dir, "worker.json")
	save := func() {
		t.Helper()
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		writeSecret(t, path, string(data))
	}
	save()
	loaded, err := LoadWorker(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "state", "runtime-settings.json")
	if loaded.AgentKind != "claude" || loaded.Runtimes[0].ClaudeBinary != filepath.Join(dir, "bin", "claude") || loaded.Runtimes[0].ClaudeSettingsFile != settings {
		t.Fatalf("Claude runtime was not normalized: %#v", loaded)
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	writeSecret(t, settings, `{"auth_mode":"login"}`)
	if err := os.Chmod(settings, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorker(path); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("public settings accepted: %v", err)
	}
	if err := os.Chmod(settings, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorker(path); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"codex", "unsupported"} {
		cfg.AgentKind = kind
		save()
		if _, err := LoadWorker(path); err == nil {
			t.Fatalf("accepted provider %q with Claude runtime", kind)
		}
	}
	cfg.AgentKind = "claude"
	cfg.Runtimes = append(cfg.Runtimes, cfg.Runtimes[0])
	save()
	if _, err := LoadWorker(path); err == nil {
		t.Fatal("accepted multiple Claude runtimes")
	}
}
