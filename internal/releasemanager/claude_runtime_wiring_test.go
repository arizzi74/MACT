package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
)

type claudeWiringFixture struct {
	m                            *Manager
	l                            *Layout
	now                          time.Time
	channel, latest, running     string
	checks, prepares             int
	projectFailure, checkFailure bool
}

func newClaudeWiringFixture(t *testing.T) *claudeWiringFixture {
	t.Helper()
	m, binary, _, _ := claudeDistributionFixture(t)
	l, err := NewLayout("claude-worker")
	if err != nil {
		t.Fatal(err)
	}
	f := &claudeWiringFixture{m: m, l: l, now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), channel: "latest", latest: "2.1.310", running: "2.1.310"}
	m.Now = func() time.Time { return f.now }
	// This fixture tests maintenance dispatch. Signed bundle verification has
	// separate fail-closed coverage in releaseauth and release manager tests.
	m.verifyManifest = func(string, string, []byte, []byte) error { return nil }
	prefix := "https://github.com/" + DefaultRepo + "/releases/download/v1.2.3/"
	workerAsset := "claude-worker-" + l.System + "-" + l.Architecture + ".tar.gz"
	if err := AtomicWrite(l.Binary, []byte("Claude worker"), 0755, nil); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(l.Manager, []byte("manager"), 0755, nil); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(l, DefaultRepo, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(l.Config, map[string]any{"worker_id": "example-worker", "agent_kind": "claude", "runtimes": []any{map[string]any{"id": "main", "claude_binary": binary, "autostart": true}}}); err != nil {
		t.Fatal(err)
	}
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case claudeLatestURL, claudeReleaseBaseURL + "/stable":
			if r.URL.String() != claudeReleaseBaseURL+"/"+f.channel {
				t.Fatalf("wrong configured Claude channel: %s", r.URL)
			}
			f.checks++
			unlock, err := Lock(l.Lock)
			if err == nil {
				unlock()
				t.Fatal("Claude discovery ran outside its worker lock")
			}
			if f.checkFailure {
				return nil, errors.New("private transport failure")
			}
			return coreResponse(r, []byte(f.latest)), nil
		case "https://api.github.com/repos/" + DefaultRepo + "/releases/latest":
			if f.projectFailure {
				response := coreResponse(r, []byte("private release failure"))
				response.StatusCode = http.StatusForbidden
				return response, nil
			}
			assets := make([]map[string]string, 0, 3)
			for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sigstore.json", workerAsset} {
				assets = append(assets, map[string]string{"name": name, "browser_download_url": prefix + name})
			}
			data, _ := json.Marshal(map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": false, "assets": assets})
			return coreResponse(r, data), nil
		case prefix + "SHA256SUMS":
			return coreResponse(r, []byte(coreHash(nil)+"  "+workerAsset+"\n")), nil
		case prefix + "SHA256SUMS.sigstore.json":
			return coreResponse(r, repositoryBundle(t, DefaultRepo, "v1.2.3")), nil
		default:
			t.Fatalf("Claude maintenance used another runtime or unexpected download: %s", r.URL)
			return nil, errors.New("unexpected request")
		}
	})
	m.CodexRun = func(context.Context, ...string) (CommandResult, error) {
		t.Fatal("Claude maintenance invoked Codex")
		return CommandResult{}, nil
	}
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		switch {
		case reflect.DeepEqual(args, []string{binary, "--version"}):
			return CommandResult{Output: []byte("2.1.310 (Claude Code)\n")}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "version"}):
			return CommandResult{Output: []byte("1.2.3\n")}, nil
		case reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "claude-worker.service", "-p", "MainPID", "-p", "ActiveState"}):
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		case len(args) == 3 && args[0] == "launchctl" && args[1] == "print":
			return CommandResult{Output: []byte("pid = 123\n")}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}):
			data, _ := json.Marshal(map[string]any{"worker_id": "example-worker", "pid": 123, "updated_at": f.now, "runtimes": []any{map[string]any{"profile_id": "main", "codex_version": f.running, "state": "running"}}})
			return CommandResult{Output: data}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "update", "prepare"}):
			f.prepares++
			return CommandResult{ExitCode: 75, Output: []byte(`{"error":"worker update: a session has an active turn"}`)}, nil
		default:
			t.Fatalf("Claude maintenance invoked another runtime or mutation: %v", args)
			return CommandResult{}, nil
		}
	}
	return f
}

