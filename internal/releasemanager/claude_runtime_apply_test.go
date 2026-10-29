package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Service operations and downloads are modeled at their actual boundaries;
// owned native ELF/Mach-O files still go through production identity checks.
type claudeApplyFixture struct {
	t                     *testing.T
	m                     *Manager
	l                     *Layout
	d                     *claudeDistribution
	now                   time.Time
	running               bool
	leaseActive           bool
	pid                   int
	busy                  bool
	stopError, startError bool
	startDespiteError     bool
	busyAfterStart        bool
	stopCalls, startCalls int
	prepares, aborts      int
	downloads, preflights int
	oldBytes, candidate   []byte
	statusFault           string
	onStop                func()
	profiles              map[string]bool
}

func newClaudeApplyFixture(t *testing.T) *claudeApplyFixture {
	t.Helper()
	m, binary, home, oldPath := claudeDistributionFixture(t)
	l, err := newLayout("claude-worker", "linux", "amd64", home)
	if err != nil {
		t.Fatal(err)
	}
	f := &claudeApplyFixture{t: t, m: m, l: l, now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC), running: true, pid: 431, profiles: map[string]bool{"main": true, "other": true, "manual": false}}
	m.Now = func() time.Time { return f.now }
	m.ReadyTimeout, m.PollInterval = 15*time.Millisecond, time.Millisecond
	if err := AtomicWrite(l.Binary, []byte("dedicated Claude worker fixture"), 0700, nil); err != nil {
		t.Fatal(err)
	}
	runtimes := []any{}
	for _, id := range []string{"main", "other", "manual"} {
		runtimes = append(runtimes, map[string]any{"id": id, "claude_binary": binary, "autostart": f.profiles[id]})
	}
	if err := WriteJSON(l.Config, map[string]any{"worker_id": "claude-worker-id", "agent_kind": "claude", "state_file": filepath.Join(home, "worker-state.db"), "runtimes": runtimes}); err != nil {
		t.Fatal(err)
	}
	f.oldBytes, err = os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	f.candidate = append(append([]byte(nil), f.oldBytes...), []byte("\nverified candidate 2.1.312\n")...)
	m.Run = f.run
	m.abortWorkerUpdate = func(context.Context, *Layout, string) error {
		f.aborts++
		f.leaseActive = false
		return nil
	}
	f.d, err = m.inspectClaudeDistribution(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"version": "2.1.312", "platforms": map[string]claudePlatformRelease{f.d.Platform: {Binary: "claude", Checksum: coreHash(f.candidate), Size: int64(len(f.candidate))}}})
	if err != nil {
		t.Fatal(err)
	}
	m.claudeVerifyManifest = func(input, signature []byte) error {
		if !bytes.Equal(input, manifest) || string(signature) != "fixture signature" {
			t.Fatal("staging did not verify the downloaded manifest/signature pair")
		}
		return nil
	}
	m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
		f.downloads++
		if f.leaseActive {
			t.Fatal("large candidate download held worker admission closed")
		}
		switch request.URL.String() {
		case claudeLatestURL:
			return claudeResponse(request, []byte("2.1.312")), nil
		case claudeReleaseBaseURL + "/2.1.312/manifest.json":
			return claudeResponse(request, manifest), nil
		case claudeReleaseBaseURL + "/2.1.312/manifest.json.sig":
			return claudeResponse(request, []byte("fixture signature")), nil
		case claudeReleaseBaseURL + "/2.1.312/" + f.d.Platform + "/claude":
			return claudeResponse(request, f.candidate), nil
		default:
			t.Fatalf("unexpected network request: %s", request.URL)
			return nil, errors.New("unexpected download")
		}
	})
	m.CodexRun = func(context.Context, ...string) (CommandResult, error) {
		t.Fatal("Claude maintenance invoked the Codex updater")
		return CommandResult{}, nil
	}
	m.claudePreflight = func(_ context.Context, candidate *claudeDistribution) error {
		f.preflights++
		if candidate.Version == "2.1.312" {
			if !strings.Contains(candidate.Binary, ".mact-stage-") {
				t.Fatal("candidate compatibility check ran after publication")
			}
			data, err := os.ReadFile(candidate.Binary)
			if err != nil || !bytes.Equal(data, f.candidate) {
				t.Fatalf("compatibility check saw an unverified candidate: %v", err)
			}
		}
		return nil
	}
	return f
}

