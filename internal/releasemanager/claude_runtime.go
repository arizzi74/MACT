package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type claudeUpdateState struct {
	Schema                  int       `json:"schema"`
	CheckedAt               time.Time `json:"checked_at,omitempty"`
	Channel                 string    `json:"channel,omitempty"`
	LatestVersion           string    `json:"latest_version,omitempty"`
	CheckFailed             bool      `json:"check_failed,omitempty"`
	RestartPending          bool      `json:"restart_pending,omitempty"`
	RejectedVersions        []string  `json:"rejected_versions,omitempty"`
	RejectedByWorkerVersion string    `json:"rejected_by_worker_version,omitempty"`
}

type claudeRuntimeProfile struct {
	ID        string `json:"id"`
	Binary    string `json:"claude_binary"`
	Autostart bool   `json:"autostart"`
}

type claudeObservedStatus struct {
	WorkerID  string    `json:"worker_id"`
	PID       int       `json:"pid"`
	UpdatedAt time.Time `json:"updated_at"`
	Connected bool      `json:"gateway_connected"`
	Runtimes  []struct {
		ProfileID string `json:"profile_id"`
		Version   string `json:"codex_version"`
		State     string `json:"state"`
	} `json:"runtimes"`
	Sessions []struct {
		State        string `json:"state"`
		ActiveTurnID string `json:"active_turn_id"`
	} `json:"sessions"`
}

type claudeRuntimePlan struct {
	Distribution  *claudeDistribution
	TargetVersion string
	NeedsInstall  bool
	Profiles      map[string]bool
}

func claudeUpdateStatePath(l *Layout) string {
	return filepath.Join(filepath.Dir(l.State), "claude-update.json")
}

func loadClaudeUpdateState(l *Layout) (claudeUpdateState, error) {
	state := claudeUpdateState{Schema: 1}
	path := claudeUpdateStatePath(l)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil || !claudeOwnedPath(path, false) || info.Mode().Perm()&0077 != 0 || info.Size() > 16<<10 {
		return state, errors.New("cannot safely read Claude runtime update state")
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &state) != nil || state.Schema != 1 || len(state.RejectedVersions) > 64 {
		return state, errors.New("invalid Claude runtime update state")
	}
	if state.Channel != "" && state.Channel != "latest" && state.Channel != "stable" {
		return state, errors.New("invalid Claude runtime update channel")
	}
	for _, version := range append(append([]string{}, state.RejectedVersions...), state.LatestVersion, state.RejectedByWorkerVersion) {
		if version != "" {
			if _, err := ParseVersion(version); err != nil {
				return state, errors.New("invalid cached Claude runtime version")
			}
		}
	}
	return state, nil
}

func claudeProfileBinary(l *Layout, binary string) (string, error) {
	return codexProfileBinary(l, binary)
}