func TestClaudeRuntimeCLIUsesSeparateInstallation(t *testing.T) {
	for _, args := range [][]string{{"update", "claude"}, {"update", "claude", "--check"}} {
		opts, err := parseOptions(args)
		if err != nil || opts.Component != "claude" {
			t.Fatalf("parse %v: %+v %v", args, opts, err)
		}
	}
	for _, args := range [][]string{{"install", "claude", "--config", "/example.json"}, {"adopt", "claude"}, {"auto-update", "enable", "claude"}, {"update", "claude", "--version", "v2.1.310"}, {"update", "claude", "--repo", "example/repo"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("invalid runtime arguments accepted: %v", args)
		}
	}
	f := newClaudeWiringFixture(t)
	if err := f.m.execute(t.Context(), options{Action: "update", Component: "claude", Check: true}); err != nil {
		t.Fatal(err)
	}
	if f.checks != 1 || f.prepares != 0 || FileExists(claudeUpdateStatePath(f.l)) {
		t.Fatal("runtime check did not use only Claude installation")
	}
}

func TestClaudeWorkerUpdateChecksRuntimeWhenWorkerCurrent(t *testing.T) {
	f := newClaudeWiringFixture(t)
	for range 2 {
		if err := f.m.execute(t.Context(), options{Action: "update", Component: "claude-worker"}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := loadClaudeUpdateState(f.l)
	if err != nil || f.checks != 1 || f.prepares != 0 || state.LatestVersion != "2.1.310" {
		t.Fatalf("daily maintenance: state=%+v checks=%d prepares=%d err=%v", state, f.checks, f.prepares, err)
	}
}

func TestClaudeWorkerReleaseFailureStillChecksRuntimeAndRetriesPending(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.projectFailure, f.latest = true, "2.1.311"
	for tick := range 3 {
		err := f.m.execute(t.Context(), options{Action: "update", Component: "claude-worker"})
		var busy *BusyError
		if err == nil || errors.As(err, &busy) || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("release failure lost: %v", err)
		}
		if f.checks != 1 || f.prepares != tick+1 {
			t.Fatalf("tick %d checks=%d prepares=%d", tick, f.checks, f.prepares)
		}
		f.now = f.now.Add(time.Hour)
	}
	f.now = f.now.Add(24 * time.Hour)
	if err := f.m.execute(t.Context(), options{Action: "update", Component: "claude-worker"}); err == nil || f.checks != 2 || f.prepares != 4 {
		t.Fatalf("next UTC day did not refresh Claude discovery: checks=%d prepares=%d err=%v", f.checks, f.prepares, err)
	}
}

func TestClaudeWorkerCheckOnlyDoesNotChangeState(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.latest = "2.1.311"
	before, err := os.ReadFile(f.l.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.execute(t.Context(), options{Action: "update", Component: "claude-worker", Check: true}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(f.l.State)
	if err != nil || !bytes.Equal(before, after) || FileExists(claudeUpdateStatePath(f.l)) || f.checks != 1 || f.prepares != 0 {
		t.Fatal("check-only mutated state or requested installation")
	}
}

func TestRequestedClaudeFreshCheckPreservesDailyScheduleAndWorkerSuccess(t *testing.T) {
	f := newClaudeWiringFixture(t)
	daily := claudeUpdateState{Schema: 1, Channel: "latest", CheckedAt: f.now.Add(-time.Hour), LatestVersion: "2.1.309"}
	if err := WriteJSON(claudeUpdateStatePath(f.l), daily); err != nil {
		t.Fatal(err)
	}
	plan := &requestedWorkerPlan{release: &Release{Tag: "v1.2.3", Repo: DefaultRepo}, claudeCache: filepath.Join(t.TempDir(), "request.claude-check.json")}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.State != "up_to_date" || result.Codex != nil || result.Claude == nil || result.Claude.State != "up_to_date" || result.Claude.LatestVersion != "2.1.310" || f.checks != 1 {
		t.Fatalf("explicit runtime check missing: %+v %v", result, err)
	}
	if len(result.Claude.Profiles) != 1 || result.Claude.Profiles[0].InstalledVersion != "2.1.310" || result.Claude.Profiles[0].RunningVersion != "2.1.310" {
		t.Fatalf("versions missing: %+v", result.Claude)
	}
	after, err := loadClaudeUpdateState(f.l)
	if err != nil || !reflect.DeepEqual(after, daily) {
		t.Fatalf("explicit request changed daily schedule: %+v %v", after, err)
	}
}

func TestRequestedClaudeBusyRetriesReuseDurableDiscovery(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.latest = "2.1.311"
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "up_to_date", Version: "v1.2.3"}, claudeCache: filepath.Join(t.TempDir(), "request.claude-check.json")}
	for tick := range 3 {
		_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
		var busy *BusyError
		if !errors.As(err, &busy) || f.checks != 1 || f.prepares != tick+1 {
			t.Fatalf("retry %d checks=%d prepares=%d err=%v", tick, f.checks, f.prepares, err)
		}
	}
	plan = &requestedWorkerPlan{workerResult: plan.workerResult, claudeCache: plan.claudeCache}
	_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	var busy *BusyError
	if !errors.As(err, &busy) || f.checks != 1 || f.prepares != 4 {
		t.Fatalf("restart lost discovery: checks=%d prepares=%d err=%v", f.checks, f.prepares, err)
	}
	info, err := os.Stat(plan.claudeCache)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("runtime cache is not private: %v", err)
	}
}

