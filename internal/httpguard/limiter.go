package httpguard

import (
	"sync"
	"time"
)

// Limiter admits per-client and global budgets over a rolling window. Admissions
// cannot double at a fixed-window boundary. Rejected requests allocate no state;
// the event queue and client map are bounded by the global admission budget.
type Limiter struct {
	mu                       sync.Mutex
	window                   time.Duration
	clientLimit, globalLimit int
	last                     time.Time
	events                   []admission
	clients                  map[string]int
}

type admission struct {
	client string
	at     time.Time
}

func NewLimiter(clientLimit, globalLimit int, window time.Duration) *Limiter {
	return &Limiter{clientLimit: clientLimit, globalLimit: globalLimit, window: window}
}

func (l *Limiter) Allow(client string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window <= 0 || l.clientLimit <= 0 || l.globalLimit <= 0 {
		return false
	}
	// A wall-clock correction must not release an already consumed budget.
	if now.Before(l.last) {
		now = l.last
	}
	l.last = now
	if l.clients == nil {
		l.clients = make(map[string]int)
	}
	cutoff := now.Add(-l.window)
	expired := 0
	for expired < len(l.events) && !l.events[expired].at.After(cutoff) {
		client := l.events[expired].client
		l.clients[client]--
		if l.clients[client] == 0 {
			delete(l.clients, client)
		}
		expired++
	}
	if expired > 0 {
		remaining := copy(l.events, l.events[expired:])
		clear(l.events[remaining:])
		l.events = l.events[:remaining]
	}
	if len(l.events) >= l.globalLimit || l.clients[client] >= l.clientLimit {
		return false
	}
	l.clients[client]++
	l.events = append(l.events, admission{client: client, at: now})
	return true
}
