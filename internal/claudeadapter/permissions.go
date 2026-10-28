package claudeadapter

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

type permission struct {
	ID        string
	NativeID  string
	Process   *process
	ToolID    string
	ToolName  string
	Input     map[string]any
	Questions map[string]string // Normalized ID -> exact native question text.
}

func (p *process) controlSuccess(id string, response any) {
	go func() {
		if err := p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": response}}); err != nil {
			p.stop()
		}
	}()
}

func (p *process) controlError(id string) {
	go func() {
		if err := p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "Unsupported Claude control request"}}); err != nil {
			p.stop()
		}
	}()
}

func (p *process) request(id string, raw json.RawMessage) {
	var request struct {
		Subtype    string         `json:"subtype"`
		CallbackID string         `json:"callback_id"`
		ToolName   string         `json:"tool_name"`
		ToolUseID  string         `json:"tool_use_id"`
		Input      map[string]any `json:"input"`
	}
	if id == "" || json.Unmarshal(raw, &request) != nil {
		return
	}
	if request.Subtype == "hook_callback" && request.CallbackID == "mact_background_guard" {
		input, _ := request.Input["tool_input"].(map[string]any)
		if background, _ := input["run_in_background"].(bool); background {
			p.controlSuccess(id, map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "Background tools are unavailable in this worker. Run the tool in the foreground."}})
		} else {
			p.controlSuccess(id, map[string]any{})
		}
		return
	}
	if request.Subtype != "can_use_tool" {
		p.controlError(id)
		return
	}
	t := p.server.threads[p.threadID]
	if t == nil || t.active() == nil || p.interrupting {
		p.controlSuccess(id, map[string]any{"behavior": "deny", "message": "The turn is no longer active", "interrupt": true})
		return
	}
	if background, _ := request.Input["run_in_background"].(bool); background {
		p.controlSuccess(id, map[string]any{"behavior": "deny", "message": "Run the tool in the foreground; background tools are unavailable in this worker."})
		return
	}
	for _, pending := range p.server.pending {
		if pending.Process == p && pending.NativeID == id {
			return
		}
	}
	pending := &permission{ID: uuid.NewString(), NativeID: id, Process: p, ToolID: request.ToolUseID, ToolName: request.ToolName, Input: request.Input}
	if pending.ToolID == "" {
		pending.ToolID = uuid.NewString()
	}
	method := "item/commandExecution/requestApproval"
	params := map[string]any{"threadId": t.ID, "turnId": t.active().ID, "itemId": pending.ToolID, "cwd": t.CWD, "availableDecisions": []string{"accept", "decline", "cancel"}, "reason": "Claude Code requests permission to use " + request.ToolName}
	if request.ToolName == "AskUserQuestion" {
		method = "item/tool/requestUserInput"
		pending.Questions = make(map[string]string)
		questions, ok := request.Input["questions"].([]any)
		if !ok || len(questions) == 0 {
			p.controlSuccess(id, map[string]any{"behavior": "deny", "message": "Question format is not supported by this worker"})
			return
		}
		normalized := make([]map[string]any, 0, len(questions))
		for _, value := range questions {
			question, ok := value.(map[string]any)
			if !ok {
				continue
			}
			text, _ := question["question"].(string)
			if text == "" {
				continue
			}
			questionID := uuid.NewString()
			pending.Questions[questionID] = text
			options := make([]map[string]any, 0)
			if choices, ok := question["options"].([]any); ok {
				for _, choice := range choices {
					if option, ok := choice.(map[string]any); ok {
						options = append(options, map[string]any{"label": option["label"], "description": option["description"]})
					}
				}
			}
			normalized = append(normalized, map[string]any{"id": questionID, "header": question["header"], "question": text, "isOther": true, "isSecret": false, "options": options})
		}
		if len(normalized) == 0 {
			p.controlSuccess(id, map[string]any{"behavior": "deny", "message": "No supported question was provided"})
			return
		}
		params["questions"] = normalized
	} else {
		command, _ := request.Input["command"].(string)
		if command == "" {
			command = request.ToolName
		}
		if p.server.cfg.Redact != nil {
			command = p.server.cfg.Redact(command)
		}
		params["command"] = boundedText(command, 8192)
		params["toolName"] = request.ToolName
		params["toolInput"] = request.Input
		if encoded, err := json.MarshalIndent(request.Input, "", "  "); err == nil {
			preview := string(encoded)
			if p.server.cfg.Redact != nil {
				preview = p.server.cfg.Redact(preview)
			}
			params["reason"] = "Claude Code requests permission to use " + request.ToolName + " with input:\n" + boundedText(preview, 8192)
		}
		if request.ToolName == "Edit" || request.ToolName == "Write" || request.ToolName == "NotebookEdit" {
			method = "item/fileChange/requestApproval"
		}
	}
	p.server.pending[pending.ID] = pending
	for peer := range p.server.peers {
		if peer.initialized {
			peer.send(map[string]any{"id": pending.ID, "method": method, "params": params})
		}
	}
}