func TestRequestedClaudeFailedDiscoveryPreservesWorkerOutcome(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.checkFailure = true
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "completed", Version: "v1.2.3"}}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.State != "completed" || result.Codex != nil || result.Claude == nil || result.Claude.State != "failed" || result.Claude.ErrorCode != "check_failed" || len(result.Claude.Profiles) != 1 || f.prepares != 0 {
		t.Fatalf("worker success lost: %+v %v", result, err)
	}
}

func TestRequestedClaudeHonorsConfiguredChannel(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.channel = "stable"
	if err := WriteJSON(filepath.Join(f.l.Home, ".claude", "settings.json"), map[string]any{"autoUpdatesChannel": "stable"}); err != nil {
		t.Fatal(err)
	}
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "up_to_date", Version: "v1.2.3"}}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.Claude == nil || result.Claude.State != "up_to_date" || result.Claude.Channel != "stable" || f.checks != 1 {
		t.Fatalf("explicit request ignored configured channel: %+v %v", result, err)
	}
}

func TestRequestedClaudeRefusesCachedReleaseAfterChannelChanges(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.latest = "2.1.311"
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "up_to_date", Version: "v1.2.3"}}
	_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	var busy *BusyError
	if !errors.As(err, &busy) || f.prepares != 1 {
		t.Fatalf("initial update was not deferred: %v", err)
	}
	if err := WriteJSON(filepath.Join(f.l.Home, ".claude", "settings.json"), map[string]any{"autoUpdatesChannel": "stable"}); err != nil {
		t.Fatal(err)
	}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.State != "up_to_date" || result.Claude == nil || result.Claude.State != "failed" || result.Claude.ErrorCode != "inspection_failed" || f.checks != 1 || f.prepares != 1 {
		t.Fatalf("cached release crossed the channel change: %+v %v", result, err)
	}
}

func TestRequestedClaudeExternalInstallationIsReportedWithoutUpdating(t *testing.T) {
	f := newClaudeWiringFixture(t)
	cfg, err := ReadJSON(f.l.Config)
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg["runtimes"].([]any)[0].(map[string]any)
	binary := profile["claude_binary"].(string)
	target, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	profile["claude_binary"] = target
	if err := WriteJSON(f.l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	run := f.m.Run
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{target, "--version"}) {
			return CommandResult{Output: []byte("2.1.310 (Claude Code)\n")}, nil
		}
		return run(ctx, args...)
	}
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "up_to_date", Version: "v1.2.3"}}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.Claude == nil || result.Claude.State != "unsupported" || len(result.Claude.Profiles) != 1 || result.Claude.Profiles[0].Support != "external" || result.Claude.Profiles[0].InstalledVersion != "2.1.310" || f.prepares != 0 {
		t.Fatalf("external installation was not preserved: %+v %v", result, err)
	}
}

