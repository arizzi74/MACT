package claudeadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os/exec"
	"time"

	"github.com/google/uuid"
)

// NativeUsage is the display projection of Claude Code's structured /usage
// control. It contains no credentials, billing identity or transcript analysis.
// Account observations are kept in memory, independently of session cost.
type NativeUsage struct {
	Plan       string        `json:"plan,omitempty"`
	Windows    []UsageWindow `json:"windows,omitempty"`
	APISpend   *UsageSpend   `json:"apiSpend,omitempty"`
	ObservedAt int64         `json:"observedAt"`
}

type UsageWindow struct {
	Name        string  `json:"name"`
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    *int64  `json:"resetsAt,omitempty"`
}

type UsageSpend struct {
	Enabled   bool     `json:"enabled"`
	Currency  string   `json:"currency"`
	Period    string   `json:"period"`
	Limit     *float64 `json:"limit"`
	Spent     *float64 `json:"spent"`
	Remaining *float64 `json:"remaining"`
}

var errNativeUsageUnavailable = errors.New("Claude Code usage is unavailable")
var errNativePermissionUnavailable = errors.New("Claude Code did not confirm the requested permission mode; it may be disabled by settings or organization policy. Existing session settings were kept")

// ReadNativeUsage reuses a ready conversation's native usage cache and provider
// cooldowns. With no loaded conversation, an isolated, non-persistent process
// supplies the same read-only control. Neither path submits a model prompt.
func (r *Runtime) ReadNativeUsage(ctx context.Context) (*NativeUsage, error) {
	if r == nil || r.server == nil {
		return nil, errNativeUsageUnavailable
	}
	s := r.server
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errNativeUsageUnavailable
	}
	s.wg.Add(1)
	var native *process
	for _, candidate := range s.runners {
		if s.threads[candidate.threadID] == nil {
			continue
		}
		select {
		case <-candidate.ready:
			if candidate.initErr == nil && !candidate.interrupting {
				native = candidate
				if s.threads[candidate.threadID].active() != nil {
					break
				}
			}
		default:
		}
		if native != nil && s.threads[native.threadID].active() != nil {
			break
		}
	}
	s.mu.Unlock()
	defer s.wg.Done()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	if native != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		raw, err := native.controlRequest(ctx, map[string]any{"subtype": "get_usage", "skip_behaviors": true}, false)
		if err != nil {
			return nil, errNativeUsageUnavailable
		}
		return decodeNativeUsage(raw, time.Now().UTC())
	}
	settings, err := s.settings()
	if err != nil {
		return nil, errNativeUsageUnavailable
	}
	_, raw, err := s.metadataProbe(ctx, settings, true)
	if err != nil {
		return nil, errNativeUsageUnavailable
	}
	return decodeNativeUsage(raw, time.Now().UTC())
}

func (r *Runtime) SetNativeUsageAt(generation uint64, usage *NativeUsage) error {
	if r == nil || r.server == nil {
		return errNativeUsageUnavailable
	}
	if !validNativeUsage(usage) {
		return errNativeUsageUnavailable
	}
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || generation != s.balanceGeneration {
		return errNativeUsageUnavailable
	}
	// Re-decode the small allowlisted projection to avoid sharing caller-owned
	// pointers or slices with snapshots and concurrent status readers.
	s.nativeUsage = cloneNativeUsage(usage)
	for _, t := range s.threads {
		s.broadcastStatus(t)
	}
	return nil
}

