package claudeadapter

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

type params struct {
	ThreadID        string  `json:"threadId"`
	TurnID          string  `json:"turnId"`
	ExpectedTurnID  string  `json:"expectedTurnId"`
	CWD             string  `json:"cwd"`
	Model           string  `json:"model"`
	Effort          *string `json:"effort"`
	ReasoningEffort *string `json:"reasoningEffort"`
	PermissionMode  *string `json:"permissionMode"`
	Config          struct {
		ReasoningEffort *string `json:"model_reasoning_effort"`
	} `json:"config"`
	Name           string `json:"name"`
	ApprovalPolicy string `json:"approvalPolicy"`
	Sandbox        string `json:"sandbox"`
	Personality    string `json:"personality"`
	Cursor         string `json:"cursor"`
	Limit          int    `json:"limit"`
	SortDirection  string `json:"sortDirection"`
	ItemsView      string `json:"itemsView"`
	IncludeTurns   bool   `json:"includeTurns"`
	Input          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URL  string `json:"url"`
	} `json:"input"`
}

func (s *server) receive(peer *peer, data []byte) {
	var msg rpcMessage
	if len(data) > maxMessageBytes || json.Unmarshal(data, &msg) != nil {
		rpcError(peer, nil, -32700, "Invalid JSON-RPC message")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if msg.Method == "" {
		if len(msg.ID) != 0 {
			s.replyPermission(peer, msg)
		}
		return
	}
	if msg.Method == "initialized" {
		return
	}
	if len(msg.ID) == 0 {
		return
	}
	if msg.Method == "initialize" {
		if peer.initialized {
			rpcError(peer, msg.ID, -32600, "Connection is already initialized")
			return
		}
		rpcResult(peer, msg.ID, initializeInfo(s))
		peer.initialized = true
		return
	}
	if !peer.initialized {
		rpcError(peer, msg.ID, -32600, "Connection is not initialized")
		return
	}
	if !methodSupported(msg.Method) {
		message := "This capability is not supported by Claude Code"
		if msg.Method == "turn/steer" {
			message = "Claude Code queues follow-up messages; send again when the current turn finishes"
		}
		rpcError(peer, msg.ID, -32601, message)
		return
	}
	var p params
	if len(msg.Params) != 0 && json.Unmarshal(msg.Params, &p) != nil {
		rpcError(peer, msg.ID, -32602, "Invalid request parameters")
		return
	}
	if msg.Method == "thread/delete" {
		record, proc, err := s.beginDeletion(p.ThreadID)
		if err != nil {
			code := -32602
			if record != nil || s.deletions[p.ThreadID] != nil {
				code = -32010
			}
			rpcError(peer, msg.ID, code, err.Error())
			return
		}
		if record.Done {
			rpcResult(peer, msg.ID, map[string]any{})
			s.broadcast("thread/deleted", map[string]any{"threadId": record.ThreadID})
			return
		}
		s.wg.Add(1)
		go s.runDeletion(peer, append(json.RawMessage(nil), msg.ID...), record, proc)
		return
	}
	result, err := s.handle(peer, msg, p)
	if err != nil {
		rpcError(peer, msg.ID, -32602, err.Error())
		return
	}
	if result != nil {
		rpcResult(peer, msg.ID, result)
	}
}

func (s *server) handle(peer *peer, msg rpcMessage, p params) (any, error) {
	switch msg.Method {
	case "config/read":
		settings, err := s.settings()
		if err != nil {
			return nil, err
		}
		model := settings.Model
		mode := settings.PermissionMode
		if !permissionMode(mode) {
			mode = ""
		}
		return map[string]any{"config": map[string]any{"model": optionalString(model), "model_provider": "anthropic", "permissionMode": optionalString(mode), "model_reasoning_effort": optionalString(settings.ReasoningEffort), "supportedPermissionModes": s.supportedPermissionModes()}}, nil
	case "model/list":
		settings, err := s.settings()
		if err != nil {
			return nil, err
		}
		return s.modelList(settings), nil
	case "thread/list", "thread/loaded/list":
		threads := sortedThreads(s.threads)
		data := make([]any, 0, len(threads))
		for _, t := range threads {
			if s.deletions[t.ID] != nil {
				continue
			}
			if p.CWD != "" && filepath.Clean(p.CWD) != t.CWD {
				continue
			}
			if msg.Method == "thread/loaded/list" {
				if s.runners[t.ID] != nil {
					data = append(data, t.ID)
				}
			} else {
				data = append(data, s.threadMetadata(t, false))
			}
		}
		start, end, next, err := pageBounds(p.Cursor, p.Limit, len(data))
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": data[start:end], "nextCursor": next}, nil
	case "thread/start":
		if len(s.deleting) > 0 {
			return nil, deletionError("a session deletion is in progress; wait before creating a session.")
		}
		if p.Sandbox != "" || p.Personality != "" {
			return nil, errors.New("Claude Code does not support Codex sandbox or personality settings")
		}
		if p.ApprovalPolicy != "" && p.ApprovalPolicy != "on-request" {
			return nil, errors.New("Claude Code uses its own permission configuration")
		}
		cwd := p.CWD
		if cwd == "" {
			cwd = s.cfg.WorkingDirectory
		}
		if !filepath.IsAbs(cwd) {
			return nil, errors.New("Workspace must be absolute")
		}
		cwd, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return nil, errors.New("Workspace is unavailable")
		}
		if s.deletionProtectsWorkspace(cwd) {
			return nil, deletionError("this workspace is inside session artifacts awaiting deletion.")
		}
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return nil, errors.New("Workspace is not a directory")
		}
		settings, err := s.settings()
		if err != nil {
			return nil, err
		}
		model := p.Model
		if model == "" {
			model = settings.Model
		}
		if model != "" {
			if err := validateModel(model); err != nil {
				return nil, err
			}
		}
		now := time.Now().Unix()
		t := &savedThread{ID: uuid.NewString(), CWD: cwd, Model: model, CreatedAt: now, UpdatedAt: now, Turns: []*savedTurn{}}
		t.RequestedModel = p.Model
		t.RequestedEffort = p.Config.ReasoningEffort
		if p.ReasoningEffort != nil {
			t.RequestedEffort = p.ReasoningEffort
		}
		if p.Effort != nil {
			t.RequestedEffort = p.Effort
		}
		if t.RequestedEffort != nil && *t.RequestedEffort != "" && !reasoningEffort(*t.RequestedEffort) {
			return nil, errors.New("Invalid Claude effort level")
		}
		if p.PermissionMode != nil {
			if *p.PermissionMode != "" && !s.supportsPermissionMode(*p.PermissionMode) {
				return nil, errors.New("This permission mode is not advertised by the installed Claude runtime")
			}
			t.RequestedPermissionMode = *p.PermissionMode
		}
		if t.RequestedEffort != nil && *t.RequestedEffort != "" {
			selected := t.effectiveSettings(settings).Model
			if selected == "" {
				selected = "default"
			}
			if !s.supportsEffort(selected, *t.RequestedEffort) {
				return nil, errors.New("This effort level is not advertised for the selected Claude model")
			}
		}
		s.applyStatusSettings(t, settings, true)
		if err := s.save(t); err != nil {
			return nil, err
		}
		s.threads[t.ID] = t
		rpcResult(peer, msg.ID, s.threadResult(t, false))
		s.broadcast("thread/started", map[string]any{"thread": s.threadMetadata(t, false)})
		return nil, nil
	}
	if !canonicalID(p.ThreadID) {
		return nil, errors.New("Invalid Claude session ID")
	}
	if record := s.deletions[p.ThreadID]; record != nil {
		if record.Done {
			return nil, errors.New("Claude session was not found")
		}
		if message := s.deletionErrors[p.ThreadID]; message != "" {
			return nil, errors.New(message)
		}
		return nil, deletionError("this session has an incomplete deletion.")
	}
	t := s.threads[p.ThreadID]
	if t == nil {
		return nil, errors.New("Claude session was not found")
	}
	switch msg.Method {
	case "thread/history/recover":
		return s.recoverNativeHistory(t.ID, p.TurnID)
	case "thread/read", "thread/resume":
		if msg.Method == "thread/resume" {
			if p.CWD != "" {
				cwd, err := filepath.EvalSymlinks(p.CWD)
				if err != nil || cwd != t.CWD {
					return nil, errors.New("Resume workspace does not match the saved session")
				}
			}
			if p.Model != "" && p.Model != t.Model && p.Model != t.StatusConfigModel {
				return nil, errors.New("Changing a saved Claude model through resume is not supported")
			}
			if p.Effort != nil || p.ReasoningEffort != nil || p.Config.ReasoningEffort != nil || p.PermissionMode != nil {
				return nil, errors.New("Use thread/settings/update to change saved Claude session preferences")
			}
			if p.Sandbox != "" || p.Personality != "" || (p.ApprovalPolicy != "" && p.ApprovalPolicy != "on-request") {
				return nil, errors.New("Claude Code does not support these Codex thread settings")
			}
		}
		return s.threadResult(t, p.IncludeTurns), nil
	case "thread/settings/update":
		if err := s.updateThreadSettings(t, msg.Params); err != nil {
			return nil, err
		}
		rpcResult(peer, msg.ID, map[string]any{})
		s.broadcastSettings(t)
		s.broadcastStatus(t)
		return nil, nil
	case "thread/status/read":
		return map[string]any{"threadId": t.ID, "claudeStatus": s.statusSnapshot(t), "threadSettings": t.settingsSnapshot()}, nil
	case "thread/name/set":
		if len(p.Name) > 512 {
			return nil, errors.New("Session name is too long")
		}
		old := t.Name
		t.Name = p.Name
		t.UpdatedAt = time.Now().Unix()
		if err := s.save(t); err != nil {
			t.Name = old
			return nil, err
		}
		rpcResult(peer, msg.ID, map[string]any{})
		s.broadcast("thread/name/updated", map[string]any{"threadId": t.ID, "threadName": t.Name})
		return nil, nil
	case "thread/turns/list":
		turns := make([]any, 0, len(t.Turns))
		for i := range t.Turns {
			index := i
			if p.SortDirection != "asc" {
				index = len(t.Turns) - 1 - i
			}
			turns = append(turns, t.Turns[index].view(p.ItemsView != "notLoaded"))
		}
		start, end, next, err := pageBounds(p.Cursor, p.Limit, len(turns))
		if err != nil {
			return nil, err
		}
		var backwards any
		if start > 0 {
			backwards = strconv.Itoa(len(turns) - start - 1)
		}
		return map[string]any{"data": turns[start:end], "nextCursor": next, "backwardsCursor": backwards}, nil
	case "turn/start":
		if t.active() != nil {
			return nil, errors.New("Claude session already has an active turn")
		}
		content, err := convertInput(p)
		if err != nil {
			return nil, err
		}
		if len(t.Queue) > 0 {
			return nil, errors.New("Clear the paused queued messages before starting another turn")
		}
		turn, proc, userItem, err := s.beginTurn(t, content)
		if err != nil {
			return nil, err
		}
		rpcResult(peer, msg.ID, map[string]any{"turn": map[string]any{"id": turn.ID, "threadId": t.ID, "status": "inProgress"}})
		s.admitTurn(t, turn, proc, userItem, content)
		return nil, nil
	case "turn/steer":
		turn := t.active()
		if turn == nil || p.ExpectedTurnID != turn.ID {
			return nil, errors.New("Turn is no longer the active Claude turn")
		}
		if len(t.Queue) >= 20 {
			return nil, errors.New("Claude follow-up queue is full")
		}
		content, err := convertInput(p)
		if err != nil {
			return nil, err
		}
		queued := &queuedInput{ID: uuid.NewString(), Content: content}
		t.Queue = append(t.Queue, queued)
		encoded, _ := json.Marshal(t.Queue)
		if len(encoded) > 8<<20 {
			t.Queue = t.Queue[:len(t.Queue)-1]
			return nil, errors.New("Claude follow-up queue exceeds the size limit")
		}
		if err := s.save(t); err != nil {
			t.Queue = t.Queue[:len(t.Queue)-1]
			return nil, err
		}
		rpcResult(peer, msg.ID, map[string]any{"turnId": turn.ID, "queued": true, "queuedInputId": queued.ID})
		s.broadcast("thread/queue/changed", map[string]any{"threadId": t.ID})
		return nil, nil
	case "thread/queue/list":
		data := make([]any, 0, len(t.Queue))
		for _, entry := range t.Queue {
			data = append(data, map[string]any{"id": entry.ID, "input": historyInput(entry.Content)})
		}
		start, end, next, err := pageBounds(p.Cursor, p.Limit, len(data))
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": data[start:end], "nextCursor": next}, nil
	case "thread/queue/clear":
		old := t.Queue
		t.Queue = nil
		if err := s.save(t); err != nil {
			t.Queue = old
			return nil, err
		}
		rpcResult(peer, msg.ID, map[string]any{})
		s.broadcast("thread/queue/changed", map[string]any{"threadId": t.ID})
		return nil, nil
	case "turn/interrupt":
		turn := t.active()
		if turn == nil || p.TurnID != turn.ID {
			return nil, errors.New("Turn is no longer the active Claude turn")
		}
		proc := s.runners[t.ID]
		if proc == nil {
			return nil, errors.New("Claude process is unavailable")
		}
		proc.interrupting = true
		t.Queue = nil
		if err := s.save(t); err != nil {
			return nil, err
		}
		rpcResult(peer, msg.ID, map[string]any{})
		go proc.interrupt(turn.ID)
		return nil, nil
	}
	return nil, errors.New("Unsupported Claude capability")
}

