package claudeadapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type process struct {
	server        *server
	threadID      string
	cmd           *exec.Cmd
	in            io.WriteCloser
	out           io.ReadCloser
	writeMu       sync.Mutex
	controlMu     sync.Mutex
	controls      map[string]chan controlReply
	ready         chan struct{}
	initErr       error // Published by closing ready.
	done          chan struct{}
	stopOnce      sync.Once
	interrupting  bool   // Protected by server.mu.
	providerError string // Protected by server.mu; never persisted verbatim.
	messageOwners map[string]string
	messageOrder  []string
	seenFrames    map[string]bool
	frameOrder    []string
	streamID      string
	streamItem    map[int]string
	streamBlocks  map[int]*streamContentBlock
	tools         map[string]map[string]any
	lifecycle     turnLifecycle // Protected by server.mu, including task state.
	tasks         map[string]*backgroundTask
	taskRuns      map[string]string // Run ID -> owning MACT turn, including retired runs.
	backgroundSet map[string]string // Latest native level snapshot: task ID -> run ID.
	stateEvents   bool              // Session-state support is detected once per process.
	seenResults   map[string]bool
	resultOrder   []string
}

type controlReply struct {
	data json.RawMessage
	err  error
}

func (s *server) startProcess(thread *savedThread) (*process, error) {
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	s.applyStatusSettings(thread, settings, false)
	settings = thread.effectiveSettings(settings)
	args := append([]string(nil), s.cfg.Args...)
	args = append(args, "--print", "--verbose", "--input-format=stream-json", "--output-format=stream-json", "--include-partial-messages", "--permission-prompt-tool=stdio")
	args = append(args, "--append-system-prompt="+artifactInstructions)
	if thread.NativeStarted {
		args = append(args, "--resume="+thread.ID)
	} else {
		args = append(args, "--session-id="+thread.ID)
	}
	model := settings.Model
	if model != "" {
		if err := validateModel(model); err != nil {
			return nil, err
		}
	}
	if model != "" {
		args = append(args, "--model="+model)
		thread.Model = model
	}
	if settings.ReasoningEffort != "" {
		args = append(args, "--effort="+settings.ReasoningEffort)
	}
	if settings.SettingsFile != "" {
		args = append(args, "--settings="+settings.SettingsFile)
	}
	if settings.PermissionMode != "" {
		switch settings.PermissionMode {
		case "default", "acceptEdits", "plan", "bypassPermissions", "dontAsk", "auto":
			mode := settings.PermissionMode
			if mode == "default" && s.capabilities.manualPermission {
				mode = "manual"
			}
			args = append(args, "--permission-mode="+mode)
			if mode == "bypassPermissions" {
				args = append(args, "--allow-dangerously-skip-permissions")
			}
		default:
			return nil, errors.New("Invalid Claude permission mode")
		}
	}
	env, err := childEnvironment(s.cfg.Env, settings.Env)
	if err != nil {
		return nil, err
	}
	// An explicit per-session effort outranks the inherited environment, as
	// Claude reads CLAUDE_CODE_EFFORT_LEVEL before --effort. Empty selects
	// the native/API default instead of restoring an old session setting.
	if thread.RequestedEffort != nil || settings.ReasoningEffort != "" {
		effort := settings.ReasoningEffort
		if effort == "" {
			effort = "auto"
		}
		env, _ = childEnvironment(env, map[string]string{"CLAUDE_CODE_EFFORT_LEVEL": effort})
	}
	// A result ends one native query, which can be an intermediate reply while
	// background agents continue. The opt-in idle signal ends the whole run.
	env, _ = childEnvironment(env, map[string]string{"CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": "1", "CLAUDE_CODE_BG_TASKS_REPORT_RUNNING": "1"})
	if err := s.rememberNativeRoot(thread, env); err != nil {
		return nil, err
	}
	cmd := exec.Command(s.cfg.Command, args...)
	cmd.Dir = thread.CWD
	cmd.Env = env
	cmd.Stderr = s.cfg.Stderr
	configureProcess(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("Cannot open Claude input")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, errors.New("Cannot open Claude output")
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, errors.New("Cannot start Claude Code; check the configured executable")
	}
	p := &process{server: s, threadID: thread.ID, cmd: cmd, in: in, out: out, controls: make(map[string]chan controlReply), ready: make(chan struct{}), done: make(chan struct{}), streamItem: make(map[int]string), tools: make(map[string]map[string]any)}
	s.wg.Add(2)
	go func() { defer s.wg.Done(); p.read() }()
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.StartupTimeout)
		defer cancel()
		var initialized json.RawMessage
		initialized, p.initErr = p.control(ctx, map[string]any{"subtype": "initialize", "hooks": map[string]any{}})
		if p.initErr == nil {
			s.mu.Lock()
			if !s.closed && s.runners[thread.ID] == p {
				s.observeCapabilities(initialized)
				var native struct {
					PermissionMode string `json:"current_permission_mode"`
				}
				if json.Unmarshal(initialized, &native) == nil {
					if native.PermissionMode == "manual" {
						native.PermissionMode = "default"
					}
					if permissionMode(native.PermissionMode) {
						thread.ensureStatus().PermissionMode = optionalString(native.PermissionMode)
						p.publishStatus(thread)
						if settings.PermissionMode != "" && settings.PermissionMode != native.PermissionMode {
							p.initErr = errors.New("Claude Code did not accept the requested permission mode; native settings or organization policy may disable it. Choose an allowed mode before retrying")
							// Initialization is local control traffic. Stop here,
							// before submitting a model prompt under another mode.
							p.finish(thread, "failed", p.initErr.Error())
						}
					}
				}
			}
			s.mu.Unlock()
		}
		close(p.ready)
		if p.initErr != nil {
			p.stop()
		}
	}()
	return p, nil
}

