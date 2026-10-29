package releasemanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The journal contains executable identities only. Claude settings, account
// credentials, project files and conversation stores are never copied or reset.
type claudeRecovery struct {
	Schema            int                `json:"schema"`
	Phase             string             `json:"phase"`
	CreatedAt         time.Time          `json:"created_at"`
	Restart           bool               `json:"restart"`
	OldPID            int                `json:"old_pid"`
	WorkerVersion     string             `json:"worker_version"`
	WorkerSHA256      string             `json:"worker_sha256"`
	ConfigSHA256      string             `json:"config_sha256"`
	Distribution      claudeDistribution `json:"distribution"`
	OldTarget         string             `json:"old_target"`
	OldSHA256         string             `json:"old_sha256"`
	OldMode           uint32             `json:"old_mode"`
	BackupDir         string             `json:"backup_dir"`
	CandidateVersion  string             `json:"candidate_version"`
	CandidateSHA256   string             `json:"candidate_sha256"`
	CandidateSelected bool               `json:"candidate_selected,omitempty"`
	CandidateCreated  bool               `json:"candidate_created,omitempty"`
	CandidateDevice   uint64             `json:"candidate_device,omitempty"`
	CandidateInode    uint64             `json:"candidate_inode,omitempty"`
	Profiles          map[string]bool    `json:"profiles"`
}

func claudeRecoveryPath(l *Layout) string {
	return filepath.Join(filepath.Dir(l.State), "claude-recovery.json")
}
func claudeBackupRoot(l *Layout) string {
	return filepath.Join(filepath.Dir(l.Lock), "claude-runtime-backups")
}
func (j *claudeRecovery) oldPath() string {
	return filepath.Join(j.Distribution.VersionsDir, j.Distribution.Version)
}
func (j *claudeRecovery) candidatePath() string {
	return filepath.Join(j.Distribution.VersionsDir, j.CandidateVersion)
}

func claudeFileHash(path string) (string, error) {
	if !claudeOwnedPath(path, false) {
		return "", errors.New("Claude executable ownership cannot be verified")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("cannot open Claude executable for verification")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > claudeBinaryLimit {
		return "", errors.New("invalid Claude executable for verification")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, claudeBinaryLimit+1))
	if err != nil || n != info.Size() {
		return "", errors.New("cannot hash Claude executable")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateClaudeRecovery(l *Layout, j *claudeRecovery) error {
	if j == nil || j.Schema != 1 || j.CreatedAt.IsZero() || len(j.Profiles) == 0 || len(j.Profiles) > 128 {
		return errors.New("invalid Claude recovery journal")
	}
	switch j.Phase {
	case "prepared", "stopped", "installing", "starting", "rollback", "committed":
	default:
		return errors.New("invalid Claude recovery phase")
	}
	d := j.Distribution
	if d.Home != l.Home || d.Binary != filepath.Join(l.Home, ".local/bin/claude") || d.InstallDir != filepath.Join(l.Home, ".local/bin") || d.VersionsDir != filepath.Join(l.Home, ".local/share/claude/versions") {
		return errors.New("Claude recovery installation identity is invalid")
	}
	for _, v := range []string{d.Version, j.CandidateVersion, j.WorkerVersion} {
		if _, err := ParseVersion(v); err != nil {
			return errors.New("invalid Claude recovery version")
		}
	}
	for _, h := range []string{j.OldSHA256, j.CandidateSHA256, j.ConfigSHA256, j.WorkerSHA256} {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
			return errors.New("invalid Claude recovery hash")
		}
	}
	for id := range j.Profiles {
		if id == "" || len(id) > 256 {
			return errors.New("invalid Claude recovery profile")
		}
	}
	if j.CandidateCreated && j.CandidateInode == 0 {
		return errors.New("Claude recovery candidate identity is invalid")
	}
	old := j.OldTarget
	if !filepath.IsAbs(old) {
		old = filepath.Join(filepath.Dir(d.Binary), old)
	}
	if filepath.Clean(old) != j.oldPath() || filepath.Dir(j.BackupDir) != claudeBackupRoot(l) || !strings.HasPrefix(filepath.Base(j.BackupDir), "update-") || j.OldMode&^uint32(0777) != 0 || j.OldMode&0002 != 0 {
		return errors.New("Claude recovery paths are invalid")
	}
	return nil
}

func loadClaudeRecovery(l *Layout) (*claudeRecovery, error) {
	p := claudeRecoveryPath(l)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !claudeOwnedPath(p, false) || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("cannot safely read Claude recovery journal")
	}
	data, err := os.ReadFile(p)
	var j claudeRecovery
	if err != nil || json.Unmarshal(data, &j) != nil {
		return nil, errors.New("cannot read Claude recovery journal")
	}
	if err := validateClaudeRecovery(l, &j); err != nil {
		return nil, err
	}
	return &j, nil
}
func writeClaudeRecovery(l *Layout, j *claudeRecovery) error {
	if err := validateClaudeRecovery(l, j); err != nil {
		return err
	}
	return WriteJSON(claudeRecoveryPath(l), j)
}
func clearClaudeRecovery(l *Layout) error {
	if err := os.Remove(claudeRecoveryPath(l)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return SyncDir(filepath.Dir(l.State))
}

func claudeCopyExecutable(source, target, expected string, mode os.FileMode) (retErr error) {
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("cannot open Claude executable backup source")
	}
	in := os.NewFile(uintptr(fd), source)
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("cannot create private Claude executable copy")
	}
	defer out.Close()
	defer func() {
		if retErr != nil {
			_ = os.Remove(target)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, claudeBinaryLimit+1))
	if err != nil || n > claudeBinaryLimit || hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("Claude executable copy failed verification")
	}
	if out.Sync() != nil || out.Close() != nil {
		return errors.New("cannot persist Claude executable copy")
	}
	return SyncDir(filepath.Dir(target))
}

