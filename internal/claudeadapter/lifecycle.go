package claudeadapter

import "encoding/json"

// One MACT turn can contain several native queries: the user's prompt, then
// queries triggered by background-task notifications. Native result messages
// end those individual queries, not necessarily the user's whole request.
type turnLifecycle struct {
	turnID       string
	submitted    bool
	state        string
	runningSeen  bool
	result       *turnOutcome
	needsResult  bool // Legacy producers: a task ended after the latest result.
	queryOpen    bool // Parent output after a result requires another result.
	messageTexts map[string][]string
	textItems    map[string][]map[string]any
	lastMessage  string
	lastText     map[string]any
	costStart    *float64
}

type turnOutcome struct{ status, message string }

type streamContentBlock struct {
	kind, toolID string
	claimed      bool
}

type messageOrigin struct {
	Kind  string `json:"kind"`
	RunID string `json:"runId"`
}

type backgroundTask struct {
	id, runID, turnID string
	taskType          string
	toolUseID         string
	consolidated      bool
	active            bool
	background        bool
	ambient           bool
	item              map[string]any
}

type taskEvent struct {
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id"`
	TaskType    string `json:"task_type"`
	ToolUseID   string `json:"tool_use_id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	State       string `json:"state"`
	Ambient     bool   `json:"ambient"`
	Skip        bool   `json:"skip_transcript"`
	Background  *bool  `json:"is_backgrounded"`
	Patch       struct {
		Status     string `json:"status"`
		Background *bool  `json:"is_backgrounded"`
	} `json:"patch"`
	Tasks []taskEvent `json:"tasks"`
}

func settleInterruptedTaskItems(t *savedThread) {
	for _, turn := range t.Turns {
		if turn.Status != "interrupted" && turn.Status != "failed" {
			continue
		}
		for _, item := range turn.Items {
			if item["type"] == "dynamicToolCall" && item["status"] == "inProgress" {
				item["status"] = "interrupted"
			}
		}
	}
}

func (p *process) beginLifecycle(t *savedThread, turnID string) {
	p.lifecycle = turnLifecycle{turnID: turnID, messageTexts: make(map[string][]string), textItems: make(map[string][]map[string]any)}
	if prior := t.ensureStatus().Cost.SessionUSD; prior != nil {
		value := *prior
		p.lifecycle.costStart = &value
	}
	p.streamID = ""
	p.streamItem = make(map[int]string)
	p.streamBlocks = make(map[int]*streamContentBlock)
	// Keep recent tombstones for late protocol frames without accumulating
	// every completed task in an indefinitely reused native process.
	if len(p.tasks) > 1024 {
		for id, task := range p.tasks {
			if !task.active && len(p.tasks) > 512 {
				delete(p.tasks, id)
			}
		}
	}
	if len(p.taskRuns) > 2048 {
		for run, owner := range p.taskRuns {
			if owner != turnID && len(p.taskRuns) > 1024 {
				delete(p.taskRuns, run)
			}
		}
	}
}

func (p *process) acceptTurnFrame(t *savedThread, inputID string, inputIDs []string, origin messageOrigin) bool {
	turn := t.active()
	if turn == nil || p.lifecycle.turnID != turn.ID || !p.lifecycle.submitted {
		return false
	}
	if inputID != "" || len(inputIDs) > 0 {
		matched := inputID == turn.ID
		for _, id := range inputIDs {
			matched = matched || id == turn.ID
		}
		if !matched {
			return false
		}
	}
	// A resumed task can reuse its task id. A late notification from an older
	// run, or a task belonging to a preceding user turn, is not this turn's reply.
	if origin.Kind == "task-notification" && origin.RunID != "" {
		if owner, known := p.taskRuns[origin.RunID]; known && owner != turn.ID {
			return false
		}
	}
	return true
}

// SDK 0.3.310 sdk.d.ts documents idle as occurring after held-back results are
// flushed and the background-agent loop exits. State events require the launch
// environment flag. The full background set has REPLACE semantics; its ordering
// relative to task_started/task_notification bookends is explicitly unspecified.
// https://www.npmjs.com/package/@anthropic-ai/claude-agent-sdk/v/0.3.310
func (p *process) handleLifecycle(t *savedThread, raw []byte, subtype string) bool {
	switch subtype {
	case "session_state_changed", "background_tasks_changed", "task_started", "task_progress", "task_updated", "task_notification":
	default:
		return false
	}
	var event taskEvent
	if json.Unmarshal(raw, &event) != nil {
		return true
	}
	active := t.active() != nil && p.lifecycle.turnID == t.active().ID && p.lifecycle.submitted
	if subtype == "session_state_changed" {
		p.stateEvents = true
		if active {
			if event.State == "running" || event.State == "requires_action" {
				p.lifecycle.runningSeen = true
				p.lifecycle.state = event.State
			} else if event.State == "idle" && p.lifecycle.runningSeen {
				p.lifecycle.state = event.State
				p.tryFinish(t)
			}
		}
		return true
	}
	if subtype == "background_tasks_changed" {
		p.backgroundSet = make(map[string]string)
		allTasks := make(map[string]bool)
		for _, entry := range event.Tasks {
			if !validTaskID(entry.TaskID) {
				continue
			}
			allTasks[entry.TaskID] = true
			task := p.task(t, entry, active)
			if task == nil {
				// An older run's delayed level entry cannot resurrect it or
				// erase the newer run we already observed.
				if current := p.tasks[entry.TaskID]; current != nil && current.active && !current.ambient {
					p.backgroundSet[current.id] = current.runID
				}
			} else if task.active {
				if task.ambient && !entry.Ambient && !entry.Skip && active && task.turnID != p.lifecycle.turnID {
					task.turnID, task.item = p.lifecycle.turnID, nil
					if task.runID != "" {
						p.taskRuns[task.runID] = task.turnID
					}
				}
				task.ambient = entry.Ambient || entry.Skip
				if !task.ambient {
					p.backgroundSet[entry.TaskID] = task.runID
				}
				task.background = true
				p.showTask(t, task, entry)
			}
		}
		// A level can repair a missed terminal edge. It must not manufacture a
		// parent result or treat removal as proof of completed consolidation.
		for _, task := range p.tasks {
			if !allTasks[task.id] && task.active && task.background {
				task.active = false
				if task.turnID == p.lifecycle.turnID {
					p.endTask(t, task, "completed")
					if !task.ambient {
						p.lifecycle.needsResult = true
						p.taskEnded(task, "completed")
					}
				}
			}
		}
		p.tryFinish(t)
		p.saveLifecycle(t)
		return true
	}
	task := p.task(t, event, active)
	if task == nil {
		return true
	}
	status := event.Status
	if subtype == "task_updated" {
		status = event.Patch.Status
	}
	if task.turnID != p.lifecycle.turnID || !active {
		if taskTerminal(status) {
			task.active = false
		}
		return true
	}
	if event.Background != nil {
		task.background = *event.Background
	}
	if event.Patch.Background != nil {
		task.background = *event.Patch.Background
	}
	if taskTerminal(status) {
		// Replayed terminal bookends must not invalidate a later parent result.
		if task.active {
			task.active = false
			if !task.ambient {
				p.lifecycle.needsResult = true
				p.taskEnded(task, status)
			}
			p.endTask(t, task, taskItemStatus(status))
		}
	} else if task.active {
		p.showTask(t, task, event)
	}
	p.tryFinish(t)
	p.saveLifecycle(t)
	return true
}

func validTaskID(id string) bool { return id != "" && len(id) <= 512 }

func taskTerminal(status string) bool {
	switch status {
	case "completed", "failed", "stopped", "killed":
		return true
	}
	return false
}

func taskItemStatus(status string) string {
	if status == "failed" {
		return "failed"
	}
	if status == "stopped" || status == "killed" {
		return "interrupted"
	}
	return "completed"
}

func (p *process) task(t *savedThread, event taskEvent, active bool) *backgroundTask {
	if !validTaskID(event.TaskID) || len(event.RunID) > 512 {
		return nil
	}
	if p.tasks == nil {
		p.tasks = make(map[string]*backgroundTask)
	}
	task := p.tasks[event.TaskID]
	if task != nil {
		if event.RunID == "" || event.RunID == task.runID {
			if event.TaskType != "" {
				task.taskType = event.TaskType
			}
			if event.ToolUseID != "" {
				task.toolUseID = event.ToolUseID
			}
			return task
		}
		if task.runID == "" && task.turnID == p.lifecycle.turnID {
			task.runID = event.RunID
			if event.TaskType != "" {
				task.taskType = event.TaskType
			}
			if event.ToolUseID != "" {
				task.toolUseID = event.ToolUseID
			}
			if p.taskRuns == nil {
				p.taskRuns = make(map[string]string)
			}
			p.taskRuns[event.RunID] = task.turnID
			return task
		}
		// Run ids sort in opening order. Ignore an older run's late frames.
		if task.runID != "" && event.RunID < task.runID {
			return nil
		}
		if task.active {
			p.endTask(t, task, "interrupted")
		}
	}
	turnID := ""
	if active {
		turnID = t.active().ID
	}
	task = &backgroundTask{id: event.TaskID, runID: event.RunID, turnID: turnID, taskType: event.TaskType, toolUseID: event.ToolUseID, active: true, background: true, ambient: event.Ambient || event.Skip}
	p.tasks[event.TaskID] = task
	if event.RunID != "" {
		if p.taskRuns == nil {
			p.taskRuns = make(map[string]string)
		}
		p.taskRuns[event.RunID] = turnID
	}
	return task
}

func (p *process) showTask(t *savedThread, task *backgroundTask, event taskEvent) {
	if task.ambient || !task.background || !task.active || t.active() == nil || t.active().ID != task.turnID {
		return
	}
	if task.item == nil {
		task.item = map[string]any{"id": "claude-task-" + task.id + "-" + task.runID, "type": "dynamicToolCall", "namespace": "claude", "tool": "backgroundTask", "arguments": map[string]any{"taskId": task.id, "description": event.Description, "taskType": event.TaskType}, "status": "inProgress"}
		p.appendItem(t, task.item)
		p.notifyItem(t, "item/started", task.item)
	}
}

func (p *process) endTask(t *savedThread, task *backgroundTask, status string) {
	if task.item != nil && t.active() != nil && t.active().ID == task.turnID {
		task.item["status"] = status
		p.notifyItem(t, "item/completed", task.item)
	}
}

func (p *process) hasBackgroundWork() bool {
	if len(p.backgroundSet) > 0 {
		return true
	}
	// A fresh start can precede the next level snapshot. Keep that work until
	// a terminal edge or a later authoritative replacement removes it. Known
	// terminal runs remain tombstones, so delayed starts cannot resurrect them.
	for _, task := range p.tasks {
		if task.active && task.background && !task.ambient {
			return true
		}
	}
	return false
}

func (p *process) recordResult(t *savedThread, status, message string, origin messageOrigin) {
	if p.interrupting {
		// Cancellation is not proof that every subprocess/agent stopped. Retire
		// the process group before accepting another prompt on this session.
		p.stop()
		if p.server.runners[t.ID] == p {
			delete(p.server.runners, t.ID)
		}
		p.finish(t, "interrupted", message)
		return
	}
	if p.lifecycle.result == nil || p.lifecycle.result.status == "completed" {
		p.lifecycle.result = &turnOutcome{status: status, message: message}
	}
	if origin.Kind == "task-notification" && origin.RunID != "" {
		for _, task := range p.tasks {
			if task.runID == origin.RunID {
				task.consolidated = true
			}
		}
	}
	p.lifecycle.needsResult = false
	p.lifecycle.queryOpen = false
	p.tryFinish(t)
	p.saveLifecycle(t)
}

func (p *process) taskEnded(task *backgroundTask, status string) {
	// Agent/workflow idle already certifies their notification loop drained,
	// even when the independent task-level stream/bookends arrive afterward.
	// Bash may validly coexist with idle: its OLD idle does not certify that a
	// newly finished shell's notification was consolidated. Killed tasks can
	// have no notification at all and need no model continuation.
	if status != "killed" && !task.consolidated && task.taskType != "local_agent" && task.taskType != "local_workflow" {
		p.lifecycle.state = "running"
	}
}

func (p *process) observeToolLifecycle(t *savedThread, raw json.RawMessage, toolUseID string) {
	var output struct {
		IsAsync      bool   `json:"isAsync"`
		Status       string `json:"status"`
		AgentID      string `json:"agentId"`
		TaskID       string `json:"taskId"`
		Description  string `json:"description"`
		BackgroundID string `json:"backgroundTaskId"`
	}
	if json.Unmarshal(raw, &output) != nil {
		return
	}
	if output.Status == "completed" && output.AgentID != "" {
		// Legacy CLIs can register foreground Agent tasks without the newer
		// is_backgrounded flag or a separate terminal notification. The native
		// structured result certifies that this particular tool call finished.
		if task := p.tasks[output.AgentID]; task != nil && task.active && task.turnID == p.lifecycle.turnID && (task.toolUseID == "" || task.toolUseID == toolUseID) {
			task.active = false
			p.endTask(t, task, "completed")
			p.lifecycle.needsResult = true
		}
		return
	}
	event := taskEvent{Description: output.Description, ToolUseID: toolUseID}
	if output.Status == "async_launched" || output.IsAsync {
		event.TaskID, event.TaskType = output.AgentID, "local_agent"
	} else if output.Status == "remote_launched" {
		event.TaskID, event.TaskType = output.TaskID, "remote_agent"
	} else if output.BackgroundID != "" {
		event.TaskID, event.TaskType = output.BackgroundID, "local_bash"
	} else {
		return
	}
	if task := p.task(t, event, true); task != nil {
		p.showTask(t, task, event)
	}
}

func (p *process) tryFinish(t *savedThread) {
	if t.active() == nil || t.active().ID != p.lifecycle.turnID || p.lifecycle.result == nil || p.lifecycle.queryOpen || p.hasBackgroundWork() {
		return
	}
	if p.stateEvents && p.lifecycle.runningSeen {
		if p.lifecycle.state != "idle" {
			return
		}
	} else if p.lifecycle.needsResult {
		// Older CLIs have no state events: ordinary foreground turns still end
		// on result, but task completion must be followed by a parent result.
		return
	}
	if p.lifecycle.result.status == "completed" {
		items := p.lifecycle.textItems[p.lifecycle.lastMessage]
		if len(items) == 0 && p.lifecycle.lastText != nil {
			items = []map[string]any{p.lifecycle.lastText}
		}
		for _, item := range items {
			if item["phase"] != "final_answer" {
				item["phase"] = "final_answer"
				p.notifyItem(t, "item/completed", item)
			}
		}
	}
	outcome := *p.lifecycle.result
	p.finish(t, outcome.status, outcome.message)
}

func (p *process) noteParentActivity() {
	if p.lifecycle.result != nil {
		p.lifecycle.queryOpen = true
	}
	if p.lifecycle.state == "idle" {
		p.lifecycle.state = "running"
	}
}

func (p *process) acceptMessageOwner(t *savedThread, messageID string) bool {
	if messageID == "" {
		return true
	}
	if owner, known := p.messageOwners[messageID]; known {
		return t.active() != nil && owner == t.active().ID
	}
	if t.active() == nil {
		return false
	}
	if p.messageOwners == nil {
		p.messageOwners = make(map[string]string)
	}
	p.messageOwners[messageID] = t.active().ID
	p.messageOrder = append(p.messageOrder, messageID)
	if len(p.messageOrder) > 2048 {
		delete(p.messageOwners, p.messageOrder[0])
		p.messageOrder = p.messageOrder[1:]
	}
	return true
}

func (p *process) claimStreamBlock(messageID, kind, toolID string) (int, bool) {
	if p.streamID != messageID {
		return 0, false
	}
	index := -1
	for candidate, block := range p.streamBlocks {
		if !block.claimed && block.kind == kind && (toolID == "" || block.toolID == toolID) && (index < 0 || candidate < index) {
			index = candidate
		}
	}
	if index < 0 {
		return 0, false
	}
	p.streamBlocks[index].claimed = true
	return index, true
}

func (p *process) saveLifecycle(t *savedThread) {
	if err := p.server.save(t); err != nil {
		p.stop()
	}
}

func (p *process) observeTurnCost(t *savedThread, raw []byte) {
	var result struct {
		Total json.RawMessage `json:"total_cost_usd"`
	}
	if json.Unmarshal(raw, &result) != nil || rawNumber(result.Total) == nil {
		return
	}
	status := t.ensureStatus()
	if start := p.lifecycle.costStart; start != nil && status.Cost.SessionUSD != nil && !status.Cost.Incomplete && *status.Cost.SessionUSD >= *start {
		delta := *status.Cost.SessionUSD - *start
		status.Cost.LastTurnUSD = &delta
	} else if start == nil && len(t.Turns) == 1 && status.Cost.SessionUSD != nil && !status.Cost.Incomplete {
		total := *status.Cost.SessionUSD
		status.Cost.LastTurnUSD = &total
	}
}
