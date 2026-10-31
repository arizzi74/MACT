package worker

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/claudeadapter"
)

// A missing SDK usage payload deliberately carries no guessed HTTP status.
// Loaded native sessions retain the CLI's own provider cooldown/cache; the
// worker also backs off opaque native failures instead of starting new probes.
type claudeAccountError struct {
	cause   error
	status  int
	seconds uint64
	at      time.Time
}

func (e *claudeAccountError) Error() string { return e.cause.Error() }
func (e *claudeAccountError) Unwrap() error { return e.cause }

func claudeAccountHTTPError(response *http.Response, cause error) error {
	err := &claudeAccountError{cause: cause, status: response.StatusCode}
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if len(value) <= 128 {
		if seconds, parseErr := strconv.ParseUint(value, 10, 64); parseErr == nil {
			err.seconds = seconds
		} else if date, parseErr := http.ParseTime(value); parseErr == nil && date.Year() < 10000 {
			err.at = date
		}
	}
	return err
}

type claudeAccountSchedule struct {
	next     time.Time
	failures int
}

func (s *claudeAccountSchedule) complete(now time.Time, interval time.Duration, err error, jitter func(time.Duration) time.Duration) claudeadapter.AccountRefresh {
	delay := interval
	limited := false
	var serverAt time.Time
	if err == nil {
		s.failures = 0
	} else {
		s.failures++
		for count := 1; count < s.failures && delay < time.Hour; count++ {
			delay = min(delay*2, time.Hour)
		}
		delay += jitter(delay / 10)
		var failure *claudeAccountError
		if errors.As(err, &failure) {
			limited = failure.status == http.StatusTooManyRequests
			serverAt = failure.at
			if failure.seconds != 0 {
				seconds := min(failure.seconds, uint64(253402300799-now.Unix()))
				deadline := time.Unix(now.Unix()+int64(seconds), int64(now.Nanosecond()))
				if deadline.After(serverAt) {
					serverAt = deadline
				}
			}
		}
	}
	s.next = now.Add(delay)
	if serverAt.After(s.next) {
		s.next = serverAt
	}
	return claudeadapter.AccountRefresh{LastAttemptAt: now.Unix(), NextAttemptAt: s.next.Unix(), Failed: err != nil, RateLimited: limited}
}

type claudeAccountPoller struct {
	mu          sync.Mutex // Single flight, including callers outside the timer loop.
	manager     *RuntimeManager
	bridge      *claudeadapter.Runtime
	client      *http.Client
	now         func() time.Time
	jitter      func(time.Duration) time.Duration
	readUsage   func(context.Context) (*claudeadapter.NativeUsage, error)
	initialized bool
	generation  uint64
	settings    claudeSettingsFile
	sources     map[string]*claudeAccountSchedule
	lastUsage   *claudeadapter.NativeUsage
}

func newClaudeAccountPoller(manager *RuntimeManager, bridge *claudeadapter.Runtime, client *http.Client) *claudeAccountPoller {
	return &claudeAccountPoller{manager: manager, bridge: bridge, client: client, now: time.Now,
		jitter: func(limit time.Duration) time.Duration {
			if limit <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(limit)))
		}, readUsage: bridge.ReadNativeUsage}
}

func (p *claudeAccountPoller) poll(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	generation := p.bridge.BalanceGeneration()
	settings, err := readClaudeSettings(claudeSettingsPath(p.manager.cfg))
	if err != nil {
		// A temporarily unreadable settings file is a failed read, not evidence
		// of a different account. Keep the existing, timestamped observations.
		return
	}
	if !p.initialized || p.generation != generation || p.settings != settings {
		changed := p.initialized && p.settings != settings
		p.initialized, p.generation, p.settings = true, generation, settings
		p.sources = map[string]*claudeAccountSchedule{"usage": {}, "enterpriseBudget": {}, "balance": {}}
		p.lastUsage = nil
		// Explicit account/configuration changes invalidate cached values and
		// cooldowns. Delayed responses remain fenced by generation below.
		if changed {
			_ = p.bridge.SetBalanceAt(generation, nil)
			_ = p.bridge.SetEnterpriseBudgetAt(generation, nil)
			_ = p.bridge.SetNativeUsageAt(generation, nil)
		}
	}
	for _, source := range []string{"balance", "enterpriseBudget", "usage"} {
		if ctx.Err() != nil {
			return
		}
		enabled := source == "balance" && supportsClaudeCredits(settings) || source == "enterpriseBudget" && supportsClaudeEnterpriseBudget(settings) || source == "usage" && settings.AuthMode == "login" && settings.BaseURL == ""
		if !enabled {
			continue
		}
		schedule := p.sources[source]
		if p.now().Before(schedule.next) {
			continue
		}
		var balance *claudeadapter.CreditBalance
		var budget *claudeadapter.EnterpriseBudget
		var usage *claudeadapter.NativeUsage
		var readErr error
		interval := 5 * time.Minute
		switch source {
		case "balance":
			interval = time.Minute
			balance, readErr = fetchClaudeCredits(ctx, settings, p.client)
		case "enterpriseBudget":
			budget, readErr = fetchClaudeEnterpriseBudget(ctx, settings, p.client)
		case "usage":
			usage, readErr = p.readUsage(ctx)
			usable := usage != nil && (len(usage.Windows) != 0 || usage.APISpend != nil && (usage.APISpend.Limit != nil || usage.APISpend.Spent != nil || usage.APISpend.Remaining != nil))
			lostBudget := false
			if usable && p.lastUsage != nil && p.lastUsage.APISpend != nil {
				old, current := p.lastUsage.APISpend, usage.APISpend
				lostBudget = current == nil || old.Limit != nil && current.Limit == nil || old.Spent != nil && current.Spent == nil || old.Remaining != nil && current.Remaining == nil
			}
			if readErr == nil && (!usable || lostBudget) {
				readErr = errors.New("Claude account usage is unavailable")
			}
		}
		latest, settingsErr := readClaudeSettings(claudeSettingsPath(p.manager.cfg))
		if ctx.Err() != nil || settingsErr != nil || latest != settings || p.bridge.BalanceGeneration() != generation {
			return // Never clear a newer account's observation with an old failure.
		}
		if readErr == nil {
			switch source {
			case "balance":
				_ = p.bridge.SetBalanceAt(generation, balance)
			case "enterpriseBudget":
				_ = p.bridge.SetEnterpriseBudgetAt(generation, budget)
			case "usage":
				_ = p.bridge.SetNativeUsageAt(generation, usage)
				p.lastUsage = usage
			}
		}
		refresh := schedule.complete(p.now(), interval, readErr, p.jitter)
		_ = p.bridge.SetAccountRefreshAt(generation, source, refresh)
	}
}
