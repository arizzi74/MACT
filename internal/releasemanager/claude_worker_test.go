package releasemanager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

func TestClaudeWorkerCommandsUseSeparateInstallation(t *testing.T) {
	for _, system := range []string{"linux", "darwin"} {
		l, err := newLayout("claude-worker", system, "arm64", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if l.Component != "worker" || l.AgentKind != "claude" || filepath.Base(l.Binary) != "claude-worker" || !strings.Contains(l.Config, "/claude-worker/") || !strings.Contains(l.Lock, "/claude-worker/") {
			t.Fatalf("Claude shares a Codex installation: %#v", l)
		}
		var calls [][]string
		m := New(nil)
		m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
			calls = append(calls, append([]string{}, args...))
			return CommandResult{}, nil
		}
		if err := m.Service(t.Context(), l, "stop"); err != nil {
			t.Fatal(err)
		}
		if system == "linux" && !reflect.DeepEqual(calls[0], []string{"systemctl", "--user", "stop", "claude-worker.service"}) {
			t.Fatalf("wrong service: %v", calls)
		}
		if system == "darwin" && !strings.HasSuffix(calls[0][2], "/com.iaia.claude-worker") {
			t.Fatalf("wrong launch agent: %v", calls)
		}
		if err := m.AutoUpdate(t.Context(), l, true); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(l.UnitDir, "claude-worker-update.service")
		if system == "darwin" {
			path = filepath.Join(l.Home, "Library", "LaunchAgents", "com.iaia.claude-worker-update.plist")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "claude-worker") || strings.Contains(string(data), "codex-worker") {
			t.Fatalf("wrong maintenance target: %s", data)
		}
	}
	for _, args := range [][]string{{"install", "claude-worker", "--config", "/example.json", "--service-access", "restricted"}, {"adopt", "claude-worker"}, {"update", "claude-worker", "--check"}, {"auto-update", "enable", "claude-worker"}} {
		opts, err := parseOptions(args)
		if err != nil || opts.Component != "claude-worker" {
			t.Fatalf("command %v: %#v %v", args, opts, err)
		}
	}
}

func TestClaudeRequestedUpdateDoesNotRunCodexMaintenance(t *testing.T) {
	f := newClaudeWiringFixture(t)
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "up_to_date", Version: "v1.2.3"}}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.Codex != nil || plan.codexReport != nil || result.State != "up_to_date" || result.Claude == nil || result.Claude.State != "up_to_date" || f.checks != 1 {
		t.Fatalf("Claude-only maintenance missing or Codex touched: %#v %v", result, err)
	}
}

func TestClaudeSetupUsesExistingCLIWithoutCodexInstallOrLogin(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "claude")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	l, err := newLayout("claude-worker", "linux", "amd64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	var calls [][]string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		calls = append(calls, append([]string{}, args...))
		if !reflect.DeepEqual(args, []string{binary, "--version"}) {
			t.Fatalf("Claude setup called an installer or login: %v", args)
		}
		return CommandResult{Output: []byte("2.1.123 (Claude Code)\n")}, nil
	}
	got, err := m.setupWorkerClaude(t.Context(), l)
	if err != nil || got != binary || len(calls) != 1 {
		t.Fatalf("existing Claude CLI not reused: %q %v %v", got, calls, err)
	}
}

func TestClaudeUpdatePreservesCodexInstallationWithoutLocalHelperPackage(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	codexBinary := l.Binary
	l.AgentKind, l.Binary = "claude", filepath.Join(l.Bin, "claude-worker")
	platformWrite(t, l.Binary, "old Claude worker", 0755)
	platformWrite(t, filepath.Join(packages["worker"], "claude-worker"), "new Claude worker", 0755)
	codexHelper := filepath.Join(l.Bin, "codex-local")
	platformWrite(t, codexHelper, "existing Codex helper", 0755)
	delete(packages, "local")
	started := false
	m := &Manager{ReadyTimeout: time.Second, Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "systemctl" {
			if len(args) < 4 || args[3] != "claude-worker.service" {
				t.Fatalf("touched another service: %v", args)
			}
			if args[2] == "show" {
				return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
			}
			if args[2] == "start" {
				started = true
			}
			return CommandResult{}, nil
		}
		if args[0] == l.Binary && args[3] == "status" {
			if !started {
				return CommandResult{ExitCode: 1}, nil
			}
			data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": true, "version": "1.2.3", "runtimes": []any{}})
			return CommandResult{Output: data}, nil
		}
		t.Fatalf("unexpected maintenance command: %v", args)
		return CommandResult{}, nil
	}}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err != nil {
		t.Fatal(err)
	}
	if updateRead(t, l.Binary) != "new Claude worker" || updateRead(t, codexBinary) != "old binary" || updateRead(t, codexHelper) != "existing Codex helper" {
		t.Fatal("Claude update changed the Codex installation")
	}
	entries, err := os.ReadDir(l.Backups)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing previous Claude release backup: %v %v", entries, err)
	}
	root, err := os.OpenRoot(l.Backups)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	backup, err := inspectUpdateBackup(root, entries[0].Name(), "claude-worker")
	if err != nil || !backup.complete {
		t.Fatalf("Claude rollback backup not recognized: %#v %v", backup, err)
	}
}

func TestRequestedClaudeUpdateRejectsMismatchedStateBeforeServiceOrNetwork(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("worker administration requires a non-root account")
	}
	for _, mismatch := range []string{"state", "identity"} {
		t.Run(mismatch, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			l, err := NewLayout("claude-worker")
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(home, "worker.db")
			configuredState := state
			if mismatch == "state" {
				configuredState = filepath.Join(home, "different.db")
			}
			request := protocol.WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString()}
			configuredID := request.WorkerID
			if mismatch == "identity" {
				configuredID = uuid.NewString()
			}
			if err := WriteJSON(l.Config, map[string]any{"agent_kind": "claude", "worker_id": configuredID, "state_file": configuredState}); err != nil {
				t.Fatal(err)
			}
			if err := workerupdate.Create(state, request); err != nil {
				t.Fatal(err)
			}
			m := New(nil)
			m.Run = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("mismatched update invoked a command")
				return CommandResult{}, nil
			}
			if err := m.requestWorkerUpdate(t.Context(), []string{"claude-worker", "--request-id", request.RequestID, "--worker-state", state}); err != nil {
				t.Fatal(err)
			}
			result, err := workerupdate.Result(state, request.RequestID)
			if err != nil || result.State != "failed" || result.ErrorCode != "update_unavailable" || result.Codex != nil {
				t.Fatalf("mismatched request outcome: %#v %v", result, err)
			}
			if _, err := os.Stat(l.Lock); !os.IsNotExist(err) {
				t.Fatalf("mismatched queue entered installation transaction: %v", err)
			}
		})
	}
}
