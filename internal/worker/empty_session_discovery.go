package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	emptyDiscoveryLimit       = 16
	emptyDiscoveryTimeout     = 3 * time.Second
	emptyDiscoveryReadTimeout = 750 * time.Millisecond
)

// Codex omits persisted threads without a first user message from thread/list.
// Once they unload, absence from both inventories does not mean they vanished.
// Probe only known empty identities, without loading them or reading their turns.
func (m *RuntimeManager) recoverUnlistedEmptySessions(ctx context.Context, runtime protocol.Runtime, client *codexadapter.Client, existing []protocol.Session, observed, visible map[string]bool) error {
	if m.cfg.AgentKind == "claude" {
		return nil
	}
	info, initialized := client.InitializeInfo()
	if !initialized || info.CodexHome == "" {
		return nil
	}
	var candidates []protocol.Session
	for _, prior := range existing {
		if observed[prior.ThreadID] || prior.Deleted || prior.Preview != "" || prior.CWD == "" || !workspaceAllowed(prior.CWD, m.cfg.AllowedWorkspaceRoots) {
			continue
		}
		if stats := prior.Stats; stats != nil && ((stats.PromptCount != nil && *stats.PromptCount > 0) || (stats.AssistantMessageCount != nil && *stats.AssistantMessageCount > 0) || stats.LastMessageAt != nil || stats.LastMessage != "" || stats.LastMessageRole != "") {
			continue
		}
		candidates = append(candidates, prior)
		// A cap, timeout, or unavailable metadata RPC is not evidence that a
		// currently visible session should be hidden. Hidden entries still need
		// positive evidence before they can be restored.
		if !prior.Archived {
			visible[prior.ThreadID] = true
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	start := m.emptyDiscoveryStart(runtime, client, len(candidates))
	attempted := 0
	defer func() { m.advanceEmptyDiscovery(runtime, client, start+attempted, len(candidates)) }()
	ctx, cancel := context.WithTimeout(ctx, emptyDiscoveryTimeout)
	defer cancel()
	for offset := 0; offset < min(len(candidates), emptyDiscoveryLimit) && ctx.Err() == nil; offset++ {
		prior := candidates[(start+offset)%len(candidates)]
		attempted++
		readCtx, readCancel := context.WithTimeout(ctx, emptyDiscoveryReadTimeout)
		thread, err := client.ReadThread(readCtx, prior.ThreadID, false)
		readCancel()
		if err != nil {
			var rpc *codexadapter.RPCError
			if errors.As(err, &rpc) && rpc.Code == -32600 && rpc.Message == "thread not found: "+prior.ThreadID {
				delete(visible, prior.ThreadID)
				continue
			}
			if errors.Is(err, codexadapter.ErrMethodUnavailable) {
				m.markRuntimeDegraded(runtime, err)
				break
			}
			continue
		}
		delete(visible, prior.ThreadID)
		if thread.ID != prior.ThreadID || !thread.UserSession() {
			continue
		}
		cwd, err := canonicalWorkspace(thread.CWD, m.cfg.AllowedWorkspaceRoots)
		if err != nil {
			continue
		}
		previousCWD, err := canonicalWorkspace(prior.CWD, m.cfg.AllowedWorkspaceRoots)
		if err != nil || cwd != previousCWD {
			continue
		}
		persisted, err := activeEmptyRollout(info.CodexHome, thread)
		if err != nil {
			if !prior.Archived {
				visible[prior.ThreadID] = true
			}
			continue
		}
		if !persisted {
			continue
		}
		// A first prompt may have arrived after thread/list took its snapshot.
		// Keep its visible identity once active persistence is verified, without
		// replacing the actor's live turn or restoring a nonempty hidden one.
		if thread.Preview != "" {
			if !prior.Archived {
				visible[prior.ThreadID] = true
			}
			continue
		}
		candidate := sessionFromThread(runtime, thread, false)
		candidate.ID, candidate.CWD = prior.ID, prior.CWD
		if candidate.Name == "" {
			candidate.Name = prior.Name
		}
		if prior.Stats != nil {
			stats := *prior.Stats
			stats.ActiveSince = nil
			candidate.Stats = &stats
		}
		if prior.Archived {
			var changed bool
			candidate, changed, err = m.store.RestoreDiscoveredSession(runtime, prior, candidate)
			if err != nil {
				return persistenceError(err)
			}
			if !changed {
				continue
			}
		}
		visible[prior.ThreadID] = true
		if m.hooks.OnSession != nil {
			m.hooks.OnSession(runtime, candidate)
		}
	}
	return nil
}

// Rotate the small probe budget so older empty entries cannot starve newer ones.
func (m *RuntimeManager) emptyDiscoveryStart(runtime protocol.Runtime, client *codexadapter.Client, count int) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.runtimes[runtime.ID]
	if current == nil || current.client != client || current.runtime.Generation != runtime.Generation {
		return 0
	}
	return current.emptyProbeNext % count
}

func (m *RuntimeManager) advanceEmptyDiscovery(runtime protocol.Runtime, client *codexadapter.Client, next, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.runtimes[runtime.ID]; current != nil && current.client == client && current.runtime.Generation == runtime.Generation {
		current.emptyProbeNext = next % count
	}
}

// The active sessions directory is positive persistence evidence. In particular,
// thread/read can also return archived threads, whose paths must never restore
// inventory. Check just the bounded identity header, never the conversation.
func activeEmptyRollout(home string, thread codexadapter.Thread) (bool, error) {
	var wire struct {
		Path string `json:"path"`
	}
	if !filepath.IsAbs(home) || json.Unmarshal(thread.Raw, &wire) != nil || !filepath.IsAbs(wire.Path) || filepath.Clean(wire.Path) != wire.Path {
		return false, nil
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return emptyRolloutFileError(err)
	}
	rel, err := filepath.Rel(filepath.Join(home, "sessions"), wire.Path)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !strings.HasPrefix(filepath.Base(rel), "rollout-") || !strings.HasSuffix(filepath.Base(rel), "-"+thread.ID+".jsonl") {
		return false, nil
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return emptyRolloutFileError(err)
	}
	defer root.Close()
	directory, err := root.Lstat("sessions")
	if err != nil {
		return emptyRolloutFileError(err)
	}
	if !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	resolved, err := filepath.EvalSymlinks(wire.Path)
	if err != nil {
		return emptyRolloutFileError(err)
	}
	resolvedRel, err := filepath.Rel(filepath.Join(home, "sessions"), resolved)
	if err != nil || filepath.IsAbs(resolvedRel) || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	sessions, err := root.OpenRoot("sessions")
	if err != nil {
		return emptyRolloutFileError(err)
	}
	defer sessions.Close()
	openedDirectory, err := sessions.Stat(".")
	if err != nil {
		return emptyRolloutFileError(err)
	}
	if !os.SameFile(directory, openedDirectory) {
		return false, nil
	}
	stat, err := sessions.Stat(rel)
	if err != nil {
		return emptyRolloutFileError(err)
	}
	if !stat.Mode().IsRegular() {
		return false, nil
	}
	file, err := sessions.Open(rel)
	if err != nil {
		return emptyRolloutFileError(err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return emptyRolloutFileError(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(stat, opened) {
		return false, nil
	}
	line, err := bufio.NewReader(io.LimitReader(file, statsLineBytes)).ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return emptyRolloutFileError(err)
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != thread.ID {
		return false, nil
	}
	current, err := sessions.Stat(rel)
	if err != nil {
		return emptyRolloutFileError(err)
	}
	return os.SameFile(opened, current), nil
}

func emptyRolloutFileError(err error) (bool, error) {
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	// Permission and temporary I/O failures cannot establish absence.
	return false, err
}