func (p *process) write(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.writeLocked(value)
}

func (p *process) writeLocked(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxMessageBytes {
		return errors.New("Claude input exceeds message limit")
	}
	select {
	case <-p.done:
		return errors.New("Claude process has exited")
	default:
	}
	_, err = p.in.Write(append(data, '\n'))
	return err
}

func (p *process) control(ctx context.Context, request any) (json.RawMessage, error) {
	id := uuid.NewString()
	reply := make(chan controlReply, 1)
	p.controlMu.Lock()
	p.controls[id] = reply
	p.controlMu.Unlock()
	defer func() { p.controlMu.Lock(); delete(p.controls, id); p.controlMu.Unlock() }()
	written := make(chan error, 1)
	go func() {
		written <- p.write(map[string]any{"type": "control_request", "request_id": id, "request": request})
	}()
	select {
	case err := <-written:
		if err != nil {
			return nil, err
		}
	case <-ctx.Done():
		p.stop()
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("Claude process exited during control write")
	}
	select {
	case response := <-reply:
		return response.data, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("Claude process exited during control request")
	}
}

func (p *process) submit(turnID string, content []map[string]any) {
	select {
	case <-p.ready:
	case <-p.done:
		return
	case <-p.server.ctx.Done():
		return
	}
	if p.initErr != nil {
		return
	}
	p.writeMu.Lock()
	p.server.mu.Lock()
	t := p.server.threads[p.threadID]
	if p.server.closed || p.server.runners[p.threadID] != p || t == nil || t.active() == nil || t.active().ID != turnID || p.interrupting {
		p.server.mu.Unlock()
		p.writeMu.Unlock()
		return
	}
	p.providerError = ""
	p.lifecycle.submitted = true
	p.server.mu.Unlock()
	err := p.writeLocked(map[string]any{"type": "user", "session_id": p.threadID, "message": map[string]any{"role": "user", "content": content}, "parent_tool_use_id": nil, "uuid": turnID, "origin": map[string]any{"kind": "human"}})
	p.writeMu.Unlock()
	if err != nil {
		p.stop()
		return
	}
	p.server.mu.Lock()
	if t = p.server.threads[p.threadID]; t != nil && p.server.runners[p.threadID] == p {
		t.NativeStarted = true
		if err := p.server.save(t); err != nil {
			p.stop()
		}
	}
	p.server.mu.Unlock()
}

