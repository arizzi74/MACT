package claudeadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxNativeHistoryBytes = 64 << 20

// NativeHistoryRecovery describes an explicit, local reconciliation. It does
// not submit input to Claude, read child transcripts, or estimate missing cost.
type NativeHistoryRecovery struct {
	ThreadID    string `json:"threadId"`
	TurnID      string `json:"turnId"`
	AddedItems  int    `json:"addedItems"`
	Changed     bool   `json:"changed"`
	CompletedAt int64  `json:"completedAt"`
}

// RecoverNativeHistory imports parent-visible persisted output into the same
// saved turn. A running CLI, even an idle one, must first be closed. The mutex
// fences local admission/deletion; filesystem snapshots fence native writers.
func (r *Runtime) RecoverNativeHistory(threadID, turnID string) (*NativeHistoryRecovery, error) {
	if r == nil || r.server == nil {
		return nil, errors.New("Claude runtime is unavailable")
	}
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverNativeHistory(threadID, turnID)
}

// server.mu must be held. All changes are prepared on a copy and committed to
// memory only after the normal atomic display-history save succeeds.
func (s *server) recoverNativeHistory(threadID, turnID string) (*NativeHistoryRecovery, error) {
	if s.closed || !canonicalID(threadID) || !canonicalID(turnID) {
		return nil, recoveryError("invalid or unavailable session")
	}
	t := s.threads[threadID]
	if t == nil || s.deletions[threadID] != nil || s.deleting[threadID] {
		return nil, recoveryError("session is unavailable or being deleted")
	}
	if t.active() != nil || len(t.Queue) != 0 || s.runners[threadID] != nil {
		return nil, recoveryError("close the native process and clear active or queued input first")
	}
	for _, pending := range s.pending {
		if pending.Process.threadID == threadID {
			return nil, recoveryError("resolve pending approvals or questions first")
		}
	}
	index := -1
	for i, turn := range t.Turns {
		if turn.ID == turnID {
			index = i
			break
		}
	}
	if index < 0 || t.Turns[index].Status != "completed" || !t.NativeStarted || len(t.NativeConfigRoots) == 0 {
		return nil, recoveryError("a completed turn and recorded native session location are required")
	}
	data, err := readNativeHistory(t.NativeConfigRoots, threadID)
	if err != nil {
		return nil, err
	}
	before, err := json.Marshal(t)
	if err != nil {
		return nil, recoveryError("saved history is invalid")
	}
	var next savedThread
	if json.Unmarshal(before, &next) != nil {
		return nil, recoveryError("saved history is invalid")
	}
	turn := next.Turns[index]
	var nextStart int64
	if index+1 < len(next.Turns) {
		nextStart = next.Turns[index+1].StartedAt
	}
	items, completed, err := reconstructNativeTurn(data, &next, turn, nextStart)
	if err != nil {
		return nil, err
	}
	merged, added, err := mergeNativeItems(turn.Items, items)
	if err != nil {
		return nil, err
	}
	oldCompleted := turn.CompletedAt
	turn.Items, turn.CompletedAt = merged, &completed
	next.UpdatedAt = max(next.UpdatedAt, completed)
	if oldCompleted == nil || completed > *oldCompleted {
		// A native transcript has message usage, not an authoritative cumulative
		// USD result. Keep prior evidence internally but hide the stale totals.
		next.ensureStatus().Cost.Incomplete = true
		next.ClaudeStatus.Context.UsedTokens = nil
	}
	after, err := json.Marshal(&next)
	if err != nil {
		return nil, recoveryError("recovered history is invalid")
	}
	result := &NativeHistoryRecovery{ThreadID: threadID, TurnID: turnID, AddedItems: added, CompletedAt: completed, Changed: !bytes.Equal(before, after)}
	if !result.Changed {
		return result, nil
	}
	if err := s.save(&next); err != nil {
		return nil, err
	}
	s.threads[threadID] = &next
	s.broadcastStatus(&next)
	s.broadcast("thread/history/recovered", result)
	return result, nil
}

