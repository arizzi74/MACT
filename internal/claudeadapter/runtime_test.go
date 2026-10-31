package claudeadapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

// This subprocess models the documented Agent SDK control wire. It deliberately
// rejects a resume without a native persisted session, so a history-only fake
// cannot pass the restart lifecycle test.
func init() {
	if os.Getenv("MACT_CLAUDE_HELPER") == "1" {
		TestClaudeProcessHelper(nil)
	}
}

func TestClaudeProcessHelper(t *testing.T) {
	if os.Getenv("MACT_CLAUDE_HELPER") != "1" {
		return
	}
	root := os.Getenv("MACT_CLAUDE_HELPER_ROOT")
	probe := false
	for _, arg := range os.Args[1:] {
		if arg == "--help" {
			if os.Getenv("MACT_CLAUDE_HELPER_NO_CAPABILITIES") == "1" {
				os.Exit(1)
			}
			if os.Getenv("MACT_CLAUDE_HELPER_MANUAL_MODE") == "1" {
				fmt.Fprintln(os.Stdout, `  --permission-mode <mode> (choices: "manual", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions")`)
			} else {
				fmt.Fprintln(os.Stdout, `  --permission-mode <mode> (choices: "default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions")`)
			}
			os.Exit(0)
		}
		if arg == "--no-session-persistence" {
			probe = true
		}
	}
	session, resume := "", false
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--session-id=") {
			session = strings.TrimPrefix(arg, "--session-id=")
		}
		if strings.HasPrefix(arg, "--resume=") {
			session = strings.TrimPrefix(arg, "--resume=")
			resume = true
		}
	}
	if session == "" {
		os.Exit(31)
	}
	if resume {
		if _, err := os.Stat(filepath.Join(root, session+".native")); err != nil {
			os.Exit(32)
		}
	}
	log := func(value any) {
		f, err := os.OpenFile(filepath.Join(root, "actions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(33)
		}
		_ = json.NewEncoder(f).Encode(value)
		_ = f.Close()
	}
	model, effort, permissionMode, artifactPrompt := "", "", "", ""
	allowBypass := false
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "--append-system-prompt=") {
			artifactPrompt = strings.TrimPrefix(arg, "--append-system-prompt=")
		}
		if arg == "--allow-dangerously-skip-permissions" {
			allowBypass = true
		}
		if strings.HasPrefix(arg, "--effort=") {
			effort = strings.TrimPrefix(arg, "--effort=")
		}
		if strings.HasPrefix(arg, "--permission-mode=") {
			permissionMode = strings.TrimPrefix(arg, "--permission-mode=")
		}
		if strings.HasPrefix(arg, "--model=") {
			model = strings.TrimPrefix(arg, "--model=")
		}
	}
	if !probe {
		log(map[string]any{"launch": true, "resume": resume, "session": session, "model": model, "effort": effort, "effortEnv": os.Getenv("CLAUDE_CODE_EFFORT_LEVEL"), "permissionMode": permissionMode, "artifactPrompt": artifactPrompt, "allowBypass": allowBypass, "hasToken": os.Getenv("ANTHROPIC_AUTH_TOKEN") != "", "hasAPIKey": os.Getenv("ANTHROPIC_API_KEY") != ""})
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	result := func(text string) {
		id := uuid.NewString()
		send(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": text}}}})
		send(map[string]any{"type": "result", "subtype": "success", "result": text, "session_id": session})
	}
	mode := ""
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 65536), maxMessageBytes)
	for scanner.Scan() {
		var msg map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			os.Exit(34)
		}
		var kind string
		_ = json.Unmarshal(msg["type"], &kind)
		switch kind {
		case "control_request":
			var id string
			_ = json.Unmarshal(msg["request_id"], &id)
			var request struct {
				Subtype       string         `json:"subtype"`
				Hooks         map[string]any `json:"hooks"`
				SkipBehaviors bool           `json:"skip_behaviors"`
				Mode          string         `json:"mode"`
			}
			_ = json.Unmarshal(msg["request"], &request)
			if request.Subtype == "initialize" {
				if request.Hooks["PreToolUse"] != nil || (!probe && os.Getenv("CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS") != "1") {
					os.Exit(35)
				}
				if !probe && os.Getenv("MACT_CLAUDE_HELPER_HANG_INIT") == "1" {
					continue
				}
				catalog := map[string]any{}
				if os.Getenv("MACT_CLAUDE_HELPER_NO_CAPABILITIES") != "1" {
					_ = json.Unmarshal([]byte(`{"models":[{"value":"default","resolvedModel":"claude-sonnet-4-6","displayName":"Default","supportedEffortLevels":["low","medium","high","max"]},{"value":"sonnet","resolvedModel":"claude-sonnet-4-6","displayName":"Sonnet","supportsEffort":true,"supportedEffortLevels":["low","medium","high","max"]},{"value":"opus","resolvedModel":"claude-opus-4-7","displayName":"Opus","supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"]},{"value":"haiku","displayName":"Haiku"}]}`), &catalog)
				}
				if effective := os.Getenv("MACT_CLAUDE_HELPER_EFFECTIVE_PERMISSION"); effective != "" && !probe {
					catalog["current_permission_mode"] = effective
				}
				send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": catalog}})
			}
			if request.Subtype == "interrupt" {
				send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
				send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
			}
			if request.Subtype == "get_usage" {
				if !probe || !request.SkipBehaviors {
					os.Exit(98)
				}
				if os.Getenv("MACT_CLAUDE_HELPER_HANG_USAGE") == "1" {
					continue
				}
				log(map[string]any{"usageProbe": true, "skipBehaviors": true})
				if os.Getenv("MACT_CLAUDE_HELPER_USAGE_ERROR") == "1" {
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "PRIVATE NATIVE USAGE FAILURE"}})
					continue
				}
				var usage map[string]any
				_ = json.Unmarshal([]byte(os.Getenv("MACT_CLAUDE_HELPER_USAGE")), &usage)
				send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": usage}})
			}
			if request.Subtype == "set_permission_mode" {
				if !probe || request.Mode == "bypassPermissions" && !allowBypass {
					os.Exit(97)
				}
				log(map[string]any{"permissionProbe": true, "mode": request.Mode})
				if os.Getenv("MACT_CLAUDE_HELPER_DISABLE_BYPASS") == "1" && request.Mode == "bypassPermissions" {
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "PRIVATE native settings disable bypass permissions"}})
				} else {
					send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{}}})
				}
			}
		case "user":
			if probe {
				os.Exit(99)
			}
			_ = os.WriteFile(filepath.Join(root, session+".native"), []byte("native Claude session"), 0o600)
			var message struct {
				Content []map[string]any `json:"content"`
			}
			_ = json.Unmarshal(msg["message"], &message)
			mode = ""
			for _, block := range message.Content {
				if block["type"] == "text" {
					mode, _ = block["text"].(string)
					break
				}
			}
			log(map[string]any{"user": mode, "session": session})
			switch mode {
			case "/compact":
				switch os.Getenv("MACT_CLAUDE_HELPER_COMPACT") {
				case "block":
					continue
				case "exit":
					os.Exit(7)
				case "skip":
					send(map[string]any{"type": "result", "subtype": "success", "result": "Not enough messages to compact."})
					continue
				case "error":
					send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
					continue
				}
				send(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "running"})
				send(map[string]any{"type": "system", "subtype": "status", "status": "compacting"})
				send(map[string]any{"type": "system", "subtype": "compact_boundary", "compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 32000}})
				send(map[string]any{"type": "result", "subtype": "success"})
				if os.Getenv("MACT_CLAUDE_HELPER_COMPACT") == "hold-idle" {
					continue
				}
				send(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle"})
			case "provider-error":
				send(map[string]any{"type": "assistant", "error": "authentication_failed", "message": map[string]any{"id": uuid.NewString(), "content": []any{map[string]any{"type": "text", "text": "PRIVATE PROVIDER AUTH RESPONSE"}}}})
				send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "PRIVATE PROVIDER AUTH RESPONSE"})
			case "exit":
				os.Exit(7)
			case "malformed":
				fmt.Fprintln(os.Stdout, "{bad JSON")
			case "block":
			case "approval":
				send(map[string]any{"type": "control_request", "request_id": "native-approval", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "tool-approval", "input": map[string]any{"command": "printf hello"}}})
			case "file-approval":
				send(map[string]any{"type": "control_request", "request_id": "native-approval", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "tool_use_id": "tool-write", "input": map[string]any{"file_path": "/workspace/x.txt", "content": "new text"}}})
			case "question":
				send(map[string]any{"type": "control_request", "request_id": "native-question", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": "tool-question", "input": map[string]any{"questions": []any{map[string]any{"question": "Which database?", "header": "Database", "options": []any{map[string]any{"label": "SQLite", "description": "A local file"}, map[string]any{"label": "Postgres", "description": "A server"}}}}}}})
			case "background":
				send(map[string]any{"type": "control_request", "request_id": "native-background", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Agent", "tool_use_id": "background-launch", "input": map[string]any{"run_in_background": true, "description": "Background review"}}})
			default:
				id := uuid.NewString()
				send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": id}}})
				send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}})
				send(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Working"}}})
				send(map[string]any{"type": "assistant", "message": map[string]any{"id": id, "stop_reason": "tool_use", "content": []any{map[string]any{"type": "text", "text": "Working"}, map[string]any{"type": "tool_use", "id": "tool-read", "name": "Read", "input": map[string]any{"file_path": "README.md"}}}}})
				send(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-read", "content": "PRIVATE TOOL OUTPUT"}}}})
				result("Answer: " + mode)
			}
		case "control_response":
			var response struct {
				Subtype   string         `json:"subtype"`
				RequestID string         `json:"request_id"`
				Response  map[string]any `json:"response"`
			}
			_ = json.Unmarshal(msg["response"], &response)
			if response.RequestID == "native-approval" {
				if response.Response["behavior"] == "allow" {
					result("approved")
				} else {
					result("denied")
				}
			}
			if response.RequestID == "native-question" {
				updated, _ := response.Response["updatedInput"].(map[string]any)
				answers, _ := updated["answers"].(map[string]any)
				selected, _ := answers["Which database?"].(string)
				result("selected: " + selected)
			}
			if response.RequestID == "native-background" {
				if response.Response["behavior"] != "allow" {
					os.Exit(36)
				}
				result("background allowed")
			}
		}
	}
	os.Exit(0)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	// The socket parent must stay private independently of the runner's umask.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{Command: os.Args[0], WorkingDirectory: root, StateDirectory: filepath.Join(root, "state"), SocketPath: filepath.Join(root, "s"), Env: append(os.Environ(), "MACT_CLAUDE_HELPER=1", "GORACE=atexit_sleep_ms=0", "MACT_CLAUDE_HELPER_ROOT="+root, "HOME="+root, "CLAUDE_CONFIG_DIR="+filepath.Join(root, "native")), StartupTimeout: time.Second, RequestTimeout: 2 * time.Second}
}