func (f *claudeApplyFixture) selectedVersion() string {
	f.t.Helper()
	binary := filepath.Join(f.l.Home, ".local/bin/claude")
	link, err := os.Readlink(binary)
	if err != nil {
		f.t.Fatal(err)
	}
	return filepath.Base(link)
}

func (f *claudeApplyFixture) status() []byte {
	f.t.Helper()
	runtime := func(id string) map[string]any {
		return map[string]any{"profile_id": id, "codex_version": f.selectedVersion(), "state": "running", "pid": 0}
	}
	runtimes := []any{runtime("main"), runtime("other")}
	status := map[string]any{"worker_id": "claude-worker-id", "pid": f.pid, "updated_at": f.now, "gateway_connected": true, "runtimes": runtimes}
	switch f.statusFault {
	case "worker-pid":
		status["pid"] = f.pid + 1
	case "worker-id":
		status["worker_id"] = "another-worker"
	case "stale":
		status["updated_at"] = f.now.Add(-time.Minute)
	case "future":
		status["updated_at"] = f.now.Add(time.Minute)
	case "disconnected":
		status["gateway_connected"] = false
	case "missing-profile":
		status["runtimes"] = runtimes[:1]
	case "duplicate-profile":
		status["runtimes"] = append(runtimes, runtime("main"))
	case "unconfigured-profile":
		status["runtimes"] = []any{runtime("main"), runtime("foreign")}
	case "wrong-version":
		runtimes[1].(map[string]any)["codex_version"] = "2.1.1"
	case "degraded":
		runtimes[1].(map[string]any)["state"] = "degraded"
	case "manual-stopped":
		manual := runtime("manual")
		manual["state"], manual["codex_version"] = "stopped", ""
		status["runtimes"] = append(runtimes, manual)
	}
	data, err := json.Marshal(status)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *claudeApplyFixture) run(_ context.Context, args ...string) (CommandResult, error) {
	f.t.Helper()
	if slices.Contains(args, "codex-worker.service") || (len(args) > 0 && strings.HasPrefix(filepath.Base(args[0]), "codex-")) {
		f.t.Fatalf("Claude maintenance touched Codex: %v", args)
	}
	if len(args) == 2 && args[0] == filepath.Join(f.l.Home, ".local/bin/claude") && args[1] == "--version" {
		return CommandResult{Output: []byte(f.selectedVersion() + " (Claude Code)\n")}, nil
	}
	if slices.Equal(args, []string{f.l.Binary, "version"}) {
		return CommandResult{Output: []byte("1.2.3\n")}, nil
	}
	if len(args) >= 4 && slices.Equal(args[:3], []string{"systemctl", "--user", "show"}) && args[3] == "claude-worker.service" {
		if slices.Contains(args, "--value") {
			return CommandResult{Output: []byte(strconv.Itoa(f.pid))}, nil
		}
		if f.running {
			return CommandResult{Output: []byte(fmt.Sprintf("MainPID=%d\nActiveState=active\n", f.pid))}, nil
		}
		return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
	}
	if slices.Equal(args, []string{"systemctl", "--user", "stop", "claude-worker.service"}) {
		f.stopCalls++
		if !f.leaseActive {
			f.t.Fatal("worker stopped without a current live idle reservation")
		}
		if f.stopError {
			return CommandResult{}, errors.New("fixture stop refused")
		}
		f.running, f.leaseActive = false, false
		if f.onStop != nil {
			f.onStop()
		}
		return CommandResult{}, nil
	}
	if slices.Equal(args, []string{"systemctl", "--user", "start", "claude-worker.service"}) {
		f.startCalls++
		if f.startError && f.startCalls == 1 {
			if f.startDespiteError {
				f.running, f.pid = true, 900
			}
			return CommandResult{}, errors.New("fixture start failed")
		}
		f.running, f.pid = true, 900+f.startCalls
		return CommandResult{}, nil
	}
	if slices.Equal(args, []string{f.l.Binary, "--config", f.l.Config, "status"}) {
		if !f.running {
			return CommandResult{ExitCode: 1}, nil
		}
		return CommandResult{Output: f.status()}, nil
	}
	if len(args) >= 5 && slices.Equal(args[:4], []string{f.l.Binary, "--config", f.l.Config, "update"}) {
		switch args[4] {
		case "prepare":
			f.prepares++
			if f.busy || (f.busyAfterStart && f.pid >= 900) {
				return CommandResult{ExitCode: 75, Output: []byte(`{"error":"worker update: session is busy"}`)}, nil
			}
			f.leaseActive = true
			data, _ := json.Marshal(workerLease{WorkerID: "claude-worker-id", PID: f.pid, Token: fmt.Sprintf("lease-%d", f.prepares), ExpiresAt: f.now.Add(2 * time.Minute)})
			return CommandResult{Output: data}, nil
		case "abort":
			f.aborts++
			f.leaseActive = false
			return CommandResult{}, nil
		}
	}
	f.t.Fatalf("unexpected command: %v", args)
	return CommandResult{}, nil
}