func recoveryError(message string) error { return errors.New("Claude history recovery: " + message) }

// Only the exact main transcript basename is opened, under recorded physical
// configuration roots. Never follow a native output-file or subagent path.
func readNativeHistory(directories []string, id string) ([]byte, error) {
	if len(directories) > 32 {
		return nil, recoveryError("too many native locations")
	}
	var found []byte
	seen := make(map[string]bool)
	for _, directory := range directories {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(filepath.Separator) || seen[directory] {
			return nil, recoveryError("invalid native location")
		}
		seen[directory] = true
		physical, err := filepath.EvalSymlinks(directory)
		if err != nil || physical != directory {
			return nil, recoveryError("native location is unavailable or contains a symbolic link")
		}
		config, err := openRealDirectory(directory)
		if err != nil {
			return nil, recoveryError("native location is unavailable")
		}
		if err := verifyNoRunningNativeSession(config, id); err != nil {
			config.Close()
			return nil, recoveryError("native session is running or its process registry cannot be verified")
		}
		projects, err := openRealChild(config, "projects")
		config.Close()
		if err != nil {
			return nil, recoveryError("native projects directory is unavailable")
		}
		entries, err := readRootDirectory(projects, maxNativeProjectDirectories)
		if err != nil {
			projects.Close()
			return nil, recoveryError("native project inventory exceeds limits or is unavailable")
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			project, err := openRealChild(projects, entry.Name())
			if err != nil {
				projects.Close()
				return nil, recoveryError("native project directory changed")
			}
			data, err := readPrivateNativeTranscript(project, id+".jsonl")
			project.Close()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				projects.Close()
				return nil, err
			}
			if found != nil {
				projects.Close()
				return nil, recoveryError("multiple main transcripts make the source ambiguous")
			}
			found = data
		}
		projects.Close()
	}
	if found == nil {
		return nil, recoveryError("main native transcript was not found")
	}
	return found, nil
}