// A promotion never overwrites a version file, even if another native updater
// has installed one concurrently. Reuse requires the signed candidate's hash.
func claudePublishExecutable(source, target, expected string) (bool, error) {
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return false, errors.New("Claude release destination is occupied")
		}
		h, err := claudeFileHash(target)
		if err != nil || h != expected {
			return false, errors.New("Claude release destination changed independently")
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.Link(source, target); err != nil {
		return false, errors.New("could not publish Claude executable without replacing an existing file")
	}
	return true, SyncDir(filepath.Dir(target))
}

func (m *Manager) currentClaudeDistribution(ctx context.Context, expected *claudeDistribution) (*claudeDistribution, error) {
	current, err := m.inspectClaudeDistribution(ctx, expected.Binary)
	if err != nil || *current != *expected {
		return nil, errors.New("Claude installation changed while preparing maintenance")
	}
	return current, nil
}

func (m *Manager) applyClaudeRuntimeUpdates(ctx context.Context, l *Layout, plans []claudeRuntimePlan, resumeStopped bool, recordRestart func() error) (retErr error) {
	if len(plans) != 1 {
		return errors.New("Claude maintenance requires one shared native installation")
	}
	p := plans[0]
	if _, err := ParseVersion(p.TargetVersion); err != nil {
		return errors.New("invalid Claude runtime target")
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return err
	}
	// Do not download a large candidate when authoritative native state already
	// says busy. Release this probe immediately; downloads never hold admission.
	if running {
		probe, err := m.workerPrepare(ctx, l)
		if err != nil {
			return err
		}
		if err := m.workerAbort(ctx, l, probe.Token); err != nil {
			return err
		}
	}
	current, err := m.currentClaudeDistribution(ctx, p.Distribution)
	if err != nil {
		return err
	}
	var staged *claudeStagedRuntime
	if p.NeedsInstall {
		staged, err = m.stageClaudeRuntime(ctx, current, p.TargetVersion)
		if err != nil {
			return err
		}
		defer staged.Cleanup()
		candidate := *current
		candidate.Binary, candidate.Version = staged.Binary, staged.Version
		if err := m.preflightClaudeRuntime(ctx, &candidate); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return m.rejectClaudeVersion(ctx, l, p.TargetVersion, err)
		}
	} else if err := m.preflightClaudeRuntime(ctx, current); err != nil {
		return err
	}
	if _, err = m.currentClaudeDistribution(ctx, current); err != nil {
		return err
	}
	oldPath := filepath.Join(current.VersionsDir, current.Version)
	oldHash, err := claudeFileHash(oldPath)
	if err != nil {
		return err
	}
	oldTarget, err := os.Readlink(current.Binary)
	if err != nil {
		return errors.New("Claude native launcher is unavailable")
	}
	oldInfo, err := os.Lstat(oldPath)
	if err != nil {
		return err
	}
	configHash, err := claudeFileHash(l.Config)
	if err != nil {
		return err
	}
	workerHash, err := claudeFileHash(l.Binary)
	if err != nil {
		return err
	}
	version, err := m.command(ctx, l.Binary, "version")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(claudeBackupRoot(l), 0700); err != nil {
		return err
	}
	if !claudeOwnedPath(claudeBackupRoot(l), true) {
		return errors.New("Claude backup directory ownership is invalid")
	}
	backup, err := os.MkdirTemp(claudeBackupRoot(l), "update-")
	if err != nil {
		return err
	}
	// A failed final idle check must not leave a large copy on every retry.
	// Preserve the copy whenever a journal may have reached disk, including a
	// successful rename followed by a directory-sync error.
	defer func() {
		if _, err := os.Lstat(claudeRecoveryPath(l)); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Lstat(filepath.Join(backup, "receipt.json")); errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(filepath.Join(backup, "claude"))
				_ = os.Remove(backup)
			}
		}
	}()
	if err := claudeCopyExecutable(oldPath, filepath.Join(backup, "claude"), oldHash, 0700); err != nil {
		return err
	}
	j := &claudeRecovery{Schema: 1, Phase: "prepared", CreatedAt: m.updateNow().UTC(), Restart: running || resumeStopped, WorkerVersion: strings.TrimSpace(string(version)), WorkerSHA256: workerHash, ConfigSHA256: configHash, Distribution: *current, OldTarget: oldTarget, OldSHA256: oldHash, OldMode: uint32(oldInfo.Mode().Perm()), BackupDir: backup, CandidateVersion: p.TargetVersion, CandidateSHA256: oldHash, Profiles: p.Profiles}
	if staged != nil {
		j.CandidateSHA256 = staged.SHA256
	}
	var lease *workerLease
	journalWritten := false
	defer func() {
		if retErr == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if lease != nil {
			// A stopped worker's reservation died with that process. Never send
			// its token to a replacement process while recovering a failed start.
			if pid, err := m.managedClaudeWorkerPID(cleanup, l); err == nil && pid == lease.PID {
				if err := m.workerAbort(cleanup, l, lease.Token); err != nil {
					retErr = errors.Join(retErr, errors.New("Claude worker reservation could not be released; it will expire automatically"))
				}
			}
		}
		if journalWritten {
			if err := m.recoverClaudeRuntime(cleanup, l, j); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	if running {
		lease, err = m.workerPrepare(ctx, l)
		if err != nil {
			return err
		}
		j.OldPID = lease.PID
	}
	if _, err = m.currentClaudeDistribution(ctx, current); err != nil {
		return err
	}
	if err = m.claudeContextUnchanged(l, j); err != nil {
		return err
	}
	if j.Restart {
		if recordRestart == nil {
			return errors.New("Claude maintenance cannot record restart recovery")
		}
		if err = recordRestart(); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if lease != nil && lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
		return &BusyError{Reason: "Claude update reservation expired; retry later"}
	}
	if err = writeClaudeRecovery(l, j); err != nil {
		return err
	}
	journalWritten = true
	if running {
		if lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
			return &BusyError{Reason: "Claude update reservation expired before shutdown"}
		}
		if err = m.Service(ctx, l, "stop"); err != nil {
			return err
		}
	}
	if live, err := m.workerRunning(ctx, l); err != nil {
		return err
	} else if live {
		return errors.New("Claude worker did not stop; installation retained")
	}
	j.Phase = "stopped"
	if err = writeClaudeRecovery(l, j); err != nil {
		return err
	}
	if err = m.claudeContextUnchanged(l, j); err != nil {
		return err
	}
	if _, err = m.currentClaudeDistribution(ctx, current); err != nil {
		return err
	}
	j.Phase = "installing"
	if err = writeClaudeRecovery(l, j); err != nil {
		return err
	}
	if staged != nil {
		if h, err := claudeFileHash(staged.Binary); err != nil || h != j.CandidateSHA256 {
			return errors.New("staged Claude executable changed")
		}
		created, publishErr := claudePublishExecutable(staged.Binary, j.candidatePath(), j.CandidateSHA256)
		if publishErr != nil {
			return publishErr
		}
		info, statErr := os.Lstat(j.candidatePath())
		if statErr != nil {
			return statErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("Claude candidate identity is unavailable")
		}
		j.CandidateCreated, j.CandidateDevice, j.CandidateInode = created, uint64(stat.Dev), uint64(stat.Ino)
		if err = writeClaudeRecovery(l, j); err != nil {
			return err
		}
		if err = os.Remove(staged.Binary); err != nil {
			return err
		}
		if _, err = m.currentClaudeDistribution(ctx, current); err != nil {
			return err
		}
		if err = atomicCodexSymlink(j.candidatePath(), current.Binary); err != nil {
			return err
		}
		j.CandidateSelected = true
	}
	if err = m.claudeLauncherMatches(j, true); err != nil {
		return err
	}
	j.Phase = "starting"
	if err = writeClaudeRecovery(l, j); err != nil {
		return err
	}
	if j.Restart {
		after := m.updateNow()
		if err = m.Service(ctx, l, "start"); err != nil {
			return err
		}
		if err = m.claudeRuntimeReady(ctx, l, after, j.Profiles, j.CandidateVersion); err != nil {
			return err
		}
	}
	j.Phase = "committed"
	if err = writeClaudeRecovery(l, j); err != nil {
		return err
	}
	if err = WriteJSON(filepath.Join(backup, "receipt.json"), j); err != nil {
		return err
	}
	if err = clearClaudeRecovery(l); err != nil {
		return err
	}
	m.pruneClaudeBackups(l)
	return nil
}