func (f *claudeApplyFixture) plan() []claudeRuntimePlan {
	return []claudeRuntimePlan{{Distribution: f.d, TargetVersion: "2.1.312", NeedsInstall: true, Profiles: f.profiles}}
}

func (f *claudeApplyFixture) recordRestart() error {
	return WriteJSON(claudeUpdateStatePath(f.l), claudeUpdateState{Schema: 1, Channel: "latest", RestartPending: true})
}

func (f *claudeApplyFixture) journal(phase string, restart bool) *claudeRecovery {
	f.t.Helper()
	if err := os.MkdirAll(claudeBackupRoot(f.l), 0700); err != nil {
		f.t.Fatal(err)
	}
	backup, err := os.MkdirTemp(claudeBackupRoot(f.l), "update-")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := AtomicWrite(filepath.Join(backup, "claude"), f.oldBytes, 0700, nil); err != nil {
		f.t.Fatal(err)
	}
	config, err := os.ReadFile(f.l.Config)
	if err != nil {
		f.t.Fatal(err)
	}
	worker, err := os.ReadFile(f.l.Binary)
	if err != nil {
		f.t.Fatal(err)
	}
	j := &claudeRecovery{Schema: 1, Phase: phase, CreatedAt: f.now, Restart: restart, OldPID: f.pid, WorkerVersion: "1.2.3", WorkerSHA256: coreHash(worker), ConfigSHA256: coreHash(config), Distribution: *f.d, OldTarget: f.d.LinkTarget, OldSHA256: coreHash(f.oldBytes), OldMode: 0755, BackupDir: backup, CandidateVersion: "2.1.312", CandidateSHA256: coreHash(f.candidate), Profiles: f.profiles}
	if err := writeClaudeRecovery(f.l, j); err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *claudeApplyFixture) publishCandidate() {
	f.t.Helper()
	path := filepath.Join(f.d.VersionsDir, "2.1.312")
	if err := AtomicWrite(path, f.candidate, 0700, nil); err != nil {
		f.t.Fatal(err)
	}
	if err := atomicCodexSymlink(path, f.d.Binary); err != nil {
		f.t.Fatal(err)
	}
}

func TestClaudeApplyBusyNeverDownloadsOrStopsWorker(t *testing.T) {
	f := newClaudeApplyFixture(t)
	f.busy = true
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	var busy *BusyError
	if !errors.As(err, &busy) || f.downloads != 0 || f.preflights != 0 || f.stopCalls != 0 || f.startCalls != 0 || !f.running || f.selectedVersion() != "2.1.310" {
		t.Fatalf("busy worker was disrupted: downloads=%d stop=%d start=%d running=%t err=%v", f.downloads, f.stopCalls, f.startCalls, f.running, err)
	}
	if FileExists(claudeRecoveryPath(f.l)) || FileExists(claudeUpdateStatePath(f.l)) {
		t.Fatal("busy probe wrote recovery state")
	}
}

