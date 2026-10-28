package claudeadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type lifecycleInput struct{ writes chan []byte }

func (w *lifecycleInput) Write(data []byte) (int, error) {
	w.writes <- append([]byte(nil), data...)
	return len(data), nil
}
func (w *lifecycleInput) Close() error { return nil }

type lifecycleFixture struct {
	s      *server
	p      *process
	thread *savedThread
	turn   *savedTurn
	input  *lifecycleInput
	peer   *peer
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	turn := &savedTurn{ID: uuid.NewString(), Status: "inProgress", Items: []map[string]any{}}
	thread := &savedThread{ID: uuid.NewString(), CWD: root, Turns: []*savedTurn{turn}}
	s := &server{cfg: Config{StateDirectory: root}, threads: map[string]*savedThread{thread.ID: thread}, runners: map[string]*process{}, pending: map[string]*permission{}, peers: map[*peer]bool{}, ctx: context.Background()}
	input := &lifecycleInput{writes: make(chan []byte, 8)}
	p := &process{server: s, threadID: thread.ID, in: input, out: io.NopCloser(strings.NewReader("")), ready: make(chan struct{}), done: make(chan struct{}), controls: map[string]chan controlReply{}, tools: map[string]map[string]any{}}
	close(p.ready)
	s.runners[thread.ID] = p
	p.beginLifecycle(thread, turn.ID)
	p.lifecycle.submitted = true
	peer := &peer{server: s, queue: make(chan []byte, 256), done: make(chan struct{}), initialized: true}
	s.peers[peer] = true
	return &lifecycleFixture{s, p, thread, turn, input, peer}
}

func (f *lifecycleFixture) feed(raw string) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.p.handle([]byte(raw))
}

func (f *lifecycleFixture) state(state string) {
	f.feed(`{"type":"system","subtype":"session_state_changed","state":"` + state + `"}`)
}

func (f *lifecycleFixture) task(subtype, id, run, status string) {
	f.feed(fmt.Sprintf(`{"type":"system","subtype":%q,"task_id":%q,"run_id":%q,"task_type":"local_agent","description":"Review repository","status":%q,"patch":{"status":%q}}`, subtype, id, run, status, status))
}

func (f *lifecycleFixture) result(id, originRun string, total float64) {
	f.feed(fmt.Sprintf(`{"type":"result","subtype":"success","uuid":%q,"total_cost_usd":%v,"origin":{"kind":"task-notification","runId":%q}}`, id, total, originRun))
}