func (m *Manager) claudeContextUnchanged(l *Layout, j *claudeRecovery) error {
	if err := validateClaudeRecovery(l, j); err != nil {
		return err
	}
	for _, path := range []string{l.Home, filepath.Join(l.Home, ".config"), filepath.Join(l.Home, ".local"), filepath.Join(l.Home, ".local/bin"), filepath.Join(l.Home, ".local/state"), filepath.Join(l.Home, ".local/share"), filepath.Join(l.Home, ".local/share/claude"), j.Distribution.VersionsDir, filepath.Dir(l.Config), filepath.Dir(l.Lock), claudeBackupRoot(l), j.BackupDir} {
		if !claudeOwnedPath(path, true) {
			return errors.New("Claude maintenance directories changed")
		}
	}
	for _, path := range []string{claudeBackupRoot(l), j.BackupDir} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			return errors.New("Claude executable backups must remain private")
		}
	}
	for path, expected := range map[string]string{l.Config: j.ConfigSHA256, l.Binary: j.WorkerSHA256, filepath.Join(j.BackupDir, "claude"): j.OldSHA256} {
		h, err := claudeFileHash(path)
		if err != nil || h != expected {
			return errors.New("Claude maintenance configuration or executable identity changed")
		}
	}
	if !claudeOwnedPath(j.Distribution.VersionsDir, true) || !claudeOwnedPath(filepath.Dir(j.Distribution.Binary), true) {
		return errors.New("Claude installation directories changed")
	}
	return nil
}