func TestClaudeApplyFinalLeaseBusyRemovesUnjournaledBackup(t *testing.T) {
	f := newClaudeApplyFixture(t)
	originalRun := f.m.Run
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if slices.Equal(args, []string{f.l.Binary, "--config", f.l.Config, "update", "prepare"}) && f.prepares == 1 {
			// Staging and the private rollback copy are complete, but work
			// arrived before the final admission reservation was acquired.
			f.busy = true
		}
		return originalRun(ctx, args...)
	}
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	var busy *BusyError
	if !errors.As(err, &busy) || f.downloads != 3 || f.preflights != 1 || f.prepares != 2 || f.aborts != 1 || f.stopCalls != 0 || f.startCalls != 0 || !f.running || f.pid != 431 || f.selectedVersion() != "2.1.310" {
		t.Fatalf("final lease deferral disrupted worker: downloads=%d prepares=%d stop=%d start=%d err=%v", f.downloads, f.prepares, f.stopCalls, f.startCalls, err)
	}
	if FileExists(claudeRecoveryPath(f.l)) || FileExists(claudeUpdateStatePath(f.l)) {
		t.Fatal("final lease deferral wrote a maintenance journal or restart marker")
	}
	entries, err := os.ReadDir(claudeBackupRoot(f.l))
	if (err != nil && !errors.Is(err, os.ErrNotExist)) || len(entries) != 0 {
		t.Fatalf("busy retry leaked a private executable backup: entries=%d err=%v", len(entries), err)
	}
	versions, err := os.ReadDir(f.d.VersionsDir)
	if err != nil || len(versions) != 1 || versions[0].Name() != "2.1.310" {
		t.Fatalf("busy retry leaked its staged native candidate: %v %v", versions, err)
	}
}

func TestClaudeCopyExecutableHashMismatchRemovesPartialDestination(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	original := []byte("original native executable bytes")
	if err := os.WriteFile(source, original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := claudeCopyExecutable(source, destination, coreHash([]byte("different executable bytes")), 0700); err == nil {
		t.Fatal("copy accepted a mismatched executable hash")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed copy retained its partial destination: %v", err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("failed copy changed its source: %v", err)
	}
}

func TestClaudeApplySuccessStagesBeforeIdleCutoverAndPreservesPrivateFiles(t *testing.T) {
	f := newClaudeApplyFixture(t)
	private := map[string][]byte{}
	for _, relative := range []string{".claude.json", ".claude/settings.json", ".claude/projects/project/session.jsonl", "worker-sessions.db"} {
		path := filepath.Join(f.l.Home, relative)
		private[path] = []byte("private original " + relative)
		if strings.HasSuffix(relative, ".json") {
			private[path] = []byte(`{"preserve":"private original"}`)
		}
		if err := AtomicWrite(path, private[path], 0600, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(f.l.Config)
	if err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart); err != nil {
		t.Fatal(err)
	}
	if f.selectedVersion() != "2.1.312" || f.prepares != 2 || f.aborts != 1 || f.downloads != 3 || f.preflights != 1 || f.stopCalls != 1 || f.startCalls != 1 || !f.running {
		t.Fatalf("unexpected successful cutover: selected=%s prepares=%d aborts=%d downloads=%d preflights=%d stop=%d start=%d running=%t", f.selectedVersion(), f.prepares, f.aborts, f.downloads, f.preflights, f.stopCalls, f.startCalls, f.running)
	}
	after, _ := os.ReadFile(f.l.Config)
	if !bytes.Equal(before, after) || FileExists(claudeRecoveryPath(f.l)) {
		t.Fatal("cutover changed profile configuration or retained a completed journal")
	}
	for path, expected := range private {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, expected) {
			t.Fatalf("native private file changed: %v", err)
		}
	}
	entries, err := os.ReadDir(claudeBackupRoot(f.l))
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing retained rollback copy: %v", err)
	}
	backup, err := os.ReadFile(filepath.Join(claudeBackupRoot(f.l), entries[0].Name(), "claude"))
	if err != nil || !bytes.Equal(backup, f.oldBytes) {
		t.Fatalf("rollback bytes were not preserved: %v", err)
	}
}

func TestClaudeApplyRefusedStopLeavesOldWorkerRunningWithoutRestart(t *testing.T) {
	f := newClaudeApplyFixture(t)
	f.stopError = true
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	if err == nil || f.stopCalls != 1 || f.startCalls != 0 || !f.running || f.pid != 431 || f.selectedVersion() != "2.1.310" {
		t.Fatalf("refused stop restarted or replaced the old worker: stop=%d start=%d pid=%d err=%v", f.stopCalls, f.startCalls, f.pid, err)
	}
	if FileExists(claudeRecoveryPath(f.l)) {
		t.Fatal("unchanged original service left unnecessary recovery pending")
	}
}