func readPrivateNativeTranscript(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxNativeHistoryBytes {
		return nil, recoveryError("native transcript must be a bounded private regular file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, recoveryError("native transcript is unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, recoveryError("native transcript changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxNativeHistoryBytes+1))
	after, statErr := f.Stat()
	current, pathErr := root.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || len(data) > maxNativeHistoryBytes || int64(len(data)) != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return nil, recoveryError("native transcript changed during recovery")
	}
	return data, nil
}

type nativeHistoryRecord struct {
	Type              string          `json:"type"`
	SessionID         string          `json:"sessionId"`
	UUID              string          `json:"uuid"`
	Timestamp         string          `json:"timestamp"`
	CWD               string          `json:"cwd"`
	IsSidechain       bool            `json:"isSidechain"`
	ParentToolUseID   string          `json:"parent_tool_use_id"`
	APIBlockIndex     *int            `json:"apiBlockIndex"`
	PromptSource      string          `json:"promptSource"`
	TurnOrigin        string          `json:"turnOrigin"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Error             json.RawMessage `json:"error"`
	ToolUseResult     struct {
		IsAsync bool   `json:"isAsync"`
		Status  string `json:"status"`
	} `json:"toolUseResult"`
	Origin struct {
		Kind     string `json:"kind"`
		Producer string `json:"producer"`
	} `json:"origin"`
	Message struct {
		ID         string          `json:"id"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

type nativeHistoryBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

func (r *nativeHistoryRecord) taskNotification() bool {
	return r.Type == "user" && r.PromptSource == "system" && r.TurnOrigin == "task_notification" && r.Origin.Kind == "task-notification" && r.Origin.Producer == "session-task"
}

func promptCanonical(raw json.RawMessage) ([]byte, bool) {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		value, _ := json.Marshal([]map[string]any{{"type": "text", "text": plain}})
		return value, true
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return nil, false
	}
	for _, block := range blocks {
		if block["type"] != "text" && block["type"] != "image" {
			return nil, false
		}
	}
	value, err := json.Marshal(blocks)
	return value, err == nil
}

func reconstructNativeTurn(data []byte, thread *savedThread, turn *savedTurn, nextStart int64) ([]map[string]any, int64, error) {
	var prompt map[string]any
	for _, item := range turn.Items {
		if item["type"] == "userMessage" {
			if prompt != nil {
				return nil, 0, recoveryError("saved turn has ambiguous user input")
			}
			prompt = item
		}
	}
	if prompt == nil {
		return nil, 0, recoveryError("saved user input is missing")
	}
	promptJSON, _ := json.Marshal(prompt["content"])
	wanted, valid := promptCanonical(promptJSON)
	if !valid {
		return nil, 0, recoveryError("saved user input cannot be matched safely")
	}
	var rows []nativeHistoryRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 65536), maxMessageBytes)
	for scanner.Scan() {
		if len(rows) >= 100000 {
			return nil, 0, recoveryError("native transcript has too many records")
		}
		var row nativeHistoryRecord
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return nil, 0, recoveryError("native transcript contains an invalid record")
		}
		if row.SessionID != "" && row.SessionID != thread.ID {
			return nil, 0, recoveryError("native transcript belongs to another session")
		}
		rows = append(rows, row)
	}
	if scanner.Err() != nil {
		return nil, 0, recoveryError("native transcript record exceeds limits")
	}
	start, exactStart := -1, -1
	ambiguous := false
	for i := range rows {
		row := &rows[i]
		if row.Type != "user" || row.IsSidechain || row.ParentToolUseID != "" || row.taskNotification() {
			continue
		}
		candidate, ok := promptCanonical(row.Message.Content)
		if !ok || !bytes.Equal(wanted, candidate) {
			continue
		}
		stamp, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || stamp.Unix() < turn.StartedAt-5 || stamp.Unix() > turn.StartedAt+300 || (nextStart > 0 && stamp.Unix() >= nextStart) || filepath.Clean(row.CWD) != thread.CWD {
			continue
		}
		if row.UUID == turn.ID {
			if exactStart != -1 {
				return nil, 0, recoveryError("native input identity is duplicated")
			}
			exactStart = i
		} else {
			if start != -1 {
				ambiguous = true
			}
			start = i
		}
	}
	if exactStart >= 0 {
		start = exactStart
	} else if ambiguous {
		return nil, 0, recoveryError("native input matches more than one prompt")
	}
	if start == -1 {
		return nil, 0, recoveryError("native input does not match this saved turn")
	}
	stamp, _ := time.Parse(time.RFC3339Nano, rows[start].Timestamp)
	prompt["recordedAtMs"] = stamp.UnixMilli()
	items := []map[string]any{prompt}
	tools := make(map[string]map[string]any)
	background := make(map[string]bool)
	seen := make(map[string]map[string]any)
	var final map[string]any
	var finalTime int64
	var lastRelevant time.Time
	for i := start + 1; i < len(rows); i++ {
		row := &rows[i]
		if row.IsSidechain || row.ParentToolUseID != "" || (row.Type != "user" && row.Type != "assistant") {
			continue
		}
		if _, isPrompt := promptCanonical(row.Message.Content); row.Type == "user" && isPrompt && !row.taskNotification() {
			break
		}
		at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || at.UnixMilli() <= 0 || at.UnixMilli() > 253402300799999 || at.Before(lastRelevant) || (nextStart > 0 && at.Unix() >= nextStart) {
			return nil, 0, recoveryError("native message timing is invalid or overlaps another saved turn")
		}
		if row.SessionID != thread.ID || row.UUID == "" || len(row.UUID) > 512 || !filepath.IsAbs(row.CWD) || !artifactContainsProject(thread.CWD, filepath.Clean(row.CWD)) {
			return nil, 0, recoveryError("native message identity or workspace does not match")
		}
		if row.IsAPIErrorMessage || (len(row.Error) > 0 && string(row.Error) != "null" && string(row.Error) != `""`) {
			return nil, 0, recoveryError("native provider errors cannot be imported as completed output")
		}
		lastRelevant = at
		if row.taskNotification() {
			notification, err := parseNativeTaskNotification(row.Message.Content)
			if err != nil {
				return nil, 0, err
			}
			tool := tools[notification.ToolUseID]
			if tool == nil || !background[notification.ToolUseID] {
				return nil, 0, recoveryError("task notification has no matching background tool")
			}
			tool["status"], tool["completedAtMs"], tool["output"] = notification.Status, at.UnixMilli(), notification.Summary
			if notification.Result != "" {
				tool["output"] = notification.Summary + "\n\n" + notification.Result
			}
			delete(background, notification.ToolUseID)
			item := map[string]any{"id": "native-notification-" + row.UUID, "type": "agentMessage", "phase": "commentary", "text": notification.Summary, "recordedAtMs": at.UnixMilli()}
			items = append(items, item)
			final = nil
			continue
		}
		var blocks []nativeHistoryBlock
		if json.Unmarshal(row.Message.Content, &blocks) != nil {
			return nil, 0, recoveryError("native message content is invalid")
		}
		if row.Type == "user" {
			for _, block := range blocks {
				if block.Type != "tool_result" {
					return nil, 0, recoveryError("unrecognized automatic native input")
				}
				if tool := tools[block.ToolUseID]; tool != nil {
					// Claude can choose background execution implicitly, without
					// run_in_background in the original Agent arguments.
					if row.ToolUseResult.IsAsync || row.ToolUseResult.Status == "async_launched" {
						background[block.ToolUseID] = true
					}
					if !background[block.ToolUseID] || block.IsError {
						tool["status"], tool["completedAtMs"] = "completed", at.UnixMilli()
						if block.IsError {
							tool["status"] = "failed"
							delete(background, block.ToolUseID)
						}
					}
				}
			}
			continue
		}
		for j, block := range blocks {
			index := j
			if row.APIBlockIndex != nil {
				if len(blocks) != 1 || *row.APIBlockIndex < 0 || *row.APIBlockIndex > 100000 {
					return nil, 0, recoveryError("native block identity is ambiguous")
				}
				index = *row.APIBlockIndex
			}
			var item map[string]any
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				if row.Message.ID == "" || len(row.Message.ID) > 400 {
					return nil, 0, recoveryError("native assistant identity is missing")
				}
				item = map[string]any{"id": row.Message.ID + "-" + strconv.Itoa(index), "type": "agentMessage", "phase": "commentary", "text": block.Text, "recordedAtMs": at.UnixMilli()}
				final = nil
				if row.Message.StopReason == "end_turn" {
					final = item
					finalTime = at.Unix()
				}
			case "tool_use":
				if block.ID == "" || len(block.ID) > 512 || block.Name == "" {
					return nil, 0, recoveryError("native tool identity is missing")
				}
				var input any
				if json.Unmarshal(block.Input, &input) != nil {
					return nil, 0, recoveryError("native tool arguments are invalid")
				}
				item = map[string]any{"id": block.ID, "type": "dynamicToolCall", "namespace": "claude", "tool": block.Name, "arguments": input, "status": "inProgress", "recordedAtMs": at.UnixMilli(), "startedAtMs": at.UnixMilli()}
				tools[block.ID] = item
				if args, ok := input.(map[string]any); ok && args["run_in_background"] == true {
					background[block.ID] = true
				}
				final = nil
			default:
				// thinking, redacted_thinking, and unknown private blocks are
				// never copied into the display history.
				continue
			}
			id := item["id"].(string)
			if prior := seen[id]; prior != nil {
				return nil, 0, recoveryError("native item identity is duplicated")
			}
			seen[id] = item
			items = append(items, item)
		}
	}
	if final == nil || len(background) != 0 {
		return nil, 0, recoveryError("native turn has no final reply or still has background work")
	}
	for _, tool := range tools {
		if tool["status"] == "inProgress" {
			return nil, 0, recoveryError("native tool has not completed")
		}
	}
	if turn.CompletedAt != nil && finalTime < *turn.CompletedAt {
		return nil, 0, recoveryError("native completion predates saved completion")
	}
	final["phase"] = "final_answer"
	return items, finalTime, nil
}