func (f *lifecycleFixture) assistant(id, text string) {
	f.feed(fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"stop_reason":"end_turn","content":[{"type":"text","text":%q}]}}`, id, text))
}

func (f *lifecycleFixture) active(t *testing.T) {
	t.Helper()
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if f.thread.active() != f.turn || f.turn.CompletedAt != nil {
		t.Fatalf("request settled before all background work and parent consolidation: %+v", f.turn)
	}
}

func (f *lifecycleFixture) completed(t *testing.T) {
	t.Helper()
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if f.turn.Status != "completed" || f.turn.CompletedAt == nil {
		t.Fatalf("request did not settle: %+v", f.turn)
	}
}

// Protocol ordering follows the real 2026-10-28 incident: four implicit async
// Agent launches, an interim parent end_turn, three notification continuations,
// then a final parent consolidation after the fourth notification. Tool/result
// and session-state wire fields follow the official SDK 0.3.310 declarations;
// the incident's persisted transcript does not contain stdout result/idle frames.
func TestBackgroundAgentNotificationsRemainInOriginalUserTurn(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	for i := 1; i <= 4; i++ {
		f.task("task_started", fmt.Sprintf("review-%d", i), fmt.Sprintf("run-%d", i), "running")
	}
	f.assistant("initial", "All four reviewers are running. I will compile their findings.")
	f.result("initial-result", "", 1)
	f.active(t)
	if err := (&Runtime{server: f.s}).WithIdleSettingsUpdate(nil); err == nil {
		t.Fatal("settings changed during background work")
	}
	for i := 1; i <= 4; i++ {
		f.task("task_notification", fmt.Sprintf("review-%d", i), fmt.Sprintf("run-%d", i), "completed")
		f.assistant(fmt.Sprintf("parent-%d", i), fmt.Sprintf("Review %d consolidated.", i))
		f.result(fmt.Sprintf("notification-result-%d", i), fmt.Sprintf("run-%d", i), 1+float64(i)/10)
		f.active(t)
	}
	// CLI can flush an earlier held-back result after the final parent result.
	f.result("held-back-original", "", 1.4)
	f.active(t)
	f.state("idle")
	f.completed(t)
	if len(f.thread.Turns) != 1 {
		t.Fatal("notification continuations became artificial user turns")
	}
	var text []string
	for _, item := range f.turn.Items {
		if item["type"] == "agentMessage" {
			text = append(text, item["text"].(string))
		}
	}
	if len(text) != 5 || text[4] != "Review 4 consolidated." {
		t.Fatalf("late parent output was lost: %q", text)
	}
	wantNumber(t, f.thread.ClaudeStatus.Cost.LastTurnUSD, 1.4)
	count := 0
	for len(f.peer.queue) > 0 {
		var event struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(<-f.peer.queue, &event)
		if event.Method == "turn/completed" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("published %d terminal events", count)
	}
}

func TestBackgroundLevelReplacementAndDelayedBookends(t *testing.T) {
	for _, order := range []string{"terminal-first", "level-first", "idle-before-final-level", "missing-terminal"} {
		t.Run(order, func(t *testing.T) {
			f := newLifecycleFixture(t)
			f.state("running")
			f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"agent","run_id":"run-1","task_type":"local_agent"}]}`)
			f.task("task_started", "agent", "run-1", "running")
			f.result("initial", "", 1)
			if order == "terminal-first" || order == "idle-before-final-level" {
				f.task("task_notification", "agent", "run-1", "completed")
			}
			if order != "idle-before-final-level" {
				f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
			}
			f.assistant("final", "The report is complete.")
			f.result("consolidated", "run-1", 1.2)
			f.active(t)
			f.state("idle")
			if order == "idle-before-final-level" {
				f.active(t)
				f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
			}
			f.completed(t)
			// A late terminal/progress bookend cannot reopen work after the
			// authoritative empty snapshot and completed native parent loop.
			f.task("task_notification", "agent", "run-1", "completed")
			f.task("task_progress", "agent", "run-1", "running")
			if f.p.hasBackgroundWork() {
				t.Fatal("late bookends resurrected a completed task")
			}
		})
	}
}

func TestBackgroundShellRequiresFreshParentConsolidation(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.feed(`{"type":"system","subtype":"task_started","task_id":"shell","run_id":"shell-run","task_type":"local_bash"}`)
	f.result("initial", "", .1)
	// Native idle can coexist with a live Bash task. It is too old to certify
	// that the parent has read and consolidated that task's eventual output.
	f.state("idle")
	f.active(t)
	f.feed(`{"type":"system","subtype":"task_notification","task_id":"shell","run_id":"shell-run","status":"completed"}`)
	f.active(t)
	f.assistant("final", "The background command succeeded.")
	f.result("shell-final", "shell-run", .2)
	f.active(t)
	f.state("idle")
	f.completed(t)
}

func TestBackgroundFreshStartBeforeLevelSnapshotKeepsTurnActive(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
	f.feed(`{"type":"system","subtype":"task_started","task_id":"shell","run_id":"run","task_type":"local_bash"}`)
	f.result("initial", "", 1)
	f.state("idle")
	f.active(t)
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"shell","run_id":"run","task_type":"local_bash"}]}`)
	f.feed(`{"type":"system","subtype":"task_notification","task_id":"shell","run_id":"run","status":"completed"}`)
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[]}`)
	f.assistant("final", "Background command consolidated.")
	f.result("final-result", "run", 1.2)
	f.state("idle")
	f.completed(t)
}