func TestClaudeApplyIncompatibleCandidateIsRejectedWithoutStoppingWorker(t *testing.T) {
	f := newClaudeApplyFixture(t)
	f.m.claudePreflight = func(context.Context, *claudeDistribution) error {
		f.preflights++
		return errors.New("candidate control protocol is incompatible")
	}
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	state, stateErr := loadClaudeUpdateState(f.l)
	if err == nil || stateErr != nil || !slices.Contains(state.RejectedVersions, "2.1.312") || f.stopCalls != 0 || f.startCalls != 0 || f.prepares != 1 || f.aborts != 1 || !f.running || f.selectedVersion() != "2.1.310" || FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("incompatible candidate disrupted the worker or was not withheld: state=%+v err=%v stateErr=%v", state, err, stateErr)
	}
	entries, err := os.ReadDir(f.d.VersionsDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "2.1.310" {
		t.Fatalf("failed preflight published or leaked a candidate: %v, %v", entries, err)
	}
}

func TestClaudeApplyCanceledPreflightDoesNotRejectCandidate(t *testing.T) {
	f := newClaudeApplyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.m.claudePreflight = func(context.Context, *claudeDistribution) error {
		f.preflights++
		cancel()
		// The production probe can report a protocol read failure after its
		// child exits on cancellation. Parent cancellation still owns the result.
		return errors.New("candidate Claude runtime did not complete the isolated protocol compatibility check")
	}
	err := f.m.applyClaudeRuntimeUpdates(ctx, f.l, f.plan(), false, f.recordRestart)
	if !errors.Is(err, context.Canceled) || f.preflights != 1 || f.prepares != 1 || f.aborts != 1 || f.stopCalls != 0 || f.startCalls != 0 || !f.running || f.selectedVersion() != "2.1.310" {
		t.Fatalf("canceled preflight disrupted the worker or lost cancellation: prepares=%d aborts=%d stop=%d start=%d err=%v", f.prepares, f.aborts, f.stopCalls, f.startCalls, err)
	}
	if FileExists(claudeUpdateStatePath(f.l)) || FileExists(claudeRecoveryPath(f.l)) {
		t.Fatal("canceled preflight persisted a rejection or recovery state")
	}
	entries, err := os.ReadDir(f.d.VersionsDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "2.1.310" {
		t.Fatalf("canceled preflight published or leaked a candidate: %v, %v", entries, err)
	}
}

func TestClaudeApplyExternalLauncherChangeDuringCutoverIsLeftUntouched(t *testing.T) {
	f := newClaudeApplyFixture(t)
	foreign := filepath.Join(f.d.VersionsDir, "2.1.313")
	if err := AtomicWrite(foreign, f.oldBytes, 0700, nil); err != nil {
		t.Fatal(err)
	}
	f.onStop = func() {
		if err := atomicCodexSymlink(foreign, f.d.Binary); err != nil {
			t.Fatal(err)
		}
	}
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	if err == nil || f.selectedVersion() != "2.1.313" || f.stopCalls != 1 || f.startCalls != 0 || !FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("cutover overwrote an external update: selected=%s stop=%d start=%d err=%v", f.selectedVersion(), f.stopCalls, f.startCalls, err)
	}
}

func TestClaudeApplyFailedStartRestoresPriorBinaryAndWorker(t *testing.T) {
	f := newClaudeApplyFixture(t)
	f.startError = true
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	if err == nil || !f.running || f.stopCalls != 1 || f.startCalls != 2 || f.selectedVersion() != "2.1.310" || FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("failed startup did not restore the old runtime: stop=%d start=%d running=%t selected=%s err=%v", f.stopCalls, f.startCalls, f.running, f.selectedVersion(), err)
	}
	state, stateErr := loadClaudeUpdateState(f.l)
	if stateErr != nil || state.RestartPending || !slices.Contains(state.RejectedVersions, "2.1.312") {
		t.Fatalf("failed release was not durably withheld: %+v %v", state, stateErr)
	}
}