func pageBounds(cursor string, limit, total int) (int, int, any, error) {
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil || start < 0 || start > total {
			return 0, 0, nil, errors.New("Invalid history cursor")
		}
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	end := min(total, start+limit)
	var next any
	if end < total {
		next = strconv.Itoa(end)
	}
	return start, end, next, nil
}

func validateModel(model string) error {
	if !protocol.ModelMenuToken(model) || strings.HasPrefix(model, "-") {
		return errors.New("Invalid Claude model")
	}
	return nil
}

func (s *server) settings() (LaunchSettings, error) {
	settings := LaunchSettings{SettingsFile: s.cfg.SettingsFile}
	if s.cfg.LoadSettings != nil {
		loaded, err := s.cfg.LoadSettings()
		if err != nil {
			return LaunchSettings{}, errors.New("Claude runtime settings are unavailable")
		}
		settings = loaded
		if settings.SettingsFile == "" {
			settings.SettingsFile = s.cfg.SettingsFile
		}
	}
	if settings.Model != "" && validateModel(settings.Model) != nil {
		return LaunchSettings{}, errors.New("Invalid Claude model")
	}
	if settings.ReasoningEffort != "" && !reasoningEffort(settings.ReasoningEffort) {
		return LaunchSettings{}, errors.New("Invalid Claude effort level")
	}
	if settings.PermissionMode != "" && !permissionMode(settings.PermissionMode) {
		return LaunchSettings{}, errors.New("Invalid Claude permission mode")
	}
	if err := validateSettingsFile(settings.SettingsFile); err != nil {
		return LaunchSettings{}, err
	}
	return settings, nil
}