type nativeTaskNotification struct{ ToolUseID, Status, Summary, Result string }

// Native task result bodies can contain unescaped markup. Read only the fixed
// envelope fields before <result>, then take the bounded result body literally.
func parseNativeTaskNotification(raw json.RawMessage) (nativeTaskNotification, error) {
	var text string
	var out nativeTaskNotification
	if json.Unmarshal(raw, &text) != nil || len(text) > maxMessageBytes || !strings.HasPrefix(text, "<task-notification>") || !strings.HasSuffix(strings.TrimSpace(text), "</task-notification>") {
		return out, recoveryError("invalid native task notification")
	}
	header := text
	if prefix, body, ok := strings.Cut(text, "<result>"); ok {
		header = prefix
		end := strings.LastIndex(body, "</result>")
		if end < 0 {
			return out, recoveryError("invalid native task result")
		}
		out.Result = strings.TrimSpace(body[:end])
	}
	field := func(name string) string {
		_, tail, ok := strings.Cut(header, "<"+name+">")
		if !ok {
			return ""
		}
		value, _, ok := strings.Cut(tail, "</"+name+">")
		if !ok || strings.ContainsAny(value, "\r\n<>") {
			return ""
		}
		return strings.TrimSpace(value)
	}
	out.ToolUseID, out.Status, out.Summary = field("tool-use-id"), field("status"), field("summary")
	if out.ToolUseID == "" || out.Summary == "" || len(out.Summary) > 4096 {
		return out, recoveryError("native task notification lacks identity or summary")
	}
	switch out.Status {
	case "completed", "failed":
	default:
		return out, recoveryError("native task notification is not terminal")
	}
	return out, nil
}