func startTest(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	r, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func startThread(t *testing.T, r *Runtime) codexadapter.Thread {
	t.Helper()
	thread, err := r.StartThread(context.Background(), codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return thread
}

func awaitEvent(t *testing.T, r *Runtime, kind string) codexadapter.Event {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-r.Events():
			if event.Kind == kind {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", kind)
		case <-r.Done():
			t.Fatalf("runtime closed: %v", r.Err())
		}
	}
}

func awaitRequest(t *testing.T, r *Runtime) codexadapter.Request {
	t.Helper()
	select {
	case request := <-r.Requests():
		return request
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for Claude permission")
	}
	return codexadapter.Request{}
}

func runTurn(t *testing.T, r *Runtime, id, prompt string) codexadapter.Event {
	t.Helper()
	if _, err := r.StartTurn(context.Background(), id, prompt); err != nil {
		t.Fatal(err)
	}
	return awaitEvent(t, r, "turn_completed")
}

func TestPersistentSessionsStreamingToolsAndHistory(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	turn, err := r.StartTurn(context.Background(), thread.ID, "first")
	if err != nil {
		t.Fatal(err)
	}
	var delta, tool, final bool
	for !final {
		event := awaitAny(t, r)
		if event.Kind == "agent_message_delta" {
			delta = event.Text == "Working"
		}
		if event.Kind == "tool_call_started" {
			tool = strings.Contains(event.Text, "Read")
		}
		if event.Kind == "turn_completed" {
			if event.TurnID != turn.ID {
				t.Fatal("wrong turn attribution")
			}
			final = true
		}
	}
	if !delta || !tool {
		t.Fatalf("live delta=%v tool=%v", delta, tool)
	}
	page, err := r.ReadHistoryPage(context.Background(), thread.ID, "", "asc")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || len(page.Turns[0].UserPrompts) != 1 || page.Turns[0].UserPrompts[0].Text != "first" {
		t.Fatalf("bad history: %+v", page)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	if _, err := r2.ResumeThread(context.Background(), thread.ID, codexadapter.ThreadOptions{}); err != nil {
		t.Fatal(err)
	}
	runTurn(t, r2, thread.ID, "second")
	prompts, err := r2.UserPrompts(context.Background(), thread.ID)
	if err != nil || len(prompts) != 2 {
		t.Fatalf("history after resume: %+v %v", prompts, err)
	}
	log, err := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), `"resume":true`) {
		t.Fatal("second process did not resume native session")
	}
	for _, entry := range strings.Split(string(log), "\n") {
		if strings.Contains(entry, "PRIVATE TOOL OUTPUT") {
			t.Fatal("private tool result escaped into display history")
		}
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, thread.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE TOOL OUTPUT") {
		t.Fatal("private tool result persisted")
	}
	found := false
	for _, line := range bytes.Split(log, []byte{'\n'}) {
		var action struct {
			Launch         bool   `json:"launch"`
			ArtifactPrompt string `json:"artifactPrompt"`
		}
		if json.Unmarshal(line, &action) == nil && action.Launch {
			found = true
			if !strings.Contains(action.ArtifactPrompt, "Markdown link") || !strings.Contains(action.ArtifactPrompt, "Telegram") {
				t.Fatal("native process missing artifact delivery guidance")
			}
		}
	}
	if !found {
		t.Fatal("native launch was not observed")
	}
}