func validNativeUsage(usage *NativeUsage) bool {
	if usage == nil {
		return true
	}
	if usage.ObservedAt <= 0 || usage.ObservedAt > 253402300799 || len(usage.Windows) > 32 {
		return false
	}
	switch usage.Plan {
	case "", "pro", "max", "team", "enterprise":
	default:
		return false
	}
	for _, window := range usage.Windows {
		if window.Name == "" || window.Name != boundedText(window.Name, 128) || !validUSD(window.UsedPercent) || window.UsedPercent > 1_000_000 {
			return false
		}
		if window.ResetsAt != nil && (*window.ResetsAt <= 0 || *window.ResetsAt > 253402300799) {
			return false
		}
	}
	if spend := usage.APISpend; spend != nil {
		if spend.Currency != "USD" || spend.Period != "monthly" {
			return false
		}
		for _, v := range []*float64{spend.Limit, spend.Spent, spend.Remaining} {
			if v != nil && (!validUSD(*v) || *v > 10_000_000_000) {
				return false
			}
		}
		if spend.Limit == nil || spend.Spent == nil {
			return spend.Remaining == nil
		}
		if spend.Remaining == nil || math.Abs(*spend.Remaining-math.Max(0, *spend.Limit-*spend.Spent)) > math.Max(1e-8, *spend.Limit*1e-12) {
			return false
		}
	}
	return true
}

func cloneNativeUsage(value *NativeUsage) *NativeUsage {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	var copy NativeUsage
	if json.Unmarshal(data, &copy) != nil {
		return nil
	}
	return &copy
}

// The native SDK's extra_usage amounts are currency minor units, not dollars.
// Only explicit USD is projected as money; an absent currency or null amount
// never becomes a zero balance or an invented unlimited allowance.
func decodeNativeUsage(raw json.RawMessage, now time.Time) (*NativeUsage, error) {
	var reply struct {
		Plan      string                     `json:"subscription_type"`
		Available bool                       `json:"rate_limits_available"`
		Limits    map[string]json.RawMessage `json:"rate_limits"`
	}
	if len(raw) > 4<<20 || json.Unmarshal(raw, &reply) != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, errNativeUsageUnavailable
	}
	usage := &NativeUsage{ObservedAt: now.Unix()}
	switch reply.Plan {
	case "pro", "max", "team", "enterprise":
		usage.Plan = reply.Plan
	}
	if !reply.Available || reply.Limits == nil {
		return usage, nil
	}
	for _, window := range []struct{ key, name string }{{"five_hour", "Current session"}, {"seven_day", "Current week"}, {"seven_day_opus", "Current week (Opus)"}, {"seven_day_sonnet", "Current week (Sonnet)"}, {"seven_day_oauth_apps", "Current week (OAuth apps)"}} {
		var value struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if json.Unmarshal(reply.Limits[window.key], &value) != nil || value.Utilization == nil || !validUSD(*value.Utilization) || *value.Utilization > 1_000_000 {
			continue
		}
		row := UsageWindow{Name: window.name, UsedPercent: *value.Utilization}
		if value.ResetsAt != nil {
			if reset, err := time.Parse(time.RFC3339Nano, *value.ResetsAt); err == nil && reset.Unix() > 0 && reset.Year() < 10000 {
				if !reset.After(now) {
					continue
				}
				seconds := reset.Unix()
				row.ResetsAt = &seconds
			}
		}
		usage.Windows = append(usage.Windows, row)
	}
	var spend struct {
		Enabled       *bool    `json:"is_enabled"`
		Currency      string   `json:"currency"`
		Limit         *float64 `json:"monthly_limit"`
		Spent         *float64 `json:"used_credits"`
		DecimalPlaces *int     `json:"decimal_places"`
	}
	if json.Unmarshal(reply.Limits["extra_usage"], &spend) == nil && spend.Enabled != nil && spend.Currency == "USD" && (spend.DecimalPlaces == nil || *spend.DecimalPlaces == 2) {
		money := func(value *float64) *float64 {
			if value == nil || !validUSD(*value) || *value > 1_000_000_000_000 {
				return nil
			}
			dollars := *value / 100
			return &dollars
		}
		budget := &UsageSpend{Enabled: *spend.Enabled, Currency: "USD", Period: "monthly", Limit: money(spend.Limit), Spent: money(spend.Spent)}
		if budget.Limit != nil && budget.Spent != nil {
			left := math.Max(0, *budget.Limit-*budget.Spent)
			budget.Remaining = &left
		}
		usage.APISpend = budget
	}
	return usage, nil
}