func mergeNativeItems(existing, native []map[string]any) ([]map[string]any, int, error) {
	byID := make(map[string]int)
	textCounts := make(map[string]int)
	for i, item := range existing {
		id, _ := item["id"].(string)
		if id == "" {
			return nil, 0, recoveryError("saved item identity is missing")
		}
		if _, duplicate := byID[id]; duplicate {
			return nil, 0, recoveryError("saved item identity is duplicated")
		}
		byID[id] = i
		if item["type"] == "agentMessage" {
			textCounts[fmt.Sprint(item["text"])]++
		}
	}
	used := make(map[int]bool)
	result := make([]map[string]any, 0, len(native))
	added, previous := 0, -1
	for _, item := range native {
		id := item["id"].(string)
		old, matched := byID[id]
		if !matched && item["type"] == "agentMessage" && textCounts[fmt.Sprint(item["text"])] == 1 {
			for i, prior := range existing {
				if !used[i] && prior["type"] == "agentMessage" && prior["text"] == item["text"] {
					old, matched = i, true
					break
				}
			}
		}
		if matched {
			if used[old] || old < previous || existing[old]["type"] != item["type"] {
				return nil, 0, recoveryError("saved and native item order or identity conflicts")
			}
			// Preserve saved identities, including older random result IDs.
			item["id"] = existing[old]["id"]
			used[old], previous = true, old
		} else {
			added++
		}
		result = append(result, item)
	}
	if len(used) != len(existing) {
		return nil, 0, recoveryError("some saved items cannot be reconciled without loss")
	}
	return result, added, nil
}
