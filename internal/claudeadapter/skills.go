package claudeadapter

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Native initialize.commands contains skills AND control commands. Keep the
// latter on MACT's typed routes: /clear or /model inside a skill would silently
// invalidate the adapter's conversation identity or saved preferences.
func nativeSkillControl(name string) bool {
	switch name {
	case "agents", "auto-mode-setup", "autocompact", "clear", "reset", "new", "color", "compact", "config", "output-style", "context", "effort", "fast", "focus", "heapdump", "init", "mcp", "import", "model", "permissions", "reload-plugins", "reload-skills", "rename", "usage-credits", "extra-usage", "usage", "cost", "stats", "insights", "recap", "goal", "design-consent", "design-revoke", "list-agents", "team-onboarding", "workflow-launch-exec":
		return true
	}
	return strings.HasPrefix(name, "__")
}

func decodeNativeSkills(raw json.RawMessage) ([]protocol.Skill, bool) {
	var native struct {
		Commands *[]struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			ArgumentHint string `json:"argumentHint"`
			Builtin      bool   `json:"builtin"`
		} `json:"commands"`
	}
	if json.Unmarshal(raw, &native) != nil || native.Commands == nil {
		return nil, false
	}
	entries := []protocol.Skill{}
	seen := map[string]bool{}
	for _, c := range *native.Commands {
		if !protocol.ValidSkillName(c.Name) || seen[c.Name] || nativeSkillControl(c.Name) {
			continue
		}
		seen[c.Name] = true
		entries = append(entries, protocol.Skill{Name: c.Name, Description: skillText(c.Description, 500), ArgumentHint: skillText(c.ArgumentHint, 256)})
		if len(entries) == 1000 {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, true
}

func skillText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) > n {
		return string(runes[:n])
	}
	return string(runes)
}

func hasNativeSkill(entries []protocol.Skill, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

// Called under server.mu. Initialization uses the exact session CWD/settings,
// submits no user prompt, and is awaited outside the lock so native output and
// unrelated sessions continue. The process identity is checked after the wait.
func (s *server) skillRPC(peer *peer, msg rpcMessage, params params) {
	t := s.threads[params.ThreadID]
	if !canonicalID(params.ThreadID) || t == nil || s.deletions[params.ThreadID] != nil {
		rpcError(peer, msg.ID, -32602, "Claude session was not found or is being deleted")
		return
	}
	if msg.Method == "thread/skill/start" && (!protocol.ValidSkillName(params.Name) || len(params.Args) > 16<<10 || !utf8.ValidString(params.Args) || strings.ContainsRune(params.Args, 0)) {
		rpcError(peer, msg.ID, -32602, "Invalid skill name or arguments. Use /skill to list available skills")
		return
	}
	p := s.runners[t.ID]
	if p == nil {
		var err error
		p, err = s.startProcess(t)
		if err != nil {
			rpcError(peer, msg.ID, -32602, err.Error())
			return
		}
		s.runners[t.ID] = p
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-p.ready:
		case <-s.ctx.Done():
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		if s.runners[t.ID] != p || s.threads[t.ID] != t || s.deletions[t.ID] != nil || p.initErr != nil {
			rpcError(peer, msg.ID, -32602, "Claude session changed during skill discovery. Retry /skill")
			return
		}
		if !p.skillsKnown {
			rpcError(peer, msg.ID, -32602, "This Claude runtime does not advertise skills. Update Claude Code and retry")
			return
		}
		if msg.Method == "thread/skills/list" {
			rpcResult(peer, msg.ID, map[string]any{"skills": p.skills})
			return
		}
		if !hasNativeSkill(p.skills, params.Name) {
			rpcError(peer, msg.ID, -32602, "Unknown or unavailable skill. Use /skill to list skills for this session. Nothing was sent to Claude")
			return
		}
		text := "/" + params.Name
		if params.Args != "" {
			text += " " + params.Args
		}
		content := []map[string]any{{"type": "text", "text": text}}
		if t.active() != nil {
			if !params.QueueIfBusy {
				rpcError(peer, msg.ID, -32602, "Claude session already has an active turn")
				return
			}
			if len(t.Queue) >= 20 {
				rpcError(peer, msg.ID, -32602, "Claude follow-up queue is full")
				return
			}
			entry := &queuedInput{ID: uuid.NewString(), Content: content, SkillName: params.Name}
			t.Queue = append(t.Queue, entry)
			encoded, _ := json.Marshal(t.Queue)
			if len(encoded) > 8<<20 {
				t.Queue = t.Queue[:len(t.Queue)-1]
				rpcError(peer, msg.ID, -32602, "Claude follow-up queue exceeds the size limit")
				return
			}
			if err := s.save(t); err != nil {
				t.Queue = t.Queue[:len(t.Queue)-1]
				rpcError(peer, msg.ID, -32602, err.Error())
				return
			}
			rpcResult(peer, msg.ID, map[string]any{"queued": true})
			s.broadcast("thread/queue/changed", map[string]any{"threadId": t.ID})
			return
		}
		if len(t.Queue) > 0 {
			rpcError(peer, msg.ID, -32602, "Clear the paused queued messages before invoking a skill")
			return
		}
		turn, proc, userItem, err := s.beginTurn(t, content)
		if err != nil {
			rpcError(peer, msg.ID, -32602, err.Error())
			return
		}
		rpcResult(peer, msg.ID, map[string]any{"turn": map[string]any{"id": turn.ID, "threadId": t.ID, "status": "inProgress"}})
		s.admitTurn(t, turn, proc, userItem, content)
	}()
}

var errQueuedSkillUnavailable = errors.New("The queued skill is no longer available. Nothing was sent to Claude")