func TestBackgroundQueuedInputAndStaleFrames(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.task("task_started", "agent", "run-1", "running")
	f.thread.Queue = []*queuedInput{{ID: uuid.NewString(), Content: []map[string]any{{"type": "text", "text": "next user request"}}}}
	f.result("initial", "", 1)
	select {
	case data := <-f.input.writes:
		t.Fatalf("queued input was submitted during background work: %s", data)
	default:
	}
	f.task("task_notification", "agent", "run-1", "completed")
	f.assistant("consolidation", "Final report.")
	f.result("final", "run-1", 1.2)
	f.active(t)
	f.state("idle")
	var nextID string
	select {
	case data := <-f.input.writes:
		var input struct {
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(data, &input); err != nil {
			t.Fatal(err)
		}
		nextID = input.UUID
	case <-time.After(time.Second):
		t.Fatal("queued input did not start after consolidation")
	}
	f.s.mu.Lock()
	if f.thread.active() == nil || f.thread.active().ID != nextID || nextID == f.turn.ID {
		t.Fatal("queued turn attribution is invalid")
	}
	f.s.mu.Unlock()
	f.result("initial", "", 1) // Duplicate intermediate result, not just the final UUID.
	f.feed(fmt.Sprintf(`{"type":"result","subtype":"success","uuid":"old-attributed","user_message_uuid":%q}`, f.turn.ID))
	f.feed(`{"type":"assistant","origin":{"kind":"task-notification","runId":"run-1"},"message":{"id":"late","content":[{"type":"text","text":"stale output"}]}}`)
	f.assistant("consolidation", "Final report.") // Unattributed duplicate of an older main reply.
	f.feed(`{"type":"assistant","uuid":"late-unique-block","message":{"id":"consolidation","content":[{"type":"text","text":"a later block from the old reply"}]}}`)
	f.task("task_progress", "agent", "run-1", "running")
	f.state("idle") // An unattributed old idle must not settle a new send without a result.
	f.s.mu.Lock()
	if f.thread.active() == nil || f.thread.active().ID != nextID || len(f.thread.active().Items) != 1 {
		t.Fatal("a preceding request's late frame modified its queued successor")
	}
	f.s.mu.Unlock()
	f.state("running")
	f.result("new-user-result", "", 1.3)
	f.s.mu.Lock()
	if f.thread.active() == nil || f.thread.active().ID != nextID {
		t.Fatal("a preceding idle settled a fresh running query")
	}
	f.s.mu.Unlock()
	f.state("idle")
}

func TestBackgroundNestedFramesAndResumedTaskRuns(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.task("task_started", "agent", "002", "running")
	f.task("task_notification", "agent", "001", "completed")
	f.feed(`{"type":"assistant","parent_tool_use_id":"child-tool","message":{"id":"child","content":[{"type":"text","text":"private child reasoning"}]}}`)
	f.feed(`{"type":"result","parent_tool_use_id":"child-tool","subtype":"success","uuid":"child-result"}`)
	f.result("parent-initial", "", 1)
	f.active(t)
	if !f.p.hasBackgroundWork() {
		t.Fatal("an older task run's completion ended the current run")
	}
	f.task("task_notification", "agent", "002", "completed")
	f.assistant("parent-final", "Public consolidated result.")
	f.result("parent-final-result", "002", 1.2)
	f.state("idle")
	f.completed(t)
	encoded, _ := json.Marshal(f.turn.Items)
	if strings.Contains(string(encoded), "private child") {
		t.Fatal("subagent text leaked into the parent conversation")
	}
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"agent","run_id":"001","task_type":"local_agent"}]}`)
	if f.p.hasBackgroundWork() {
		t.Fatal("a late older-run level resurrected completed work")
	}
}

func TestBackgroundLegacyForegroundAndNotificationResults(t *testing.T) {
	t.Run("foreground", func(t *testing.T) {
		f := newLifecycleFixture(t)
		f.result("foreground", "", 1)
		f.completed(t)
	})
	t.Run("model-less-after-state-support", func(t *testing.T) {
		f := newLifecycleFixture(t)
		f.p.stateEvents = true
		f.state("idle") // A previous query's unattributed idle is ignored.
		f.result("local-command", "", 0)
		f.completed(t)
	})
	t.Run("background", func(t *testing.T) {
		f := newLifecycleFixture(t)
		f.task("task_started", "agent", "", "running")
		f.result("initial", "", 1)
		f.active(t)
		f.task("task_notification", "agent", "", "completed")
		f.active(t)
		f.assistant("final", "The review is complete.")
		f.result("final-result", "", 1.2)
		f.completed(t)
	})
	t.Run("foreground-agent-completion", func(t *testing.T) {
		f := newLifecycleFixture(t)
		f.feed(`{"type":"system","subtype":"task_started","task_id":"foreground-agent","tool_use_id":"launch","task_type":"local_agent"}`)
		f.feed(`{"type":"user","tool_use_result":{"status":"completed","agentId":"foreground-agent"},"message":{"content":[{"type":"tool_result","tool_use_id":"launch"}]}}`)
		f.result("foreground-agent-result", "", 1)
		f.completed(t)
	})
}

func TestBackgroundNewParentOutputRequiresItsOwnResult(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.task("task_started", "agent", "run", "running")
	f.result("initial", "", 1)
	f.task("task_notification", "agent", "run", "completed")
	f.assistant("final", "Consolidated review.")
	f.state("idle")
	f.active(t)
	f.result("final-result", "run", 1.1)
	f.completed(t)
}

func TestBackgroundResultOnlyFinalReplySurvivesEarlierInterimText(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.task("task_started", "agent", "run", "running")
	f.assistant("interim", "Waiting for the reviewer.")
	f.result("initial", "", 1)
	f.task("task_notification", "agent", "run", "completed")
	f.feed(`{"type":"assistant","uuid":"commentary","message":{"id":"last-commentary","stop_reason":"tool_use","content":[{"type":"text","text":"Checking one last detail."}]}}`)
	f.feed(`{"type":"result","subtype":"success","uuid":"final","result":"Complete final report."}`)
	f.state("idle")
	f.completed(t)
	if f.turn.Items[len(f.turn.Items)-1]["text"] != "Complete final report." {
		t.Fatal("a result-only final answer was hidden by an earlier interim reply")
	}
}

// SDKAssistantMessage is explicitly per completed content block; the wrappers
// below reproduce the official protocol, including thinking and multiple tools
// sharing the same message.id, with their own wrapper UUIDs.
func TestBackgroundAssistantBlockFramesPreserveTextToolsAndStreamIdentity(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.feed(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"native-response"}}}`)
	f.feed(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}}`)
	f.feed(`{"type":"assistant","uuid":"thinking-block","message":{"id":"native-response","stop_reason":null,"content":[{"type":"thinking","thinking":"PRIVATE REASONING"}]}}`)
	f.feed(`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"text"}}}`)
	f.feed(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Visible section"}}}`)
	f.feed(`{"type":"assistant","uuid":"first-text-block","message":{"id":"native-response","stop_reason":null,"content":[{"type":"text","text":"Visible section"}]}}`)
	f.feed(`{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool-one"}}}`)
	f.feed(`{"type":"assistant","uuid":"tool-block-one","message":{"id":"native-response","stop_reason":null,"content":[{"type":"tool_use","id":"tool-one","name":"Read","input":{"file_path":"one"}}]}}`)
	f.feed(`{"type":"assistant","uuid":"tool-block-two","message":{"id":"native-response","stop_reason":null,"content":[{"type":"tool_use","id":"tool-two","name":"Read","input":{"file_path":"two"}}]}}`)
	f.feed(`{"type":"stream_event","event":{"type":"content_block_start","index":3,"content_block":{"type":"text"}}}`)
	f.feed(`{"type":"assistant","uuid":"last-text-block","message":{"id":"native-response","stop_reason":null,"content":[{"type":"text","text":"Final report"}]}}`)
	f.feed(`{"type":"assistant","uuid":"first-text-block","message":{"id":"native-response","stop_reason":null,"content":[{"type":"text","text":"Visible section"}]}}`)
	if len(f.turn.Items) != 4 || f.turn.Items[0]["id"] != "native-response-1" || f.turn.Items[3]["id"] != "native-response-3" {
		t.Fatalf("blocks dropped, duplicated, or detached from streamed items: %+v", f.turn.Items)
	}
	f.feed(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-one"},{"type":"tool_result","tool_use_id":"tool-two"}]}}`)
	f.feed(`{"type":"result","subtype":"success","uuid":"result","result":"Visible section\nFinal report"}`)
	f.state("idle")
	f.completed(t)
	if len(f.turn.Items) != 4 {
		t.Fatalf("aggregate result duplicated per-block text: %+v", f.turn.Items)
	}
	encoded, _ := json.Marshal(f.turn.Items)
	if strings.Contains(string(encoded), "PRIVATE REASONING") {
		t.Fatal("thinking was exposed")
	}
}