func TestClaudeApplyCandidateAcceptingWorkDefersRollbackWithoutStoppingIt(t *testing.T) {
	f := newClaudeApplyFixture(t)
	f.startError, f.startDespiteError, f.busyAfterStart = true, true, true
	err := f.m.applyClaudeRuntimeUpdates(t.Context(), f.l, f.plan(), false, f.recordRestart)
	if err == nil || !f.running || f.pid != 900 || f.stopCalls != 1 || f.startCalls != 1 || f.prepares != 3 || f.selectedVersion() != "2.1.312" || !FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("rollback interrupted newly accepted work: stop=%d start=%d pid=%d prepares=%d err=%v", f.stopCalls, f.startCalls, f.pid, f.prepares, err)
	}
	// A later recovery may proceed only after a new live reservation succeeds.
	f.busyAfterStart = false
	j, err := loadClaudeRecovery(f.l)
	if err != nil || j == nil {
		t.Fatalf("pending recovery was lost: %v", err)
	}
	if err := f.m.recoverClaudeRuntime(t.Context(), f.l, j); err != nil {
		t.Fatal(err)
	}
	if f.prepares != 4 || f.stopCalls != 2 || f.selectedVersion() != "2.1.310" || !f.running {
		t.Fatal("recovery did not require a fresh lease before stopping the candidate worker")
	}
}

// The installing checkpoint is still current if a symlink rename succeeded
// and its directory fsync or the next journal write failed. Recovery must use
// the observed link and retained binary, rather than assume nothing changed.
func TestClaudeRecoveryHandlesInterruptedPromotionAndPrunedNativeRelease(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprintf("pruned=%t", prune), func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			j := f.journal("installing", true)
			f.publishCandidate()
			f.running = false
			if prune {
				if err := os.Remove(j.oldPath()); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.m.recoverClaudeRuntime(t.Context(), f.l, j); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(j.oldPath())
			if err != nil || !bytes.Equal(data, f.oldBytes) || f.selectedVersion() != "2.1.310" || !f.running || f.startCalls != 1 || f.downloads != 0 || FileExists(claudeRecoveryPath(f.l)) {
				t.Fatalf("interrupted promotion failed restoration: selected=%s running=%t err=%v", f.selectedVersion(), f.running, err)
			}
		})
	}
}

func TestClaudeRecoveryLeavesExternalLauncherUntouched(t *testing.T) {
	f := newClaudeApplyFixture(t)
	j := f.journal("starting", true)
	foreign := filepath.Join(f.d.VersionsDir, "2.1.313")
	if err := AtomicWrite(foreign, []byte("external updater owns this version"), 0700, nil); err != nil {
		t.Fatal(err)
	}
	if err := atomicCodexSymlink(foreign, f.d.Binary); err != nil {
		t.Fatal(err)
	}
	err := f.m.recoverClaudeRuntime(t.Context(), f.l, j)
	if err == nil || f.selectedVersion() != "2.1.313" || f.stopCalls != 0 || f.startCalls != 0 || f.prepares != 0 || !FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("recovery overwrote external updater selection or stopped its worker: %v", err)
	}
}

func TestClaudeRecoveryPreparedJournalDoesNotStopReplacementWorker(t *testing.T) {
	f := newClaudeApplyFixture(t)
	j := f.journal("prepared", true)
	f.pid++
	err := f.m.recoverClaudeRuntime(t.Context(), f.l, j)
	var busy *BusyError
	if !errors.As(err, &busy) || f.stopCalls != 0 || f.startCalls != 0 || f.prepares != 0 || !FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("old prepared journal stopped a replacement worker: %v", err)
	}
}

func TestClaudeRecoveryLeaseExpiringAfterPIDValidationNeverStopsWorker(t *testing.T) {
	f := newClaudeApplyFixture(t)
	j := f.journal("starting", true)
	f.publishCandidate()
	f.pid = 900
	pidValidated, postPIDChecks := false, 0
	originalRun := f.m.Run
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		result, err := originalRun(ctx, args...)
		if slices.Equal(args, []string{"systemctl", "--user", "show", "claude-worker.service", "-p", "MainPID", "--value"}) {
			pidValidated = true
		}
		return result, err
	}
	f.m.Now = func() time.Time {
		if pidValidated {
			postPIDChecks++
			if postPIDChecks > 1 {
				// The reservation passed workerPrepare's last validation, then
				// expired while executable/context hashes were being checked.
				return f.now.Add(90 * time.Second)
			}
		}
		return f.now
	}
	err := f.m.recoverClaudeRuntime(t.Context(), f.l, j)
	var busy *BusyError
	if !errors.As(err, &busy) || f.prepares != 1 || f.aborts != 1 || f.stopCalls != 0 || f.startCalls != 0 || !f.running || f.selectedVersion() != "2.1.312" || !FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("expired recovery lease authorized shutdown: prepares=%d aborts=%d stop=%d err=%v", f.prepares, f.aborts, f.stopCalls, err)
	}
}