func (m *Manager) claudeLauncherMatches(j *claudeRecovery, candidate bool) error {
	target, err := os.Readlink(j.Distribution.Binary)
	if err != nil {
		return errors.New("Claude launcher changed independently")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(j.Distribution.Binary), target)
	}
	target = filepath.Clean(target)
	expected, hash := j.oldPath(), j.OldSHA256
	if candidate {
		expected, hash = j.candidatePath(), j.CandidateSHA256
	}
	if target != expected {
		return errors.New("Claude launcher changed independently")
	}
	actual, err := claudeFileHash(target)
	if err != nil || actual != hash {
		return errors.New("Claude native executable changed independently")
	}
	return nil
}

func (m *Manager) rejectClaudeVersion(ctx context.Context, l *Layout, version string, cause error) error {
	return errors.Join(cause, m.saveClaudeRejectedVersion(ctx, l, version))
}

func (m *Manager) saveClaudeRejectedVersion(ctx context.Context, l *Layout, version string) error {
	state, err := loadClaudeUpdateState(l)
	if err != nil {
		return err
	}
	if !slices.Contains(state.RejectedVersions, version) {
		state.RejectedVersions = append(state.RejectedVersions, version)
		if len(state.RejectedVersions) > 64 {
			state.RejectedVersions = state.RejectedVersions[len(state.RejectedVersions)-64:]
		}
	}
	if data, err := m.command(ctx, l.Binary, "version"); err == nil {
		state.RejectedByWorkerVersion = strings.TrimSpace(string(data))
	}
	if err := WriteJSON(claudeUpdateStatePath(l), state); err != nil {
		return err
	}
	return nil
}