func (m *Manager) claudeRuntimePlans(ctx context.Context, l *Layout) ([]claudeRuntimePlan, string, error) {
	data, err := os.ReadFile(l.Config)
	if err != nil || len(data) > 2<<20 {
		return nil, "", errors.New("cannot read Claude worker runtime configuration")
	}
	var cfg struct {
		WorkerID  string                 `json:"worker_id"`
		AgentKind string                 `json:"agent_kind"`
		Profiles  []claudeRuntimeProfile `json:"runtimes"`
	}
	if json.Unmarshal(data, &cfg) != nil || cfg.WorkerID == "" || cfg.AgentKind != "claude" || len(cfg.Profiles) > 128 {
		return nil, "", errors.New("invalid Claude worker runtime configuration")
	}
	var plan *claudeRuntimePlan
	seen := map[string]bool{}
	for _, profile := range cfg.Profiles {
		if profile.ID == "" || len(profile.ID) > 256 || seen[profile.ID] || profile.Binary == "" {
			return nil, "", errors.New("invalid Claude runtime profile")
		}
		seen[profile.ID] = true
		binary, err := claudeProfileBinary(l, profile.Binary)
		if err != nil {
			return nil, "", err
		}
		d, err := m.inspectClaudeDistribution(ctx, binary)
		if errors.Is(err, ErrClaudeDistributionUnsupported) {
			fmt.Fprintln(m.Out, "Claude runtime update skipped: this profile is not a supported native installation.")
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if d.Home != l.Home {
			return nil, "", errors.New("Claude installation belongs to a different worker home")
		}
		if plan == nil {
			plan = &claudeRuntimePlan{Distribution: d, Profiles: map[string]bool{}}
		}
		if *plan.Distribution != *d {
			return nil, "", errors.New("Claude worker profiles must share one native installation")
		}
		plan.Profiles[profile.ID] = profile.Autostart
	}
	if plan == nil {
		return nil, cfg.WorkerID, nil
	}
	return []claudeRuntimePlan{*plan}, cfg.WorkerID, nil
}

// Discovery is daily; a discovered update is reconsidered at every maintenance
// tick. The installation lock is held by the caller, just as for Codex updates.
func (m *Manager) UpdateClaudeRuntime(ctx context.Context, l *Layout, check bool) error {
	return m.updateClaudeRuntime(ctx, l, check, "")
}

func (m *Manager) updateClaudeRuntime(ctx context.Context, l *Layout, check bool, latestOverride string) error {
	if l.Component != "worker" || l.AgentKind != "claude" {
		return errors.New("Claude runtime updates require a dedicated Claude worker")
	}
	journal, err := loadClaudeRecovery(l)
	if err != nil {
		return err
	}
	if journal != nil {
		if check {
			return &BusyError{Reason: "Claude runtime recovery is pending"}
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if err := m.recoverClaudeRuntime(cleanup, l, journal); err != nil {
			return err
		}
		fmt.Fprintln(m.Out, "Interrupted Claude runtime maintenance recovered; normal checks resume on the next tick.")
		return nil
	}
	state, err := loadClaudeUpdateState(l)
	if err != nil {
		return err
	}
	policy, err := readClaudeUpdatePolicy(l.Home)
	if err != nil {
		return err
	}
	if state.Channel != policy.Channel {
		state.Channel = policy.Channel
		if latestOverride == "" {
			state.CheckedAt = time.Time{}
			state.LatestVersion = ""
			state.CheckFailed = false
		}
	}
	if latestOverride != "" {
		if _, err := ParseVersion(latestOverride); err != nil {
			return errors.New("invalid explicit Claude release version")
		}
		state.LatestVersion = latestOverride
		state.CheckFailed = false
	}
	if state.RejectedByWorkerVersion != "" && len(state.RejectedVersions) > 0 {
		if output, err := m.command(ctx, l.Binary, "version"); err == nil && codexWorkerUpgrade(strings.TrimSpace(string(output)), state.RejectedByWorkerVersion) {
			state.RejectedVersions = nil
			state.RejectedByWorkerVersion = ""
		}
	}
	plans, workerID, err := m.claudeRuntimePlans(ctx, l)
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return nil
	}
	var checkErr error
	if latestOverride == "" && codexCheckDue(codexUpdateState{CheckedAt: state.CheckedAt}, m.updateNow()) {
		state.CheckedAt = m.updateNow().UTC()
		state.CheckFailed = true
		if !check {
			if err := WriteJSON(claudeUpdateStatePath(l), state); err != nil {
				return err
			}
		}
		latest, err := m.fetchClaudeChannel(ctx, policy.Channel)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			checkErr = err
		} else {
			state.LatestVersion = latest
			state.CheckFailed = false
			if !check {
				if err := WriteJSON(claudeUpdateStatePath(l), state); err != nil {
					return err
				}
			}
		}
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return errors.Join(checkErr, err)
	}
	var observed claudeObservedStatus
	if running {
		data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
		if err != nil || len(data) > 8<<20 || json.Unmarshal(data, &observed) != nil || observed.WorkerID != workerID || observed.PID <= 0 || m.updateNow().Sub(observed.UpdatedAt) > 20*time.Second || observed.UpdatedAt.After(m.updateNow().Add(5*time.Second)) {
			return errors.Join(checkErr, &BusyError{Reason: "Claude runtime update deferred: fresh worker status is unavailable"})
		}
	}
	maintenance := state.RestartPending
	for i := range plans {
		p := &plans[i]
		p.TargetVersion = p.Distribution.Version
		if state.LatestVersion != "" {
			comparison, err := CompareVersions(state.LatestVersion, p.TargetVersion)
			if err != nil {
				return err
			}
			if comparison > 0 && !slices.Contains(state.RejectedVersions, state.LatestVersion) {
				p.TargetVersion = state.LatestVersion
				p.NeedsInstall = true
				maintenance = true
				fmt.Fprintf(m.Out, "Claude runtime update available: %s -> %s (%s).\n", p.Distribution.Version, p.TargetVersion, policy.Channel)
			}
		}
		for _, r := range observed.Runtimes {
			if _, ok := p.Profiles[r.ProfileID]; !ok || r.State == "stopped" {
				continue
			}
			version, err := parseClaudeVersion(r.Version)
			if err != nil {
				return errors.Join(checkErr, &BusyError{Reason: "Claude runtime update deferred: running version is unavailable"})
			}
			if comparison, _ := CompareVersions(version, p.Distribution.Version); comparison < 0 {
				maintenance = true
				fmt.Fprintln(m.Out, "Claude runtime restart pending after an external native update.")
			}
		}
	}
	if check || !maintenance {
		if !maintenance && checkErr == nil {
			if slices.Contains(state.RejectedVersions, state.LatestVersion) {
				fmt.Fprintln(m.Out, "The latest Claude release failed validation previously and is withheld.")
			} else if state.CheckFailed {
				fmt.Fprintln(m.Out, "The last Claude release check failed; discovery retries on the next UTC day.")
			} else {
				fmt.Fprintf(m.Out, "Claude runtime is current on its %s channel; automatic discovery runs once per UTC day.\n", policy.Channel)
			}
		}
		return checkErr
	}
	if running {
		pid, pidErr := m.managedClaudeWorkerPID(ctx, l)
		if pidErr != nil || pid != observed.PID {
			return errors.Join(checkErr, &BusyError{Reason: "Claude runtime update deferred: managed worker changed"})
		}
		seen := map[string]bool{}
		for _, runtime := range observed.Runtimes {
			if seen[runtime.ProfileID] {
				return errors.Join(checkErr, &BusyError{Reason: "Claude runtime update deferred: duplicate runtime evidence"})
			}
			seen[runtime.ProfileID] = true
		}
		for _, plan := range plans {
			for profile, autostart := range plan.Profiles {
				if !autostart {
					continue
				}
				ready := false
				for _, runtime := range observed.Runtimes {
					if runtime.ProfileID == profile && runtime.State == "running" {
						_, err := parseClaudeVersion(runtime.Version)
						ready = err == nil
					}
				}
				if !ready {
					return errors.Join(checkErr, &BusyError{Reason: "Claude runtime update deferred: an autostart runtime is not ready"})
				}
			}
		}
	}
	// This snapshot is only a resource-saving hint. A new live idle lease below
	// remains mandatory, and additionally fences queues and native background work.
	for _, s := range observed.Sessions {
		if s.ActiveTurnID != "" || s.State == "running" || s.State == "active" || strings.HasPrefix(s.State, "waiting") {
			return errors.Join(checkErr, &BusyError{Reason: "Claude runtime update deferred: a session has active work"})
		}
	}
	if err := m.applyClaudeRuntimeUpdates(ctx, l, plans, state.RestartPending, func() error { state.RestartPending = true; return WriteJSON(claudeUpdateStatePath(l), state) }); err != nil {
		return errors.Join(checkErr, err)
	}
	state.RestartPending = false
	if err := WriteJSON(claudeUpdateStatePath(l), state); err != nil {
		return errors.Join(checkErr, err)
	}
	fmt.Fprintln(m.Out, "Claude runtime maintenance completed.")
	return checkErr
}