func TestClaudeRecoveryOnlyQuarantinesCandidateThatWasSelected(t *testing.T) {
	for _, phase := range []string{"prepared", "stopped", "installing"} {
		t.Run(phase, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			j := f.journal(phase, false)
			f.running = phase == "prepared"
			if phase == "installing" {
				// Publication alone is not activation or failed validation.
				if err := AtomicWrite(j.candidatePath(), f.candidate, 0700, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.m.recoverClaudeRuntime(t.Context(), f.l, j); err != nil {
				t.Fatal(err)
			}
			state, err := loadClaudeUpdateState(f.l)
			if err != nil || len(state.RejectedVersions) != 0 || f.selectedVersion() != "2.1.310" || f.stopCalls != 0 || f.startCalls != 0 {
				t.Fatalf("unselected candidate was quarantined or service changed: %+v %v", state, err)
			}
		})
	}
}

func TestClaudeRecoveryPreservesSelectedCandidateEvidenceWhenRejectionWriteFails(t *testing.T) {
	f := newClaudeApplyFixture(t)
	j := f.journal("starting", false)
	f.publishCandidate()
	f.running = false
	originalRun := f.m.Run
	failWrite := true
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if failWrite && slices.Equal(args, []string{f.l.Binary, "version"}) {
			// saveClaudeRejectedVersion already loaded the state. Occupying
			// its destination now makes the atomic persistence step fail.
			if err := os.Mkdir(claudeUpdateStatePath(f.l), 0700); err != nil {
				t.Fatal(err)
			}
		}
		return originalRun(ctx, args...)
	}
	err := f.m.recoverClaudeRuntime(t.Context(), f.l, j)
	if err == nil || f.selectedVersion() != "2.1.310" || !FileExists(claudeRecoveryPath(f.l)) {
		t.Fatalf("failed rejection persistence cleared recovery evidence: %v", err)
	}
	reloaded, err := loadClaudeRecovery(f.l)
	if err != nil || reloaded == nil || !reloaded.CandidateSelected || reloaded.Phase != "rollback" {
		t.Fatalf("selected candidate identity was not durable: %+v %v", reloaded, err)
	}
	if err := os.Remove(claudeUpdateStatePath(f.l)); err != nil {
		t.Fatal(err)
	}
	failWrite = false
	if err := f.m.recoverClaudeRuntime(t.Context(), f.l, reloaded); err != nil {
		t.Fatal(err)
	}
	state, err := loadClaudeUpdateState(f.l)
	if err != nil || !slices.Contains(state.RejectedVersions, "2.1.312") || FileExists(claudeRecoveryPath(f.l)) || f.stopCalls != 0 || f.startCalls != 0 {
		t.Fatalf("recovery forgot failed candidate after persistence resumed: %+v %v", state, err)
	}
}