func TestBackgroundTerminalStatesAndInterruption(t *testing.T) {
	for _, status := range []string{"completed", "failed", "stopped", "killed"} {
		t.Run(status, func(t *testing.T) {
			f := newLifecycleFixture(t)
			f.state("running")
			f.task("task_started", "agent", "run", "running")
			f.result("initial", "", 1)
			f.task("task_updated", "agent", "run", status)
			f.active(t)
			f.assistant("final", "All task outcomes consolidated.")
			f.result("final-result", "run", 1.2)
			f.state("idle")
			f.completed(t)
			if f.p.hasBackgroundWork() {
				t.Fatal("terminal task remained active")
			}
		})
	}
	t.Run("interrupt-retires-process", func(t *testing.T) {
		f := newLifecycleFixture(t)
		f.state("running")
		f.task("task_started", "agent", "run", "running")
		f.thread.Queue = []*queuedInput{{ID: uuid.NewString()}}
		f.p.interrupting = true
		f.feed(`{"type":"result","subtype":"error_during_execution","is_error":true}`)
		if f.turn.Status != "interrupted" || f.s.runners[f.thread.ID] != nil || len(f.thread.Queue) != 0 || f.p.hasBackgroundWork() {
			t.Fatal("interruption left native background work or queued input admitted")
		}
	})
}