func (p *process) interrupt(turnID string) {
	select {
	case <-p.ready:
	case <-p.done:
		return
	case <-p.server.ctx.Done():
		return
	}
	ctx, cancel := context.WithTimeout(p.server.ctx, 5*time.Second)
	defer cancel()
	_, err := p.control(ctx, map[string]any{"subtype": "interrupt"})
	if err != nil {
		p.stop()
		return
	}
	// Some CLI versions acknowledge cancellation without emitting a result
	// when the user interrupted during startup. Bound that state as well.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		p.server.mu.Lock()
		t := p.server.threads[p.threadID]
		active := t != nil && t.active() != nil && t.active().ID == turnID && p.server.runners[p.threadID] == p
		p.server.mu.Unlock()
		if active {
			p.stop()
		}
	case <-p.done:
	case <-p.server.ctx.Done():
	}
}

func (p *process) stop() {
	p.stopOnce.Do(func() { stopProcess(p.cmd); _ = p.in.Close(); _ = p.out.Close() })
}

func (p *process) read() {
	scanner := bufio.NewScanner(p.out)
	scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
	for scanner.Scan() {
		data := scanner.Bytes()
		if len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		var envelope struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &envelope) != nil {
			if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
				p.stop()
				break
			}
			continue
		}
		if envelope.Type == "control_response" {
			p.controlMu.Lock()
			reply := p.controls[envelope.Response.RequestID]
			p.controlMu.Unlock()
			if reply != nil {
				value := controlReply{data: append(json.RawMessage(nil), envelope.Response.Response...)}
				if envelope.Response.Subtype != "success" {
					value.err = errors.New("Claude rejected the control request")
				}
				select {
				case reply <- value:
				default:
				}
			}
			continue
		}
		p.server.mu.Lock()
		if !p.server.closed && p.server.runners[p.threadID] == p {
			p.handle(data)
		}
		p.server.mu.Unlock()
	}
	p.stop()
	_ = p.cmd.Wait() // The sole reaper; stdout is fully consumed before Wait.
	close(p.done)
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.server.runners[p.threadID] != p {
		return
	}
	delete(p.server.runners, p.threadID)
	p.resolveAll()
	if t := p.server.threads[p.threadID]; t != nil && t.active() != nil {
		status := "failed"
		if p.interrupting || p.server.closed {
			status = "interrupted"
		}
		p.finish(t, status, "Claude Code exited before the turn completed.")
	}
}

