package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

// Keep discovery beside the durable request so an updater restart or a busy
// worker cannot turn one explicit check into a repeated metadata polling loop.
// This cache never changes the automatic daily discovery timestamp.
func (m *Manager) requestedClaudeDiscovery(ctx context.Context, l *Layout, plan *requestedWorkerPlan) {
	if plan.claudeReport != nil {
		return
	}
	report := &protocol.ClaudeUpdateReport{State: "failed", ErrorCode: "check_failed", CheckedAt: m.updateNow().UTC()}
	plan.claudeReport = report
	if plan.claudeCache != "" {
		info, err := os.Lstat(plan.claudeCache)
		if err == nil {
			if !claudeOwnedPath(plan.claudeCache, false) || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
				return
			}
			data, err := os.ReadFile(plan.claudeCache)
			var cached protocol.ClaudeUpdateReport
			if err != nil || json.Unmarshal(data, &cached) != nil || cached.Validate() != nil || cached.CheckedAt.IsZero() || cached.CheckedAt.After(m.updateNow().Add(5*time.Minute)) || len(cached.Profiles) != 0 {
				return
			}
			if cached.State == "up_to_date" {
				if _, err := ParseVersion(cached.LatestVersion); err != nil {
					return
				}
			} else if cached.State != "failed" || cached.ErrorCode != "check_failed" {
				return
			}
			plan.claudeReport = &cached
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	policy, err := m.readClaudeUpdatePolicy(l.Home)
	latest := ""
	if err == nil {
		report.Channel = policy.Channel
		latest, err = m.fetchClaudeChannel(ctx, policy.Channel)
	}
	if ctx.Err() != nil {
		// Cancellation does not consume the request's opportunity to check.
		plan.claudeReport = nil
		return
	}
	if err == nil {
		report.State, report.ErrorCode, report.LatestVersion = "up_to_date", "", latest
	}
	if plan.claudeCache != "" {
		data, err := json.Marshal(report)
		if err != nil || workerupdate.WritePrivate(plan.claudeCache, data) != nil {
			report.State, report.ErrorCode = "failed", "check_failed"
		}
	}
}

func (m *Manager) requestedClaudeMaintenance(ctx context.Context, l *Layout, plan *requestedWorkerPlan) (protocol.WorkerUpdateResult, error) {
	result := *plan.workerResult
	report := *plan.claudeReport
	result.Claude = &report
	profiles, err := m.requestedClaudeVersions(ctx, l)
	report.Profiles = profiles
	if report.ErrorCode != "" {
		return result, nil
	}
	if err != nil {
		var busy *BusyError
		if errors.As(err, &busy) {
			return result, err
		}
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	if len(profiles) == 0 {
		report.State = "no_runtimes"
		return result, nil
	}
	if !slices.ContainsFunc(profiles, func(p protocol.ClaudeRuntimeVersion) bool { return p.Support == "supported" }) {
		report.State = "unsupported"
		return result, nil
	}
	policy, policyErr := m.readClaudeUpdatePolicy(l.Home)
	if policyErr != nil || report.Channel == "" || report.Channel != policy.Channel {
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	// The runtime helper performs recovery, quarantine, preflight and readiness
	// checks. In particular it acquires a fresh lease after any worker upgrade.
	err = m.updateClaudeRuntime(ctx, l, false, report.LatestVersion)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var busy *BusyError
	if errors.As(err, &busy) {
		return result, err
	}
	after, inspectErr := m.requestedClaudeVersions(ctx, l)
	report.Profiles = after
	if err != nil {
		report.State, report.ErrorCode = "failed", "update_failed"
		return result, nil
	}
	if inspectErr != nil {
		if errors.As(inspectErr, &busy) {
			return result, inspectErr
		}
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	state, err := loadClaudeUpdateState(l)
	if err != nil {
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	for _, profile := range after {
		if profile.Support != "supported" {
			continue
		}
		comparison, _ := CompareVersions(report.LatestVersion, profile.InstalledVersion)
		if comparison > 0 && slices.Contains(state.RejectedVersions, report.LatestVersion) {
			report.State, report.ErrorCode = "withheld", "rejected_release"
			return result, nil
		}
		runningBehind := false
		if profile.RunningVersion != "" {
			comparison, _ := CompareVersions(profile.RunningVersion, profile.InstalledVersion)
			runningBehind = comparison < 0
		}
		if comparison > 0 || runningBehind {
			// An interrupted prepared transaction can recover without installing.
			// Re-enter maintenance on the next retry instead of declaring success.
			return result, &BusyError{Reason: "Claude runtime maintenance recovered; retrying the requested update"}
		}
	}
	report.State = "up_to_date"
	for _, profile := range after {
		if profile.Support != "supported" {
			continue
		}
		for _, previous := range profiles {
			if previous.ProfileID == profile.ProfileID && previous != profile {
				report.State = "completed"
			}
		}
	}
	return result, nil
}

func (m *Manager) requestedClaudeVersions(ctx context.Context, l *Layout) ([]protocol.ClaudeRuntimeVersion, error) {
	data, err := os.ReadFile(l.Config)
	if err != nil {
		return nil, errors.New("cannot inspect configured Claude runtimes")
	}
	var cfg struct {
		WorkerID string                 `json:"worker_id"`
		Profiles []claudeRuntimeProfile `json:"runtimes"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Profiles) > 128 {
		return nil, errors.New("cannot inspect configured Claude runtimes")
	}
	profiles := make([]protocol.ClaudeRuntimeVersion, 0, len(cfg.Profiles))
	required := make(map[string]bool)
	var inspectionErr error
	for _, profile := range cfg.Profiles {
		required[profile.ID] = profile.Autostart
		entry := protocol.ClaudeRuntimeVersion{ProfileID: profile.ID, Support: "unavailable"}
		binary, err := claudeProfileBinary(l, profile.Binary)
		if err == nil {
			distribution, distributionErr := m.inspectClaudeDistribution(ctx, binary)
			if distributionErr == nil {
				entry.InstalledVersion, entry.Support = distribution.Version, "supported"
			} else if errors.Is(distributionErr, ErrClaudeDistributionUnsupported) {
				output, versionErr := m.command(ctx, binary, "--version")
				version := claudeReportedVersion(string(output))
				if versionErr == nil && version != "" {
					entry.InstalledVersion, entry.Support = version, "external"
				}
			}
		}
		if entry.Support == "unavailable" {
			inspectionErr = errors.New("cannot inspect a configured Claude runtime")
		}
		profiles = append(profiles, entry)
	}
	if len(profiles) > 0 {
		running, err := m.workerRunning(ctx, l)
		if err != nil {
			if inspectionErr == nil {
				inspectionErr = &BusyError{Reason: "Claude runtime update deferred: worker status is not yet available"}
			}
		} else if running {
			var observed claudeObservedStatus
			data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
			if err != nil {
				if inspectionErr == nil {
					inspectionErr = &BusyError{Reason: "Claude runtime update deferred: fresh worker status is unavailable"}
				}
			} else if json.Unmarshal(data, &observed) != nil || observed.WorkerID != cfg.WorkerID || observed.PID <= 0 {
				inspectionErr = errors.New("cannot verify running Claude versions")
			} else if m.updateNow().Sub(observed.UpdatedAt) > 20*time.Second || observed.UpdatedAt.After(m.updateNow().Add(5*time.Second)) {
				if inspectionErr == nil {
					inspectionErr = &BusyError{Reason: "Claude runtime update deferred: fresh worker status is unavailable"}
				}
			} else {
				for i := range profiles {
					verified := false
					for _, runtime := range observed.Runtimes {
						if runtime.ProfileID != profiles[i].ProfileID || runtime.State != "running" {
							continue
						}
						version := claudeReportedVersion(runtime.Version)
						if profiles[i].Support == "supported" {
							if _, err := parseClaudeVersion(version); err != nil {
								inspectionErr = errors.New("cannot verify running Claude versions")
								continue
							}
						}
						profiles[i].RunningVersion, verified = version, version != ""
					}
					if required[profiles[i].ProfileID] && !verified && inspectionErr == nil {
						inspectionErr = &BusyError{Reason: "Claude runtime update deferred: an autostart runtime is not ready"}
					}
				}
			}
		}
	}
	// Do not let malformed local metadata inject messages or make the durable
	// completion unrepresentable. Report inspection failure without those values.
	if (protocol.ClaudeUpdateReport{State: "up_to_date", Profiles: profiles}).Validate() != nil {
		return nil, errors.New("invalid configured Claude version metadata")
	}
	return profiles, inspectionErr
}

func claudeReportedVersion(output string) string {
	parts := strings.Fields(output)
	if len(parts) == 0 {
		return ""
	}
	if _, err := ParseVersion(parts[0]); err != nil {
		return ""
	}
	return parts[0]
}

func (m *Manager) requestedClaudeWorkerStep(ctx context.Context, l *Layout, plan *requestedWorkerPlan) (protocol.WorkerUpdateResult, error) {
	m.requestedClaudeDiscovery(ctx, l, plan)
	if ctx.Err() != nil {
		return protocol.WorkerUpdateResult{}, ctx.Err()
	}
	// An interrupted runtime installation may leave its launcher unavailable.
	// Restore its journal before worker upgrade preflight or pending readiness
	// checks, which also depend on those configured runtime executables.
	journal, recoveryErr := loadClaudeRecovery(l)
	if recoveryErr == nil && journal != nil {
		override := plan.claudeReport.LatestVersion
		policy, policyErr := m.readClaudeUpdatePolicy(l.Home)
		if policyErr != nil || policy.Channel != plan.claudeReport.Channel {
			override = ""
		}
		recoveryErr = m.updateClaudeRuntime(ctx, l, false, override)
	}
	if recoveryErr != nil {
		result := protocol.WorkerUpdateResult{}
		if plan.workerResult != nil {
			result = *plan.workerResult
		}
		report := *plan.claudeReport
		report.State, report.ErrorCode = "failed", "update_failed"
		result.Claude = &report
		var busy *BusyError
		if ctx.Err() != nil || errors.As(recoveryErr, &busy) || plan.workerResult == nil {
			return result, recoveryErr
		}
		return result, nil
	}
	if plan.workerResult == nil {
		result, err := m.requestedWorkerReleaseStep(ctx, l, plan)
		if err != nil {
			report := *plan.claudeReport
			if report.ErrorCode == "" {
				report.State, report.ErrorCode = "failed", "worker_update_failed"
			}
			result.Claude = &report
			return result, err
		}
		plan.workerResult = &result
	}
	return m.requestedClaudeMaintenance(ctx, l, plan)
}