func awaitAny(t *testing.T, r *Runtime) codexadapter.Event {
	t.Helper()
	select {
	case event := <-r.Events():
		return event
	case <-time.After(4 * time.Second):
		t.Fatal("event timeout")
	}
	return codexadapter.Event{}
}

func TestApprovalsQuestionsAndBackgroundPermission(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	if _, err := r.StartTurn(context.Background(), thread.ID, "approval"); err != nil {
		t.Fatal(err)
	}
	request := awaitRequest(t, r)
	if request.Kind != "approval_requested" || request.Command != "printf hello" || len(request.Decisions) != 3 {
		t.Fatalf("bad approval: %+v", request)
	}
	if err := r.WithIdleSettingsUpdate(nil); err == nil {
		t.Fatal("settings changed during approval")
	}
	if err := r.ReplyApproval(context.Background(), request.RequestID, codexadapter.ApprovalResponse{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_completed")
	if _, err := r.StartTurn(context.Background(), thread.ID, "question"); err != nil {
		t.Fatal(err)
	}
	request = awaitRequest(t, r)
	if request.Kind != "input_requested" || len(request.Questions) != 1 || len(request.Questions[0].Choices) != 2 {
		t.Fatalf("bad question: %+v", request)
	}
	if err := r.ReplyAnswers(context.Background(), request.RequestID, map[string][]string{request.Questions[0].ID: {"SQLite"}}); err != nil {
		t.Fatal(err)
	}
	event := awaitEvent(t, r, "agent_message_completed")
	if event.Text != "selected: SQLite" {
		t.Fatalf("native question answer = %s", event.Text)
	}
	awaitEvent(t, r, "turn_completed")
	if _, err := r.StartTurn(context.Background(), thread.ID, "background"); err != nil {
		t.Fatal(err)
	}
	request = awaitRequest(t, r)
	if request.Kind != "approval_requested" || request.Command != "Agent" {
		t.Fatalf("background permission was not forwarded: %+v", request)
	}
	if err := r.ReplyApproval(context.Background(), request.RequestID, codexadapter.ApprovalResponse{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_completed")
}

func TestInterruptAndUnexpectedExitAlwaysSettleTurn(t *testing.T) {
	for _, prompt := range []string{"block", "exit", "malformed"} {
		t.Run(prompt, func(t *testing.T) {
			r := startTest(t, testConfig(t))
			thread := startThread(t, r)
			turn, err := r.StartTurn(context.Background(), thread.ID, prompt)
			if err != nil {
				t.Fatal(err)
			}
			kind := "turn_failed"
			if prompt == "block" {
				if err := r.Interrupt(context.Background(), thread.ID, turn.ID); err != nil {
					t.Fatal(err)
				}
				kind = "turn_interrupted"
			}
			event := awaitEvent(t, r, kind)
			if event.TurnID != turn.ID {
				t.Fatal("wrong terminal turn")
			}
			state, err := r.ReadThreadState(context.Background(), thread.ID)
			if err != nil || state.ActiveTurnID != "" {
				t.Fatalf("turn remained active: %+v %v", state, err)
			}
		})
	}
}

func TestSettingsReloadIsIdleAtomicAndKeepsCredentialsPrivate(t *testing.T) {
	cfg := testConfig(t)
	var generation atomic.Int64
	cfg.LoadSettings = func() (LaunchSettings, error) {
		return LaunchSettings{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": fmt.Sprintf("private-test-token-%d", generation.Load())}, Model: fmt.Sprintf("model-%d", generation.Load())}, nil
	}
	r := startTest(t, cfg)
	thread := startThread(t, r)
	runTurn(t, r, thread.ID, "one")
	if err := r.WithIdleSettingsUpdate(func() error { generation.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	runTurn(t, r, thread.ID, "two")
	log, _ := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if !strings.Contains(string(log), `"model":"model-1"`) {
		t.Fatal("new model did not apply on native resume")
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, thread.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-test-token") {
		t.Fatal("authentication leaked into persisted history")
	}
	if _, err := r.StartTurn(context.Background(), thread.ID, "block"); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := r.WithIdleSettingsUpdate(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("settings update was not fenced against a running turn")
	}
}

func TestQueuedFollowUpStartsOnlyAfterPriorResult(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	first, err := r.StartTurn(context.Background(), thread.ID, "approval")
	if err != nil {
		t.Fatal(err)
	}
	request := awaitRequest(t, r)
	if _, err := r.Steer(context.Background(), thread.ID, first.ID, "queued follow-up"); err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyThreadQueueIdle(context.Background(), thread.ID); err == nil {
		t.Fatal("queue appeared idle before execution")
	}
	log, _ := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if strings.Contains(string(log), "queued follow-up") {
		t.Fatal("follow-up reached native CLI before preceding result")
	}
	if err := r.ReplyApproval(context.Background(), request.RequestID, codexadapter.ApprovalResponse{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	a := awaitEvent(t, r, "turn_completed")
	b := awaitEvent(t, r, "turn_completed")
	if a.TurnID != first.ID || b.TurnID == a.TurnID {
		t.Fatalf("successor attribution: %s %s", a.TurnID, b.TurnID)
	}
	if err := r.VerifyThreadQueueIdle(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	prompts, err := r.UserPrompts(context.Background(), thread.ID)
	if err != nil || len(prompts) != 2 || prompts[1].Text != "queued follow-up" {
		t.Fatalf("queue history: %+v %v", prompts, err)
	}
}

func TestRestartCancelsQueuedInputWithoutReplayingIt(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	first, err := r.StartTurn(context.Background(), thread.ID, "approval")
	if err != nil {
		t.Fatal(err)
	}
	_ = awaitRequest(t, r)
	if _, err := r.Steer(context.Background(), thread.ID, first.ID, "never replay me"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := startTest(t, cfg)
	if err := r2.VerifyThreadQueueIdle(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	page, err := r2.ReadHistoryPage(context.Background(), thread.ID, "", "asc")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page)
	if !strings.Contains(string(encoded), "Queued messages were cancelled") {
		t.Fatal("restart cancellation was not visible in history")
	}
	log, _ := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if strings.Contains(string(log), "never replay me") {
		t.Fatal("queued input replayed on restart")
	}
}

func TestUnixWebSocketSharesThreadEventsAndDisconnectDoesNotCancel(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.SocketPath)
	}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://claude.local/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"id":"hello","method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"id": "turn", "method": "turn/start", "params": map[string]any{"threadId": thread.ID, "input": []any{map[string]any{"type": "text", "text": "approval"}}}})
	if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	request := awaitRequest(t, r)
	_ = ws.CloseNow()
	if err := r.ReplyApproval(context.Background(), request.RequestID, codexadapter.ApprovalResponse{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_completed")
	if mode, err := os.Stat(cfg.SocketPath); err != nil || mode.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions: %v %v", mode, err)
	}
}

func TestStartupFailureUnsupportedMethodsAndInputSecurity(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_HANG_INIT=1")
	cfg.StartupTimeout = 50 * time.Millisecond
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if _, err := r.StartTurn(context.Background(), thread.ID, "normal"); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_failed")
	if r.Supports("thread/goal/set") || r.Supports("account/rateLimits/read") {
		t.Fatal("Codex-only capabilities advertised")
	}
	if _, err := r.ReadGoal(context.Background(), thread.ID); !errors.Is(err, codexadapter.ErrMethodUnavailable) {
		t.Fatalf("unsupported goal returned %v", err)
	}
	for _, args := range [][]string{{"--resume=foreign"}, {"--input-format=text"}, {"--no-session-persistence"}} {
		bad := testConfig(t)
		bad.Args = args
		if _, err := Start(context.Background(), bad); err == nil {
			t.Fatalf("accepted protocol override %v", args)
		}
	}
	if _, err := convertInput(params{Input: []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URL  string `json:"url"`
	}{{Type: "image", URL: "file:///etc/passwd"}}}); err == nil {
		t.Fatal("accepted local file image")
	}
}

func TestProviderDiagnosticDoesNotBecomeAnAssistantMessage(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if _, err := r.StartTurn(context.Background(), thread.ID, "provider-error"); err != nil {
		t.Fatal(err)
	}
	event := awaitEvent(t, r, "turn_failed")
	if strings.Contains(event.PublicErrorMessage, "PRIVATE") {
		t.Fatal("provider diagnostic became a public turn error")
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDirectory, thread.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE PROVIDER") {
		t.Fatal("provider diagnostic persisted as assistant text")
	}
}

func TestPublicProviderErrorUsesConfiguredRedactor(t *testing.T) {
	cfg := testConfig(t)
	cfg.Redact = func(value string) string { return strings.ReplaceAll(value, "PRIVATE", "[redacted]") }
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if _, err := r.StartTurn(context.Background(), thread.ID, "provider-error"); err != nil {
		t.Fatal(err)
	}
	event := awaitEvent(t, r, "turn_failed")
	if event.PublicErrorMessage != "[redacted] PROVIDER AUTH RESPONSE" {
		t.Fatalf("public error=%q", event.PublicErrorMessage)
	}
}

func TestFileApprovalDescribesTheActualOperation(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	if _, err := r.StartTurn(context.Background(), thread.ID, "file-approval"); err != nil {
		t.Fatal(err)
	}
	request := awaitRequest(t, r)
	if request.ApprovalType != "file_change" || !strings.Contains(request.Summary, "/workspace/x.txt") || !strings.Contains(request.Summary, "new text") {
		t.Fatalf("approval lacks actual operation: %+v", request)
	}
	if err := r.ReplyApproval(context.Background(), request.RequestID, codexadapter.ApprovalResponse{Decision: "decline"}); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_completed")
}
