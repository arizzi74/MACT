package releasemanager

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestClaudeFreshEnrollmentAllowsDefaultProjectFolderWithoutOutsideAccess(t *testing.T) {
	m, previous, cwd := setupFixture(t)
	l, err := newLayout("claude-worker", "linux", "arm64", previous.Home)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(filepath.Dir(os.Getenv("PATH")), "bin", "claude")
	if err := AtomicWrite(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700, nil); err != nil {
		t.Fatal(err)
	}
	oldRun := m.Run
	m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{binary, "--version"}) {
			return CommandResult{Output: []byte("2.1.310 (Claude Code)\n")}, nil
		}
		return oldRun(ctx, args...)
	}
	url := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
		writeSetupEnrollmentResponse(w, r, workerServiceRestricted)
	})
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"Claude worker", url, "yes"}}
	installed := false
	err = m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		installed = true
		if opts.Action != "install" || opts.Component != "claude-worker" {
			t.Fatalf("wrong provider installation: %+v", opts)
		}
		cfg, err := config.LoadWorker(opts.Config)
		if err != nil {
			t.Fatal(err)
		}
		home, err := filepath.EvalSymlinks(l.Home)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AgentKind != "claude" || !reflect.DeepEqual(cfg.AllowedWorkspaceRoots, []string{home}) || len(cfg.Runtimes) != 1 || cfg.Runtimes[0].WorkingDirectory != home || cfg.Runtimes[0].ClaudeBinary != binary || cfg.Runtimes[0].CodexBinary != "" {
			t.Fatalf("Claude defaults changed provider or root boundary: %+v", cfg)
		}
		project := filepath.Join(home, "CLAUDE_CODE", "My_project")
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := auth.CanonicalWorkspace(project, cfg.AllowedWorkspaceRoots); err != nil {
			t.Fatal("default Claude project path was rejected", err)
		}
		if _, err := auth.CanonicalWorkspace(cwd, cfg.AllowedWorkspaceRoots); err == nil {
			t.Fatal("enrollment widened workspace access outside home")
		}
		return nil
	})
	if err != nil || !installed {
		t.Fatal("fresh Claude enrollment failed", err)
	}
}

func TestClaudeSetupPreservesExplicitWorkspaceRootsAndRuntimeDirectory(t *testing.T) {
	for _, source := range []string{"prepared", "installed"} {
		for _, folder := range []string{"CODEX", "private-projects"} {
			t.Run(source+" "+folder, func(t *testing.T) {
				m, previous, cwd := setupFixture(t)
				l, err := newLayout("claude-worker", "linux", "arm64", previous.Home)
				if err != nil {
					t.Fatal(err)
				}
				root := filepath.Join(l.Home, folder)
				project := filepath.Join(root, "existing_project")
				if err := os.MkdirAll(project, 0o700); err != nil {
					t.Fatal(err)
				}
				token := filepath.Join(cwd, "worker.token")
				if err := os.WriteFile(token, []byte("private-worker-token"), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg := config.WorkerConfig{AgentKind: "claude", WorkerID: "00000000-0000-4000-8000-000000000002", Name: "Existing Claude worker", StateFile: filepath.Join(l.Home, "state", "worker.db"), GatewayURL: "wss://gateway.example/tgw/api/v1/workers/connect", TokenFile: token, AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "primary", Name: "Existing Claude", ClaudeBinary: "claude", WorkingDirectory: project, Autostart: true}}}
				path := l.Config
				if source == "prepared" {
					path = filepath.Join(cwd, "claude-worker.json")
				}
				if err := WriteJSON(path, cfg); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				called := false
				err = m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) {
					t.Fatal("existing explicit config unexpectedly reopened enrollment")
					return nil, nil
				}, func(_ context.Context, opts options) error {
					called = true
					want := "adopt"
					if source == "prepared" {
						want = "install"
					}
					if opts.Action != want || opts.Component != "claude-worker" {
						t.Fatalf("unexpected existing-config action: %+v", opts)
					}
					actual, err := config.LoadWorker(path)
					if err != nil {
						t.Fatal(err)
					}
					canonicalRoot, _ := filepath.EvalSymlinks(root)
					canonicalProject, _ := filepath.EvalSymlinks(project)
					if !reflect.DeepEqual(actual.AllowedWorkspaceRoots, []string{canonicalRoot}) || actual.Runtimes[0].WorkingDirectory != canonicalProject {
						t.Fatal("setup rewrote explicit custom roots or project directory")
					}
					return nil
				})
				after, readErr := os.ReadFile(path)
				if err != nil || readErr != nil || !called || !bytes.Equal(before, after) {
					t.Fatal("existing Claude config changed", err, readErr)
				}
				if _, err := os.Stat(filepath.Join(l.Home, "CLAUDE_CODE")); !os.IsNotExist(err) {
					t.Fatal("setup implicitly created or allowed an extra project root")
				}
			})
		}
	}
}
