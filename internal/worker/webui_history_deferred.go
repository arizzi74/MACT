package worker

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const webUIHistoryTimingTimeout = 15 * time.Second

type webUIHistoryTimestampItem struct {
	ItemID       string `json:"itemId"`
	TurnID       string `json:"turnId"`
	RecordedAtMS int64  `json:"recordedAtMs"`
}

type webUIHistoryTimestampReference struct {
	snapshot string
	index    int
}

// Deferred work retains fingerprints and item identities, never transcript
// text or actor state. References do not pin evicted conversation snapshots.
type webUIHistoryTimestampWork struct {
	stats   *rolloutStatsCache
	cache   *webUIHistoryCache
	manager *RuntimeManager
	client  *codexadapter.Client
	runtime protocol.Runtime
	slots   chan struct{}
	home    string
	path    string
	thread  string
	created time.Time
	items   []webUIHistoryTimestampItem
	queries []rolloutTimestampQuery
	targets []webUIHistoryTimestampReference
}

func (s *sessionActor) deferredWebUIHistoryTimestamps(client *codexadapter.Client, thread codexadapter.Thread, entries []webUIHistoryEntry, queries []rolloutTimestampQuery, targets []webUIHistoryTimestampTarget) *webUIHistoryTimestampWork {
	info, initialized := client.InitializeInfo()
	if !initialized || info.CodexHome == "" || s.agent.manager == nil {
		return nil
	}
	var wire struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(thread.Raw, &wire)
	work := &webUIHistoryTimestampWork{stats: &s.agent.manager.stats, cache: &s.agent.webHistory, manager: s.agent.manager, client: client, runtime: s.runtime, slots: s.agent.webHistoryTiming, home: info.CodexHome, path: wire.Path, thread: s.session.ThreadID, created: time.Now()}
	for i, entry := range entries {
		if entry.RecordedAtMS != nil || queries[i].Role == "" {
			continue
		}
		work.items = append(work.items, webUIHistoryTimestampItem{ItemID: queries[i].ItemID, TurnID: entry.TurnID})
		work.queries = append(work.queries, queries[i])
		work.targets = append(work.targets, webUIHistoryTimestampReference{targets[i].snapshot.id, targets[i].index})
	}
	if len(work.items) == 0 {
		return nil
	}
	return work
}

func (r *webUIRelay) scheduleHistoryTimestamps(work *webUIHistoryTimestampWork) {
	r.timestampOnce.Do(func() {
		ctx, cancel := context.WithCancel(r.ctx)
		r.timestampCancel = cancel
		r.timestampJobs = make(chan *webUIHistoryTimestampWork, 4)
		r.timestampGroup.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case work := <-r.timestampJobs:
					r.publishHistoryTimestamps(ctx, work)
				}
			}
		})
	})
	select {
	case r.timestampJobs <- work:
	default: // Optional enrichment has bounded memory even on slow storage.
	}
}

func (r *webUIRelay) stopHistoryTimestamps() {
	if r.timestampCancel != nil {
		r.timestampCancel()
	}
	r.timestampGroup.Wait()
}

func (r *webUIRelay) historyTimestampsCurrent(work *webUIHistoryTimestampWork) bool {
	if r.ctx.Err() != nil {
		return false
	}
	client, runtime, ok := work.manager.Client(work.runtime.ID)
	if !ok || client != work.client || runtime.Generation != work.runtime.Generation || runtime.State != "running" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumed && r.observerReady && r.runtime.ID == work.runtime.ID && r.runtime.Generation == work.runtime.Generation && r.session.ThreadID == work.thread
}

func (r *webUIRelay) publishHistoryTimestamps(ctx context.Context, work *webUIHistoryTimestampWork) {
	ctx, cancel := context.WithDeadline(ctx, work.created.Add(webUIHistoryTimingTimeout))
	defer cancel()
	if !r.historyTimestampsCurrent(work) {
		return
	}
	select {
	case work.slots <- struct{}{}:
		defer func() { <-work.slots }()
	case <-ctx.Done():
		return
	}
	path := work.path
	if path == "" {
		path = work.stats.historyRolloutPathContext(ctx, work.home, work.thread)
	}
	if path == "" || ctx.Err() != nil {
		return
	}
	results := work.stats.rolloutTimestampCandidatesContext(ctx, work.home, path, work.thread, work.queries)
	if ctx.Err() != nil || !r.historyTimestampsCurrent(work) {
		return
	}
	items := make([]webUIHistoryTimestampItem, 0, len(results))
	scope := webUIHistoryScope{Runtime: work.runtime.ID, Generation: work.runtime.Generation, Thread: work.thread}
	work.cache.mu.Lock()
	for i, result := range results {
		if !result.Found {
			continue
		}
		item := work.items[i]
		item.RecordedAtMS = result.Timestamp.UnixMilli()
		if item.RecordedAtMS <= 0 || item.RecordedAtMS > 253402300799999 {
			continue
		}
		items = append(items, item)
		target := work.targets[i]
		if snapshot := work.cache.snapshots[target.snapshot]; snapshot != nil && snapshot.scope == scope && target.index >= 0 && target.index < len(snapshot.recordedTimes) && snapshot.recordedTimes[target.index] == nil {
			stamp := item.RecordedAtMS
			snapshot.recordedTimes[target.index] = &stamp
		}
	}
	work.cache.mu.Unlock()
	if len(items) == 0 || ctx.Err() != nil {
		return
	}
	raw, err := json.Marshal(map[string]any{"method": "gateway/history/timestamps", "params": map[string]any{"threadId": work.thread, "items": items}})
	if err != nil || !r.historyTimestampsCurrent(work) {
		return
	}
	// The session may have been archived, deleted, moved, or removed from the
	// allowed inventory while optional indexing was pending.
	if _, _, err := r.pool.targetContext(ctx, protocol.WebUIFrame{RuntimeID: work.runtime.ID, RuntimeGeneration: work.runtime.Generation, SessionID: r.session.ID}); err != nil || ctx.Err() != nil || !r.historyTimestampsCurrent(work) {
		return
	}
	_ = r.pool.send(ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: raw})
}

func lockHistoryTimestampMutex(ctx context.Context, mu *sync.Mutex) bool {
	if ctx.Err() != nil {
		return false
	}
	if mu.TryLock() {
		return true
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
			if mu.TryLock() {
				return true
			}
		}
	}
}

type historyTimestampContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r historyTimestampContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (c *rolloutStatsCache) historyRolloutPathContext(ctx context.Context, home, threadID string) string {
	if !lockHistoryTimestampMutex(ctx, &c.mu) {
		return ""
	}
	var path string
	var newest time.Time
	for key, entry := range c.entries {
		parts := strings.Split(key, "\x00")
		if len(parts) == 4 && parts[1] == home && parts[3] == threadID && (path == "" || entry.used.After(newest)) {
			path, newest = filepath.Join(home, parts[2]), entry.used
		}
	}
	c.mu.Unlock()
	if path == "" && c.store != nil {
		path = c.store.loadRolloutTimestampPathContext(ctx, c.namespace, home, threadID)
	}
	return path
}