func boundedText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "\n[truncated]"
	}
	return value
}

func (s *server) replyPermission(peer *peer, msg rpcMessage) {
	var id string
	if json.Unmarshal(msg.ID, &id) != nil {
		return
	}
	pending := s.pending[id]
	if pending == nil {
		rpcError(peer, msg.ID, -32600, "The Claude request is no longer pending")
		return
	}
	response := map[string]any{"behavior": "deny", "message": "User declined the request"}
	if len(msg.Error) == 0 || string(msg.Error) == "null" {
		if len(pending.Questions) != 0 {
			var answer struct {
				Answers map[string]struct {
					Answers []string `json:"answers"`
				} `json:"answers"`
			}
			if json.Unmarshal(msg.Result, &answer) != nil || len(answer.Answers) != len(pending.Questions) {
				rpcError(peer, msg.ID, -32602, "Every question requires an answer")
				return
			}
			values := make(map[string]string, len(pending.Questions))
			for normalized, question := range pending.Questions {
				value, ok := answer.Answers[normalized]
				if !ok || len(value.Answers) == 0 {
					rpcError(peer, msg.ID, -32602, "Every question requires an answer")
					return
				}
				values[question] = strings.Join(value.Answers, ", ")
			}
			updated := make(map[string]any, len(pending.Input)+1)
			for key, value := range pending.Input {
				updated[key] = value
			}
			updated["answers"] = values
			response = map[string]any{"behavior": "allow", "updatedInput": updated}
		} else {
			var answer struct {
				Decision string `json:"decision"`
			}
			if json.Unmarshal(msg.Result, &answer) != nil {
				rpcError(peer, msg.ID, -32602, "Invalid approval response")
				return
			}
			switch answer.Decision {
			case "accept":
				response = map[string]any{"behavior": "allow", "updatedInput": pending.Input}
			case "decline":
			case "cancel":
				response["interrupt"] = true
				pending.Process.interrupting = true
			default:
				rpcError(peer, msg.ID, -32602, "Unsupported approval decision")
				return
			}
		}
	}
	delete(s.pending, id)
	pending.Process.controlSuccess(pending.NativeID, response)
	s.broadcast("serverRequest/resolved", map[string]any{"threadId": pending.Process.threadID, "requestId": id})
}

func (p *process) cancelPermission(nativeID string) {
	for id, pending := range p.server.pending {
		if pending.Process == p && pending.NativeID == nativeID {
			delete(p.server.pending, id)
			p.server.broadcast("serverRequest/resolved", map[string]any{"threadId": p.threadID, "requestId": id})
		}
	}
}

func (p *process) resolveAll() {
	for id, pending := range p.server.pending {
		if pending.Process == p {
			delete(p.server.pending, id)
			p.server.broadcast("serverRequest/resolved", map[string]any{"threadId": p.threadID, "requestId": id})
		}
	}
}