func TestClaudeRecoveryRejectsChangedExecutableOrContextBeforeServiceMutation(t *testing.T) {
	for _, fault := range []string{"config", "worker", "private-backup", "old-release", "old-release-symlink", "public-backup", "config-ancestor-symlink", "state-ancestor-symlink"} {
		t.Run(fault, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			j := f.journal("stopped", true)
			f.publishCandidate()
			f.running = false
			switch fault {
			case "config", "worker", "private-backup", "old-release":
				path := map[string]string{"config": f.l.Config, "worker": f.l.Binary, "private-backup": filepath.Join(j.BackupDir, "claude"), "old-release": j.oldPath()}[fault]
				if err := os.WriteFile(path, []byte("changed independently"), 0700); err != nil {
					t.Fatal(err)
				}
			case "old-release-symlink":
				if err := os.Remove(j.oldPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(j.candidatePath(), j.oldPath()); err != nil {
					t.Fatal(err)
				}
			case "public-backup":
				if err := os.Chmod(j.BackupDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "config-ancestor-symlink", "state-ancestor-symlink":
				ancestor := filepath.Join(f.l.Home, ".config")
				if fault == "state-ancestor-symlink" {
					ancestor = filepath.Join(f.l.Home, ".local/state")
				}
				moved := filepath.Join(f.l.Home, "redirected-context")
				if err := os.Rename(ancestor, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, ancestor); err != nil {
					t.Fatal(err)
				}
			}
			err := f.m.recoverClaudeRuntime(t.Context(), f.l, j)
			if err == nil || f.stopCalls != 0 || f.startCalls != 0 || f.selectedVersion() != "2.1.312" || !FileExists(claudeRecoveryPath(f.l)) {
				t.Fatalf("changed context allowed destructive recovery: stop=%d start=%d selected=%s err=%v", f.stopCalls, f.startCalls, f.selectedVersion(), err)
			}
		})
	}
}

func TestClaudeRuntimeReadyVerifiesManagedFreshProfilesAndAllowsBridgePIDZero(t *testing.T) {
	for _, fault := range []string{"", "manual-stopped", "worker-pid", "worker-id", "stale", "future", "disconnected", "missing-profile", "duplicate-profile", "unconfigured-profile", "wrong-version", "degraded"} {
		t.Run(fault, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			f.statusFault = fault
			err := f.m.claudeRuntimeReady(t.Context(), f.l, f.now, f.profiles, "2.1.310")
			wantReady := fault == "" || fault == "manual-stopped"
			if (err == nil) != wantReady || f.stopCalls != 0 || f.startCalls != 0 {
				t.Fatalf("readiness fault %q: %v", fault, err)
			}
		})
	}
}

func TestClaudeRuntimeReadyDarwinUsesDedicatedManagedServicePID(t *testing.T) {
	for _, fault := range []string{"", "duplicate-pid", "malformed-pid", "unavailable"} {
		t.Run(fault, func(t *testing.T) {
			f := newClaudeApplyFixture(t)
			f.l.System = "darwin"
			originalRun := f.m.Run
			serviceChecks := 0
			expected := []string{"launchctl", "print", "gui/" + strconv.Itoa(os.Getuid()) + "/com.iaia.claude-worker"}
			f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if len(args) > 0 && args[0] == "launchctl" {
					serviceChecks++
					if !slices.Equal(args, expected) {
						t.Fatalf("Claude readiness inspected an unexpected service: %v", args)
					}
					switch fault {
					case "duplicate-pid":
						return CommandResult{Output: []byte("pid = 431\npid = 431\n")}, nil
					case "malformed-pid":
						return CommandResult{Output: []byte("pid = invalid\n")}, nil
					case "unavailable":
						return CommandResult{ExitCode: 113}, nil
					default:
						return CommandResult{Output: []byte("state = running\npid = 431\n")}, nil
					}
				}
				return originalRun(ctx, args...)
			}
			err := f.m.claudeRuntimeReady(t.Context(), f.l, f.now, f.profiles, "2.1.310")
			if (err == nil) != (fault == "") || serviceChecks == 0 || f.stopCalls != 0 || f.startCalls != 0 {
				t.Fatalf("Darwin managed PID fault %q: checks=%d err=%v", fault, serviceChecks, err)
			}
		})
	}
}

func TestClaudeCheckOnlyNeverStagesOrWritesState(t *testing.T) {
	f := newClaudeApplyFixture(t)
	before, _ := os.ReadFile(f.l.Config)
	if err := f.m.UpdateClaudeRuntime(t.Context(), f.l, true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.l.Config)
	if f.downloads != 1 || f.prepares != 0 || f.preflights != 0 || f.stopCalls != 0 || f.startCalls != 0 || f.selectedVersion() != "2.1.310" || !bytes.Equal(before, after) || FileExists(claudeUpdateStatePath(f.l)) || FileExists(claudeRecoveryPath(f.l)) || FileExists(claudeBackupRoot(f.l)) {
		t.Fatalf("check-only performed maintenance: downloads=%d prepares=%d stop=%d start=%d", f.downloads, f.prepares, f.stopCalls, f.startCalls)
	}
}