func (p *process) handle(data []byte) {
	var envelope struct {
		Type             string          `json:"type"`
		Subtype          string          `json:"subtype"`
		SessionID        string          `json:"session_id"`
		ParentToolUseID  *string         `json:"parent_tool_use_id"`
		RequestID        string          `json:"request_id"`
		Request          json.RawMessage `json:"request"`
		Event            json.RawMessage `json:"event"`
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		Error            json.RawMessage `json:"error"`
		Errors           []string        `json:"errors"`
		UUID             string          `json:"uuid"`
		Model            string          `json:"model"`
		PermissionMode   string          `json:"permissionMode"`
		UserMessageUUID  string          `json:"user_message_uuid"`
		UserMessageUUIDs []string        `json:"user_message_uuids"`
		Origin           messageOrigin   `json:"origin"`
		ToolUseResult    json.RawMessage `json:"tool_use_result"`
		Message          struct {
			ID         string          `json:"id"`
			Model      string          `json:"model"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
			Usage      json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return
	}
	t := p.server.threads[p.threadID]
	if t == nil {
		return
	}
	// Subagent text must not masquerade as the main conversation's reply.
	if envelope.ParentToolUseID != nil && *envelope.ParentToolUseID != "" {
		return
	}
	if envelope.SessionID != "" && envelope.SessionID != t.ID {
		p.stop()
		return
	}
	if envelope.Type == "assistant" || envelope.Type == "user" || envelope.Type == "stream_event" || envelope.Type == "result" {
		if !(envelope.Type == "result" && p.interrupting) && !p.acceptTurnFrame(t, envelope.UserMessageUUID, envelope.UserMessageUUIDs, envelope.Origin) {
			return
		}
	}
	// Output from a user query can win the scheduling race with submit's
	// post-write metadata commit. Record that evidence before a result can
	// publish idle state or a settings update can retire this process.
	if envelope.Type == "assistant" || envelope.Type == "user" || envelope.Type == "stream_event" {
		t.NativeStarted = true
	}
	switch envelope.Type {
	case "control_request":
		p.request(envelope.RequestID, envelope.Request)
	case "control_cancel_request":
		p.cancelPermission(envelope.RequestID)
	case "system":
		if p.handleCompaction(t, data, envelope.Subtype, envelope.UserMessageUUID, envelope.UserMessageUUIDs, envelope.Origin) {
			return
		}
		if p.handleLifecycle(t, data, envelope.Subtype) {
			return
		}
		if envelope.Subtype == "init" && envelope.SessionID != "" && envelope.SessionID != t.ID {
			p.stop()
			return
		}
		if envelope.Subtype == "init" {
			status := t.ensureStatus()
			if envelope.Model != "" && validateModel(envelope.Model) == nil {
				if !sameString(status.Model, envelope.Model) {
					status.Context.MaxTokens = nil
				}
				status.Model = optionalString(envelope.Model)
				t.Model = envelope.Model
			}
			if envelope.PermissionMode != "" {
				if envelope.PermissionMode == "manual" {
					envelope.PermissionMode = "default"
				}
				if permissionMode(envelope.PermissionMode) {
					status.PermissionMode = optionalString(envelope.PermissionMode)
				} else {
					status.PermissionMode = nil
				}
			}
			p.publishStatus(t)
		}
	case "stream_event":
		if t.active() != nil {
			p.stream(t, envelope.Event)
		}
	case "assistant":
		if t.active() == nil {
			return
		}
		if !p.acceptMessageOwner(t, envelope.Message.ID) {
			return
		}
		// SDK assistant frames contain ONE completed content block. Several
		// frames share message.id (thinking, text, tool calls), with distinct
		// wrapper UUIDs. Deduplicate frames without discarding later blocks.
		frameID := envelope.UUID
		if frameID == "" {
			sum := sha256.Sum256(envelope.Message.Content)
			frameID = envelope.Message.ID + "-" + hex.EncodeToString(sum[:])
		}
		if p.seenFrames[frameID] {
			return
		}
		if p.seenFrames == nil {
			p.seenFrames = make(map[string]bool)
		}
		p.seenFrames[frameID] = true
		p.frameOrder = append(p.frameOrder, frameID)
		if len(p.frameOrder) > 4096 {
			delete(p.seenFrames, p.frameOrder[0])
			p.frameOrder = p.frameOrder[1:]
		}
		if p.observeUsage(t, envelope.Message.Model, envelope.Message.Usage) {
			p.publishStatus(t)
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" && string(envelope.Error) != `""` {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(envelope.Message.Content, &blocks) == nil {
				for _, block := range blocks {
					if block.Type == "text" {
						if p.server.cfg.Redact != nil {
							p.providerError = boundedText(p.server.cfg.Redact(block.Text), 8192)
						}
						break
					}
				}
			}
			return
		}
		messageID := envelope.Message.ID
		if messageID == "" {
			messageID = uuid.NewString()
		}
		p.noteParentActivity()
		var content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(envelope.Message.Content, &content) != nil {
			return
		}
		hasTool := false
		for _, block := range content {
			if block.Type == "tool_use" {
				hasTool = true
			}
		}
		for i, block := range content {
			streamIndex, streamed := p.claimStreamBlock(messageID, block.Type, block.ID)
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				id := messageID + "-" + frameID + "-" + strconvIndex(i)
				if streamed && p.streamItem[streamIndex] != "" {
					id = p.streamItem[streamIndex]
				}
				phase := "final_answer"
				if hasTool || envelope.Message.StopReason == "tool_use" || envelope.Message.StopReason == "" || p.hasBackgroundWork() {
					phase = "commentary"
				}
				item := map[string]any{"id": id, "type": "agentMessage", "text": block.Text, "phase": phase}
				p.appendItem(t, item)
				p.notifyItem(t, "item/completed", item)
				p.lifecycle.lastText = item
				p.lifecycle.lastMessage = messageID
				p.lifecycle.messageTexts[messageID] = append(p.lifecycle.messageTexts[messageID], block.Text)
				p.lifecycle.textItems[messageID] = append(p.lifecycle.textItems[messageID], item)
			case "tool_use":
				if block.ID == "" {
					continue
				}
				if p.tools[block.ID] != nil {
					continue
				}
				var input any
				_ = json.Unmarshal(block.Input, &input)
				item := map[string]any{"id": block.ID, "type": "dynamicToolCall", "namespace": "claude", "tool": block.Name, "arguments": input, "status": "inProgress"}
				p.tools[block.ID] = item
				p.appendItem(t, item)
				p.notifyItem(t, "item/started", item)
			}
		}
		if err := p.server.save(t); err != nil {
			p.stop()
		}
	case "user":
		if t.active() == nil {
			return
		}
		var content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
		}
		if json.Unmarshal(envelope.Message.Content, &content) != nil {
			return
		}
		for _, block := range content {
			if block.Type == "tool_result" {
				if item := p.tools[block.ToolUseID]; item != nil {
					item["status"] = "completed"
					if block.IsError {
						item["status"] = "failed"
					}
					p.notifyItem(t, "item/completed", item)
					delete(p.tools, block.ToolUseID)
				}
			}
		}
		if len(content) == 1 && content[0].Type == "tool_result" {
			p.observeToolLifecycle(t, envelope.ToolUseResult, content[0].ToolUseID)
		}
		if err := p.server.save(t); err != nil {
			p.stop()
		}
	case "result":
		if t.active() == nil {
			return
		}
		if envelope.SessionID != "" && envelope.SessionID != t.ID {
			p.stop()
			return
		}
		if envelope.UUID != "" {
			if len(envelope.UUID) > 512 {
				p.stop()
				return
			}
			for _, turn := range t.Turns {
				if turn.NativeResultID == envelope.UUID {
					return
				}
			}
			if p.seenResults[envelope.UUID] {
				return
			}
			if p.seenResults == nil {
				p.seenResults = make(map[string]bool)
			}
			p.seenResults[envelope.UUID] = true
			p.resultOrder = append(p.resultOrder, envelope.UUID)
			if len(p.resultOrder) > 2048 {
				delete(p.seenResults, p.resultOrder[0])
				p.resultOrder = p.resultOrder[1:]
			}
			t.active().NativeResultID = envelope.UUID
		}
		p.observeResult(t, data, envelope.Subtype)
		p.observeTurnCost(t, data)
		p.publishStatus(t)
		status := "completed"
		message := ""
		if p.interrupting {
			status = "interrupted"
		} else if envelope.IsError || strings.HasPrefix(envelope.Subtype, "error") {
			status = "failed"
			message = "Claude Code could not complete this turn. Check the worker diagnostics."
			detail := envelope.Result
			if detail == "" {
				detail = strings.Join(envelope.Errors, "\n")
			}
			if detail == "" {
				detail = p.providerError
			}
			if detail != "" && p.server.cfg.Redact != nil {
				if sanitized := strings.TrimSpace(p.server.cfg.Redact(detail)); sanitized != "" {
					message = boundedText(sanitized, 4096)
				}
			}
		}
		if status == "completed" && p.lifecycle.manualCompact && envelope.Result == "" {
			if p.lifecycle.compaction != nil && p.lifecycle.compaction["status"] == "completed" {
				envelope.Result = "Context compacted."
			} else {
				status = "failed"
				message = "Claude Code did not confirm context compaction."
			}
		}
		if status == "completed" && envelope.Result != "" && !p.hasAssistantText(t.active(), envelope.Result) {
			item := map[string]any{"id": uuid.NewString(), "type": "agentMessage", "text": envelope.Result, "phase": "final_answer"}
			p.appendItem(t, item)
			p.notifyItem(t, "item/completed", item)
			p.lifecycle.lastText = item
			p.lifecycle.lastMessage = ""
		}
		p.recordResult(t, status, message, envelope.Origin)
	}
}

func (p *process) stream(t *savedThread, raw json.RawMessage) {
	var event struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if !p.acceptMessageOwner(t, event.Message.ID) {
			return
		}
		p.noteParentActivity()
		if p.observeUsage(t, event.Message.Model, event.Message.Usage) {
			p.publishStatus(t)
		}
		p.streamID = event.Message.ID
		p.streamItem = make(map[int]string)
		p.streamBlocks = make(map[int]*streamContentBlock)
	case "content_block_start":
		if p.streamBlocks == nil {
			p.streamBlocks = make(map[int]*streamContentBlock)
		}
		p.streamBlocks[event.Index] = &streamContentBlock{kind: event.Block.Type, toolID: event.Block.ID}
		if event.Block.Type == "text" {
			id := p.streamID + "-" + strconvIndex(event.Index)
			if p.streamID == "" {
				id = uuid.NewString()
			}
			p.streamItem[event.Index] = id
			p.notifyItem(t, "item/started", map[string]any{"id": id, "type": "agentMessage", "text": ""})
			if event.Block.Text != "" {
				p.textDelta(t, id, event.Block.Text)
			}
		}
	case "content_block_delta":
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			id := p.streamItem[event.Index]
			if id != "" {
				p.textDelta(t, id, event.Delta.Text)
			}
		}
	}
}

func (p *process) textDelta(t *savedThread, id, text string) {
	p.server.broadcast("item/agentMessage/delta", map[string]any{"threadId": t.ID, "turnId": t.active().ID, "itemId": id, "delta": text})
}

func (p *process) notifyItem(t *savedThread, method string, item map[string]any) {
	if t.active() != nil {
		p.server.broadcast(method, map[string]any{"threadId": t.ID, "turnId": t.active().ID, "item": item})
	}
}

func (p *process) appendItem(t *savedThread, item map[string]any) {
	if t.active() != nil {
		t.active().Items = append(t.active().Items, item)
	}
}

func (p *process) finish(t *savedThread, status, message string) {
	turn := t.active()
	if turn == nil {
		return
	}
	p.settleCompaction(t, status)
	for id, item := range p.tools {
		item["status"] = "interrupted"
		p.notifyItem(t, "item/completed", item)
		delete(p.tools, id)
	}
	for _, task := range p.tasks {
		if task.turnID == turn.ID && task.active {
			if status == "completed" && task.ambient {
				p.endTask(t, task, "completed")
				task.turnID = ""
				continue
			}
			task.active = false
			p.endTask(t, task, "interrupted")
		}
	}
	if status != "completed" {
		p.backgroundSet = nil
	}
	if status != "completed" && !p.server.closed && len(t.Queue) > 0 {
		t.Queue = nil
		item := map[string]any{"id": uuid.NewString(), "type": "agentMessage", "text": "Queued messages were cancelled because the preceding turn ended early; resend them.", "phase": "final_answer"}
		p.appendItem(t, item)
		p.notifyItem(t, "item/completed", item)
		p.server.broadcast("thread/queue/changed", map[string]any{"threadId": t.ID})
	}
	p.resolveAll()
	now := time.Now().Unix()
	turn.Status = status
	turn.CompletedAt = &now
	t.UpdatedAt = now
	p.interrupting = false
	if err := p.server.save(t); err != nil {
		turn.Status = "failed"
		message = "Claude Code could not save the conversation history."
		p.stop()
	}
	view := turn.view(false)
	view["threadId"] = t.ID
	if message != "" {
		view["error"] = map[string]any{"message": message}
	}
	p.server.broadcast("turn/completed", map[string]any{"threadId": t.ID, "turn": view})
	// Browsers can resume during a turn and observe an active thread snapshot.
	// Completion settles that turn, while the explicit status also retires the
	// snapshot marker used by the worker's idle-safe update guard.
	p.server.broadcast("thread/status/changed", map[string]any{"threadId": t.ID, "status": map[string]string{"type": "idle"}})
	if status == "completed" && turn.Status == "completed" {
		p.server.nextQueued(t)
	}
}

func (p *process) hasAssistantText(t *savedTurn, text string) bool {
	for _, item := range t.Items {
		if item["type"] == "agentMessage" && item["text"] == text {
			return true
		}
	}
	for _, blocks := range p.lifecycle.messageTexts {
		if strings.Join(blocks, "\n") == text || strings.Join(blocks, "") == text || strings.Join(blocks, "\n\n") == text {
			return true
		}
	}
	return false
}

func strconvIndex(index int) string { return strconv.Itoa(index) }