func (m *Manager) managedClaudeWorkerPID(ctx context.Context, l *Layout) (int, error) {
	if l.System == "linux" {
		data, err := m.command(ctx, "systemctl", "--user", "show", componentBinary(l)+".service", "-p", "MainPID", "-p", "ActiveState")
		if err != nil {
			return 0, err
		}
		values := serviceDetails(data)
		pid, err := strconv.Atoi(values["MainPID"])
		if err == nil && pid > 0 && values["ActiveState"] == "active" {
			return pid, nil
		}
	} else {
		data, err := m.command(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+workerLabel(l))
		if err != nil {
			return 0, err
		}
		pid, matches := 0, 0
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.TrimSpace(key) == "pid" {
				pid, err = strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return 0, errors.New("Claude worker PID is invalid")
				}
				matches++
			}
		}
		if pid > 0 && matches == 1 {
			return pid, nil
		}
	}
	return 0, errors.New("Claude worker service has no verifiable process")
}

func (m *Manager) claudeRuntimeReady(ctx context.Context, l *Layout, after time.Time, profiles map[string]bool, version string) error {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	workerID, _ := cfg["worker_id"].(string)
	ctx, cancel := m.readyContext(ctx)
	defer cancel()
	required := 0
	for _, yes := range profiles {
		if yes {
			required++
		}
	}
	for ctx.Err() == nil {
		pid, pidErr := m.managedClaudeWorkerPID(ctx, l)
		data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
		var s claudeObservedStatus
		if pidErr == nil && err == nil && len(data) <= 8<<20 && json.Unmarshal(data, &s) == nil && s.PID == pid && workerID != "" && s.WorkerID == workerID && s.Connected && !s.UpdatedAt.Before(after) && m.updateNow().Sub(s.UpdatedAt) <= 20*time.Second && !s.UpdatedAt.After(m.updateNow().Add(5*time.Second)) {
			seen := map[string]bool{}
			ready := true
			for _, r := range s.Runtimes {
				if !profiles[r.ProfileID] {
					continue
				}
				v, e := parseClaudeVersion(r.Version)
				if seen[r.ProfileID] || r.State != "running" || e != nil || v != version {
					ready = false
				}
				seen[r.ProfileID] = true
			}
			if ready && len(seen) == required {
				return nil
			}
		}
		if m.readyPause(ctx) != nil {
			break
		}
	}
	return errors.New("Claude worker restarted but its updated runtimes did not become ready")
}

