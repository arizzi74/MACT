package claudeadapter

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const sessionCostInterval = 15 * time.Second

// get_session_cost is a local, read-only Claude control, equivalent to its
// cost summary. Unlike get_usage it does not fetch the account billing API.
// Only its numeric first line is accepted; model names and other returned
// text never enter the public status or saved conversation.
var sessionCostLine = regexp.MustCompile(`^Total cost:[ \t]+\$([0-9]+(?:\.[0-9]{1,8})?)(?:[ \t]+\([^\r\n]*\))?$`)

func decodeSessionCost(raw json.RawMessage) (total, rounding float64, ok bool) {
	var reply struct {
		Text string `json:"text"`
	}
	if len(raw) > 64<<10 || json.Unmarshal(raw, &reply) != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(reply.Text, "\n")
	match := sessionCostLine.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
	if match == nil {
		return 0, 0, false
	}
	total, err := strconv.ParseFloat(match[1], 64)
	if err != nil || !validUSD(total) {
		return 0, 0, false
	}
	_, fraction, _ := strings.Cut(match[1], ".")
	return total, math.Pow10(-len(fraction))/2 + 1e-9, true
}

// A process has one observer, bounded requests, and no request queue. Failed
// or unsupported controls retain the prior observation and never stop a turn.
func (p *process) pollSessionCost() {
	select {
	case <-p.ready:
		if p.initErr != nil {
			return
		}
	case <-p.done:
		return
	case <-p.server.ctx.Done():
		return
	}
	ticker := time.NewTicker(sessionCostInterval)
	defer ticker.Stop()
	var next time.Time
	for {
		select {
		case <-p.costWake:
		case <-ticker.C:
		case <-p.done:
			return
		case <-p.server.ctx.Done():
			return
		}
		if time.Now().Before(next) {
			continue
		}
		next = time.Now().Add(sessionCostInterval)
		p.refreshSessionCost()
	}
}

func (p *process) refreshSessionCost() {
	s := p.server
	s.mu.Lock()
	t := s.threads[p.threadID]
	if s.closed || t == nil || s.runners[p.threadID] != p || s.deleting[p.threadID] || s.deletions[p.threadID] != nil || !p.costReady || p.interrupting || t.active() == nil {
		s.mu.Unlock()
		return
	}
	revision := p.costRevision
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	raw, err := p.controlRequest(ctx, map[string]any{"subtype": "get_session_cost"}, false)
	if err != nil {
		return
	}
	total, rounding, ok := decodeSessionCost(raw)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t = s.threads[p.threadID]
	// A late, rounded summary cannot overwrite a newer precise result or
	// another process's observations after retirement, deletion or restart.
	if s.closed || t == nil || s.runners[p.threadID] != p || s.deleting[p.threadID] || s.deletions[p.threadID] != nil || p.interrupting || p.costRevision != revision || t.active() == nil {
		return
	}
	cost := &t.ensureStatus().Cost
	if cost.SessionUSD != nil && total+rounding < *cost.SessionUSD {
		cost.Incomplete = true
	}
	cost.ReportedSessionUSD = &total
	cost.ObservedAt = time.Now().UTC().Unix()
	p.publishStatus(t)
}