// metadataProbe sends only initialize, get_usage, or a permission-mode check.
// Administrator positional Args are excluded so this cannot submit a prompt.
// skip_behaviors avoids Claude's scan of the last seven days of transcripts.
func (s *server) metadataProbe(parent context.Context, settings LaunchSettings, usage bool, permission ...string) (json.RawMessage, json.RawMessage, error) {
	if parent == nil {
		parent = context.Background()
	}
	timeout := min(s.cfg.StartupTimeout, 10*time.Second)
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	env, err := childEnvironment(s.cfg.Env, settings.Env)
	if err != nil {
		return nil, nil, errNativeUsageUnavailable
	}
	args := []string{"--print", "--verbose", "--input-format=stream-json", "--output-format=stream-json", "--permission-prompt-tool=stdio", "--no-session-persistence", "--session-id=" + uuid.NewString()}
	mode := ""
	if len(permission) != 0 {
		mode = permission[0]
		if !permissionMode(mode) {
			return nil, nil, errNativePermissionUnavailable
		}
		if mode == "bypassPermissions" {
			// Make the mode available to the native control without activating
			// it or bypassing any organization policy. No user query is sent.
			args = append(args, "--allow-dangerously-skip-permissions")
		}
		if settings.Model != "" {
			args = append(args, "--model="+settings.Model)
		}
	}
	if settings.SettingsFile != "" {
		args = append(args, "--settings="+settings.SettingsFile)
	}
	cmd := exec.Command(s.cfg.Command, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = s.cfg.WorkingDirectory, env, io.Discard
	configureProcess(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, errNativeUsageUnavailable
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, nil, errNativeUsageUnavailable
	}
	if cmd.Start() != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, nil, errNativeUsageUnavailable
	}
	defer func() { stopProcess(cmd); _ = in.Close(); _ = out.Close(); _ = cmd.Wait() }()
	type response struct {
		Subtype string          `json:"subtype"`
		ID      string          `json:"request_id"`
		Data    json.RawMessage `json:"response"`
	}
	replies := make(chan response, 1)
	go func() {
		defer close(replies)
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			var frame struct {
				Type     string   `json:"type"`
				Response response `json:"response"`
			}
			if json.Unmarshal(scanner.Bytes(), &frame) == nil && frame.Type == "control_response" {
				select {
				case replies <- frame.Response:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	requests := []map[string]any{{"subtype": "initialize", "hooks": map[string]any{}}}
	if usage {
		requests = append(requests, map[string]any{"subtype": "get_usage", "skip_behaviors": true})
	}
	if mode != "" {
		requests = append(requests, map[string]any{"subtype": "set_permission_mode", "mode": mode})
	}
	var initialized, report json.RawMessage
	for index, request := range requests {
		id := "telegramgw-capabilities"
		if index > 0 {
			id = "mact-native-usage"
			if request["subtype"] == "set_permission_mode" {
				id = "mact-native-permission"
			}
		}
		written := make(chan error, 1)
		go func() {
			written <- json.NewEncoder(in).Encode(map[string]any{"type": "control_request", "request_id": id, "request": request})
		}()
		select {
		case err := <-written:
			if err != nil {
				return nil, nil, errNativeUsageUnavailable
			}
		case <-ctx.Done():
			return nil, nil, errNativeUsageUnavailable
		}
		for {
			select {
			case reply, ok := <-replies:
				if !ok {
					return nil, nil, errNativeUsageUnavailable
				}
				if reply.ID != id {
					continue
				}
				if reply.Subtype != "success" {
					if request["subtype"] == "set_permission_mode" {
						return nil, nil, errNativePermissionUnavailable
					}
					return nil, nil, errNativeUsageUnavailable
				}
				if index == 0 {
					initialized = reply.Data
				} else {
					report = reply.Data
				}
			case <-ctx.Done():
				return nil, nil, errNativeUsageUnavailable
			}
			break
		}
	}
	return initialized, report, nil
}