func (m *Manager) recoverClaudeRuntime(ctx context.Context, l *Layout, j *claudeRecovery) error {
	if err := m.claudeContextUnchanged(l, j); err != nil {
		return err
	}
	if j.Phase == "committed" {
		return m.finishClaudeRecovery(l, j)
	}
	oldErr := m.claudeLauncherMatches(j, false)
	newErr := m.claudeLauncherMatches(j, true)
	// A pruned original version is recoverable from the private executable copy,
	// but an unrelated launcher or existing changed version is never overwritten.
	link, err := os.Readlink(j.Distribution.Binary)
	if err != nil {
		return errors.New("Claude recovery cannot verify its launcher")
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(j.Distribution.Binary), link)
	}
	link = filepath.Clean(link)
	if link != j.oldPath() && link != j.candidatePath() {
		return errors.New("Claude launcher was updated independently; recovery left it untouched")
	}
	if link == j.candidatePath() && newErr != nil {
		return newErr
	}
	if link == j.candidatePath() && j.CandidateVersion != j.Distribution.Version {
		j.CandidateSelected = true
	}
	if _, err := os.Lstat(j.oldPath()); err == nil && oldErr != nil && link == j.oldPath() {
		return oldErr
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return err
	}
	if j.Phase == "prepared" && running {
		pid, err := m.managedClaudeWorkerPID(ctx, l)
		if err != nil || pid != j.OldPID || oldErr != nil {
			return &BusyError{Reason: "Claude recovery deferred: managed worker changed"}
		}
		return m.finishClaudeRecovery(l, j)
	}
	if running {
		lease, err := m.workerPrepare(ctx, l)
		if err != nil {
			return err
		}
		if err = m.claudeContextUnchanged(l, j); err != nil {
			_ = m.workerAbort(ctx, l, lease.Token)
			return err
		}
		if lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
			_ = m.workerAbort(ctx, l, lease.Token)
			return &BusyError{Reason: "Claude recovery reservation expired before shutdown"}
		}
		if err = m.Service(ctx, l, "stop"); err != nil {
			_ = m.workerAbort(ctx, l, lease.Token)
			return err
		}
	}
	if live, err := m.workerRunning(ctx, l); err != nil {
		return err
	} else if live {
		return errors.New("Claude recovery could not confirm its worker stopped")
	}
	if err = m.claudeContextUnchanged(l, j); err != nil {
		return err
	}
	j.Phase = "rollback"
	if err = writeClaudeRecovery(l, j); err != nil {
		return err
	}
	if info, err := os.Lstat(j.oldPath()); errors.Is(err, os.ErrNotExist) {
		temp, err := os.MkdirTemp(j.Distribution.VersionsDir, ".mact-restore-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temp)
		path := filepath.Join(temp, "claude")
		if err = claudeCopyExecutable(filepath.Join(j.BackupDir, "claude"), path, j.OldSHA256, os.FileMode(j.OldMode)); err != nil {
			return err
		}
		if _, err = claudePublishExecutable(path, j.oldPath(), j.OldSHA256); err != nil {
			return err
		}
	} else if err != nil || !info.Mode().IsRegular() {
		return errors.New("previous Claude release path is unavailable")
	}
	if h, err := claudeFileHash(j.oldPath()); err != nil || h != j.OldSHA256 {
		return errors.New("previous Claude release was replaced independently")
	}
	// Recheck immediately before publishing the old link; never overwrite an
	// unrelated native updater's selection during rollback.
	current, err := os.Readlink(j.Distribution.Binary)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(current) {
		current = filepath.Join(filepath.Dir(j.Distribution.Binary), current)
	}
	if filepath.Clean(current) != link {
		return errors.New("Claude launcher changed during recovery")
	}
	if err = atomicCodexSymlink(j.OldTarget, j.Distribution.Binary); err != nil {
		return err
	}
	if j.Restart {
		after := m.updateNow()
		if err = m.Service(ctx, l, "start"); err != nil {
			return err
		}
		if err = m.claudeRuntimeReady(ctx, l, after, j.Profiles, j.Distribution.Version); err != nil {
			return err
		}
	}
	if j.CandidateSelected && j.CandidateVersion != j.Distribution.Version {
		if err = m.saveClaudeRejectedVersion(ctx, l, j.CandidateVersion); err != nil {
			return err
		}
	}
	return m.finishClaudeRecovery(l, j)
}

func (m *Manager) finishClaudeRecovery(l *Layout, j *claudeRecovery) error {
	state, err := loadClaudeUpdateState(l)
	if err != nil {
		return err
	}
	state.RestartPending = false
	if err = WriteJSON(claudeUpdateStatePath(l), state); err != nil {
		return err
	}
	if err = WriteJSON(filepath.Join(j.BackupDir, "receipt.json"), j); err != nil {
		return err
	}
	return clearClaudeRecovery(l)
}