func TestRequestedClaudeWaitsForFreshAutostartVersion(t *testing.T) {
	f := newClaudeWiringFixture(t)
	f.latest = "2.1.311"
	run := f.m.Run
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		result, err := run(ctx, args...)
		if reflect.DeepEqual(args, []string{f.l.Binary, "--config", f.l.Config, "status"}) && err == nil {
			var status map[string]any
			if err := json.Unmarshal(result.Output, &status); err != nil {
				t.Fatal(err)
			}
			status["runtimes"] = []any{}
			result.Output, _ = json.Marshal(status)
		}
		return result, err
	}
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "up_to_date", Version: "v1.2.3"}}
	_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	var busy *BusyError
	if !errors.As(err, &busy) || f.prepares != 0 || f.checks != 1 {
		t.Fatalf("missing autostart runtime version allowed maintenance: prepares=%d checks=%d err=%v", f.prepares, f.checks, err)
	}
}

func TestClaudeWorkerUpdateRecoversRuntimeBeforeWorkerReleaseDiscovery(t *testing.T) {
	for _, check := range []bool{false, true} {
		name := "recover"
		if check {
			name = "check"
		}
		t.Run(name, func(t *testing.T) {
			f := newClaudeWiringFixture(t)
			binary := filepath.Join(f.l.Home, ".local/bin/claude")
			d, err := f.m.inspectClaudeDistribution(t.Context(), binary)
			if err != nil {
				t.Fatal(err)
			}
			old, err := os.ReadFile(filepath.Join(d.VersionsDir, d.Version))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(claudeBackupRoot(f.l), 0700); err != nil {
				t.Fatal(err)
			}
			backup, err := os.MkdirTemp(claudeBackupRoot(f.l), "update-")
			if err != nil {
				t.Fatal(err)
			}
			if err := AtomicWrite(filepath.Join(backup, "claude"), old, 0700, nil); err != nil {
				t.Fatal(err)
			}
			config, err := os.ReadFile(f.l.Config)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := os.ReadFile(f.l.Binary)
			if err != nil {
				t.Fatal(err)
			}
			journal := &claudeRecovery{Schema: 1, Phase: "prepared", CreatedAt: f.now, Restart: true, OldPID: 123, WorkerVersion: "1.2.3", WorkerSHA256: coreHash(worker), ConfigSHA256: coreHash(config), Distribution: *d, OldTarget: d.LinkTarget, OldSHA256: coreHash(old), OldMode: 0700, BackupDir: backup, CandidateVersion: "2.1.311", CandidateSHA256: coreHash(old), Profiles: map[string]bool{"main": true}}
			if err := writeClaudeRecovery(f.l, journal); err != nil {
				t.Fatal(err)
			}
			f.m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				t.Fatalf("pending Claude recovery fetched release metadata: %s", r.URL)
				return nil, errors.New("unexpected release discovery")
			})
			err = f.m.execute(t.Context(), options{Action: "update", Component: "claude-worker", Check: check})
			var busy *BusyError
			if check && !errors.As(err, &busy) || !check && err != nil {
				t.Fatalf("recovery result: %v", err)
			}
			after, readErr := os.ReadFile(f.l.Binary)
			if readErr != nil || !bytes.Equal(worker, after) || f.checks != 0 || f.prepares != 0 {
				t.Fatalf("runtime recovery ran worker release maintenance: checks=%d prepares=%d err=%v", f.checks, f.prepares, readErr)
			}
			if FileExists(claudeRecoveryPath(f.l)) != check {
				t.Fatalf("recovery journal lifecycle disagrees with check=%t", check)
			}
		})
	}
}