func TestBackgroundImplicitLaunchAndAmbientHousekeeping(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.feed(`{"type":"user","tool_use_result":{"isAsync":true,"status":"async_launched","agentId":"implicit-agent","description":"Review"},"message":{"content":[{"type":"tool_result","tool_use_id":"launch"}]}}`)
	f.result("initial", "", 1)
	f.active(t)
	// The structured tool result may precede a task_started carrying its run id.
	f.task("task_started", "implicit-agent", "run-1", "running")
	f.task("task_notification", "implicit-agent", "run-1", "completed")
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"watcher","task_type":"local_bash","ambient":true}]}`)
	f.assistant("final", "The implicit background review completed.")
	f.result("final-result", "run-1", 1.1)
	f.state("idle")
	f.completed(t)
	var tasks int
	for _, item := range f.turn.Items {
		if item["tool"] == "backgroundTask" {
			tasks++
			if item["status"] != "completed" {
				t.Fatalf("implicit task left in progress: %+v", item)
			}
		}
	}
	if tasks != 1 || f.p.hasBackgroundWork() {
		t.Fatalf("duplicate implicit task or housekeeping counted as work: %d", tasks)
	}
}

func TestBackgroundLevelRefreshesAmbientMembership(t *testing.T) {
	f := newLifecycleFixture(t)
	f.state("running")
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"watcher","task_type":"local_bash","ambient":true}]}`)
	if f.p.hasBackgroundWork() {
		t.Fatal("ambient task counted as request work")
	}
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"watcher","task_type":"local_bash","ambient":false}]}`)
	if !f.p.hasBackgroundWork() || len(f.turn.Items) != 1 {
		t.Fatal("a newly nonambient task was not tracked and displayed")
	}
	f.result("initial", "", 1)
	f.feed(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"watcher","task_type":"local_bash","ambient":true}]}`)
	f.state("idle")
	f.completed(t)
}

func TestBackgroundRestartSettlesPersistedTaskRows(t *testing.T) {
	f := newLifecycleFixture(t)
	f.task("task_started", "agent", "run", "running")
	r := startTest(t, Config{Command: "unused-until-next-prompt", WorkingDirectory: f.thread.CWD, StateDirectory: f.s.cfg.StateDirectory})
	r.server.mu.Lock()
	defer r.server.mu.Unlock()
	turn := r.server.threads[f.thread.ID].Turns[0]
	if turn.Status != "interrupted" || len(turn.Items) != 1 || turn.Items[0]["status"] != "interrupted" {
		t.Fatalf("restart left stale background activity in history: %+v", turn)
	}
}