// Keep three private rollback copies. Tiny ownership receipts may outlive a
// copy while that version remains protected; otherwise its later removal would
// lose the proof needed to prune MACT-created native binaries safely.
func (m *Manager) pruneClaudeBackups(l *Layout) {
	for _, path := range []string{l.Home, filepath.Join(l.Home, ".local"), filepath.Join(l.Home, ".local/bin"), filepath.Join(l.Home, ".local/share"), filepath.Join(l.Home, ".local/share/claude"), filepath.Join(l.Home, ".local/share/claude/versions"), filepath.Join(l.Home, ".local/state"), filepath.Dir(l.Lock), claudeBackupRoot(l)} {
		if !claudeOwnedPath(path, true) {
			return
		}
	}
	rootInfo, err := os.Lstat(claudeBackupRoot(l))
	if err != nil || rootInfo.Mode().Perm()&0077 != 0 {
		return
	}
	entries, err := os.ReadDir(claudeBackupRoot(l))
	if err != nil {
		return
	}
	type backup struct {
		path string
		j    claudeRecovery
		full bool
	}
	var records []backup
	for _, entry := range entries {
		path := filepath.Join(claudeBackupRoot(l), entry.Name())
		if !strings.HasPrefix(entry.Name(), "update-") || !claudeOwnedPath(path, true) {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			continue
		}
		files, err := os.ReadDir(path)
		if err != nil || len(files) < 1 || len(files) > 2 {
			continue
		}
		known := true
		for _, file := range files {
			if file.Name() != "claude" && file.Name() != "receipt.json" {
				known = false
			}
		}
		if !known {
			continue
		}
		metadata := filepath.Join(path, "receipt.json")
		info, err = os.Lstat(metadata)
		if err != nil || !claudeOwnedPath(metadata, false) || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
			continue
		}
		data, err := os.ReadFile(metadata)
		var j claudeRecovery
		if err != nil || json.Unmarshal(data, &j) != nil || validateClaudeRecovery(l, &j) != nil || j.BackupDir != path || (j.Phase != "committed" && j.Phase != "rollback" && j.Phase != "prepared") {
			continue
		}
		full := len(files) == 2
		if full {
			if h, err := claudeFileHash(filepath.Join(path, "claude")); err != nil || h != j.OldSHA256 {
				continue
			}
		}
		records = append(records, backup{path, j, full})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].j.CreatedAt.After(records[j].j.CreatedAt) })
	selected, err := os.Readlink(filepath.Join(l.Home, ".local/bin/claude"))
	if err != nil {
		return
	}
	if !filepath.IsAbs(selected) {
		selected = filepath.Join(l.Home, ".local/bin", selected)
	}
	protected := map[string]bool{filepath.Clean(selected): true}
	keep := map[string]bool{}
	copies := 0
	for _, record := range records {
		if record.full && copies < 3 {
			keep[record.path] = true
			protected[record.j.oldPath()] = true
			protected[record.j.candidatePath()] = true
			copies++
		}
	}
	for _, record := range records {
		if keep[record.path] {
			continue
		}
		if record.full {
			if err := os.Remove(filepath.Join(record.path, "claude")); err != nil {
				continue
			}
		}
		j := record.j
		if j.CandidateCreated && protected[j.candidatePath()] {
			_ = SyncDir(record.path)
			continue
		}
		if j.CandidateCreated && !pruneClaudeNativeVersion(&j, claudeFileHash) {
			continue
		}
		if err := os.Remove(filepath.Join(record.path, "receipt.json")); err != nil {
			continue
		}
		_ = os.Remove(record.path)
	}
	_ = SyncDir(claudeBackupRoot(l))
}

// A pathname may be replaced while its old descriptor is being hashed. Check
// its identity again afterward, and recheck the current selection immediately
// before unlinking. Changed files are no longer considered MACT-owned.
func pruneClaudeNativeVersion(j *claudeRecovery, hash func(string) (string, error)) bool {
	path := j.candidatePath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || uint64(stat.Dev) != j.CandidateDevice || uint64(stat.Ino) != j.CandidateInode {
		return true
	}
	h, err := hash(path)
	if err != nil {
		return false
	}
	if h != j.CandidateSHA256 {
		return true
	}
	after, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if !os.SameFile(info, after) || !after.Mode().IsRegular() || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return true
	}
	selected, err := os.Readlink(j.Distribution.Binary)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(selected) {
		selected = filepath.Join(filepath.Dir(j.Distribution.Binary), selected)
	}
	if filepath.Clean(selected) == path {
		return false
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return SyncDir(j.Distribution.VersionsDir) == nil
}