func convertInput(p params) ([]map[string]any, error) {
	if len(p.Input) == 0 {
		return nil, errors.New("A prompt or image is required")
	}
	content := make([]map[string]any, 0, len(p.Input))
	for _, block := range p.Input {
		switch block.Type {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		case "image":
			if !strings.HasPrefix(block.URL, "data:") {
				return nil, errors.New("Only inline images are supported")
			}
			parts := strings.SplitN(strings.TrimPrefix(block.URL, "data:"), ";base64,", 2)
			if len(parts) != 2 {
				return nil, errors.New("Invalid inline image")
			}
			switch parts[0] {
			case "image/png", "image/jpeg", "image/gif", "image/webp":
			default:
				return nil, errors.New("Unsupported image format")
			}
			decoded, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil || len(decoded) == 0 || len(decoded) > 20<<20 {
				return nil, errors.New("Invalid inline image")
			}
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}})
		default:
			return nil, errors.New("Unsupported prompt content")
		}
	}
	return content, nil
}

func historyInput(content []map[string]any) []map[string]any {
	history := make([]map[string]any, 0, len(content))
	for _, block := range content {
		if block["type"] == "text" {
			history = append(history, block)
		} else {
			history = append(history, map[string]any{"type": "image"})
		}
	}
	return history
}
