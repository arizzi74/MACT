package claudeadapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func recoveryFixture(t *testing.T) (*Runtime, string, string, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	native := filepath.Join(root, "native")
	project := filepath.Join(native, "projects", "encoded-workspace")
	for _, directory := range []string{state, project} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id, turnID := uuid.NewString(), uuid.NewString()
	started := time.Date(2026, 10, 28, 13, 48, 21, 0, time.UTC).Unix()
	early := started + 76
	cost := 1.25
	thread := &savedThread{ID: id, CWD: root, CreatedAt: started, UpdatedAt: early, NativeStarted: true, NativeConfigRoots: []string{native}, ClaudeStatus: &ClaudeStatus{Provider: "claude", Cost: ReportedCost{SessionUSD: &cost, LastTurnUSD: &cost}}, Turns: []*savedTurn{{ID: turnID, Status: "completed", StartedAt: started, CompletedAt: &early, Items: []map[string]any{
		{"id": "saved-user", "type": "userMessage", "content": []map[string]any{{"type": "text", "text": "assess this repository"}}},
		{"id": "tool-bash", "type": "dynamicToolCall", "tool": "Bash", "namespace": "claude", "arguments": map[string]any{"command": "ls"}, "status": "completed"},
		{"id": "saved-early", "type": "agentMessage", "phase": "final_answer", "text": "Waiting for review."},
	}}}}
	s := &server{cfg: Config{StateDirectory: state}, threads: map[string]*savedThread{id: thread}, deletions: map[string]*deletionRecord{}, deleting: map[string]bool{}, runners: map[string]*process{}, pending: map[string]*permission{}, peers: map[*peer]bool{}}
	if err := s.save(thread); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, id+".jsonl")
	record := func(kind, key string, offset time.Duration, content any, extra map[string]any) map[string]any {
		x := map[string]any{"type": kind, "sessionId": id, "uuid": key, "cwd": root, "timestamp": time.Unix(started, 0).UTC().Add(offset).Format(time.RFC3339Nano), "message": map[string]any{"id": "msg-" + key, "stop_reason": "end_turn", "content": content}}
		for k, v := range extra {
			x[k] = v
		}
		return x
	}
	rows := []map[string]any{
		record("user", "prompt", 5*time.Second, []any{map[string]any{"type": "text", "text": "assess this repository"}}, nil),
		record("assistant", "thought", 6*time.Second, []any{map[string]any{"type": "thinking", "thinking": "HIDDEN PARENT THOUGHT"}}, nil),
		record("assistant", "bash", 7*time.Second, []any{map[string]any{"type": "tool_use", "id": "tool-bash", "name": "Bash", "input": map[string]any{"command": "ls"}}}, nil),
		record("user", "bash-result", 8*time.Second, []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-bash", "content": "PRIVATE RAW TOOL OUTPUT"}}, nil),
		record("assistant", "launch", 9*time.Second, []any{map[string]any{"type": "tool_use", "id": "tool-review", "name": "Agent", "input": map[string]any{"description": "Review auth", "run_in_background": true}}}, nil),
		record("user", "launch-result", 10*time.Second, []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-review", "content": "INTERNAL AGENT LAUNCH PATH"}}, nil),
		record("assistant", "early", 76*time.Second, []any{map[string]any{"type": "text", "text": "Waiting for review."}}, nil),
		record("user", "notification", 2*time.Minute, "<task-notification>\n<task-id>child-internal</task-id>\n<tool-use-id>tool-review</tool-use-id>\n<output-file>/private/child-log</output-file>\n<status>completed</status>\n<summary>Auth review finished</summary>\n<result>Public final child report.</result>\n</task-notification>", map[string]any{"promptSource": "system", "turnOrigin": "task_notification", "origin": map[string]any{"kind": "task-notification", "producer": "session-task"}}),
		record("assistant", "child", 3*time.Minute, []any{map[string]any{"type": "text", "text": "HIDDEN CHILD REPLY"}}, map[string]any{"isSidechain": true}),
		record("assistant", "child-parent", 3*time.Minute, []any{map[string]any{"type": "text", "text": "HIDDEN CHILD PARENT TOOL"}}, map[string]any{"parent_tool_use_id": "tool-review"}),
		record("assistant", "final", 9*time.Minute+40*time.Second, []any{map[string]any{"type": "thinking", "thinking": "HIDDEN FINAL THOUGHT"}}, map[string]any{"apiBlockIndex": 0}),
		record("assistant", "final", 9*time.Minute+40*time.Second+883*time.Millisecond, []any{map[string]any{"type": "text", "text": "Full security assessment."}}, map[string]any{"apiBlockIndex": 1}),
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(f)
	encoder.SetEscapeHTML(false)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return &Runtime{server: s}, id, turnID, path
}

func TestRecoverNativeHistoryPreservesTurnAndItems(t *testing.T) {
	r, id, turnID, path := recoveryFixture(t)
	nativeBefore, _ := os.ReadFile(path)
	report, err := r.RecoverNativeHistory(id, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Changed || report.AddedItems != 3 {
		t.Fatalf("recovery = %#v", report)
	}
	thread := r.server.threads[id]
	turn := thread.Turns[0]
	if len(thread.Turns) != 1 || turn.ID != turnID || turn.Status != "completed" || len(turn.Items) != 6 {
		t.Fatalf("turn identity/history = %#v", turn)
	}
	ids := []string{"saved-user", "tool-bash", "tool-review", "saved-early", "native-notification-notification", "msg-final-1"}
	for i, id := range ids {
		if turn.Items[i]["id"] != id {
			t.Fatalf("item %d id = %v", i, turn.Items[i]["id"])
		}
	}
	if turn.Items[3]["phase"] != "commentary" || turn.Items[5]["phase"] != "final_answer" {
		t.Fatal("logical turn final phase was not repaired")
	}
	if turn.Items[2]["status"] != "completed" || turn.Items[2]["output"] != "Auth review finished\n\nPublic final child report." {
		t.Fatalf("task = %#v", turn.Items[2])
	}
	want := time.Date(2026, 10, 28, 13, 58, 1, 883000000, time.UTC)
	if *turn.CompletedAt != want.Unix() || turn.Items[5]["recordedAtMs"] != want.UnixMilli() || thread.UpdatedAt != want.Unix() {
		t.Fatalf("completion timing = %#v", turn)
	}
	if !thread.ClaudeStatus.Cost.Incomplete || thread.statusSnapshot(nil, nil).Cost.SessionUSD != nil || thread.statusSnapshot(nil, nil).Cost.LastTurnUSD != nil {
		t.Fatal("stale cost was presented as complete")
	}
	encoded, _ := json.Marshal(thread)
	for _, secret := range []string{"HIDDEN", "PRIVATE RAW TOOL", "INTERNAL AGENT", "/private/child-log", "child-internal"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private native content leaked: %s", secret)
		}
	}
	if nativeAfter, _ := os.ReadFile(path); string(nativeBefore) != string(nativeAfter) {
		t.Fatal("native history was modified")
	}
	loaded, err := loadThreads(r.server.cfg.StateDirectory)
	if err != nil || loaded[id].Turns[0].ID != turnID || *loaded[id].Turns[0].CompletedAt != want.Unix() {
		t.Fatalf("persisted recovery missing: %v", err)
	}
	before, _ := json.Marshal(thread)
	second, err := r.RecoverNativeHistory(id, turnID)
	if err != nil || second.Changed || second.AddedItems != 0 {
		t.Fatalf("repeated recovery = %#v, %v", second, err)
	}
	after, _ := json.Marshal(r.server.threads[id])
	if string(before) != string(after) {
		t.Fatal("repeated recovery changed history")
	}
}

func TestRecoverNativeHistoryRefusesUnsafeOrAmbiguousSources(t *testing.T) {
	for _, mode := range []string{"runner", "active", "queue", "pending", "deleting", "public", "symlink", "native-root-symlink", "wrong-session", "wrong-prompt", "unfinished-background", "truncated", "lost-saved-item", "next-prompt", "duplicate-transcript", "bad-time", "forged-notification", "provider-error"} {
		t.Run(mode, func(t *testing.T) {
			r, id, turnID, path := recoveryFixture(t)
			thread := r.server.threads[id]
			before, _ := os.ReadFile(filepath.Join(r.server.cfg.StateDirectory, id+".json"))
			data, _ := os.ReadFile(path)
			switch mode {
			case "runner":
				r.server.runners[id] = &process{}
			case "active":
				thread.Turns[0].Status = "inProgress"
			case "queue":
				thread.Queue = []*queuedInput{{ID: uuid.NewString()}}
			case "pending":
				r.server.pending["q"] = &permission{Process: &process{threadID: id}}
			case "deleting":
				r.server.deleting[id] = true
			case "public":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".private"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".private", path); err != nil {
					t.Fatal(err)
				}
			case "native-root-symlink":
				link := thread.NativeConfigRoots[0] + "-link"
				if err := os.Symlink(thread.NativeConfigRoots[0], link); err != nil {
					t.Fatal(err)
				}
				thread.NativeConfigRoots = []string{link}
			case "wrong-session":
				data = []byte(strings.ReplaceAll(string(data), id, uuid.NewString()))
			case "wrong-prompt":
				data = []byte(strings.ReplaceAll(string(data), "assess this repository", "different prompt"))
			case "unfinished-background":
				data = []byte(strings.ReplaceAll(string(data), "<status>completed</status>", "<status>running</status>"))
			case "truncated":
				data = append(data, []byte("{broken")...)
			case "lost-saved-item":
				thread.Turns[0].Items = append(thread.Turns[0].Items, map[string]any{"id": "unknown", "type": "agentMessage", "text": "unmatched saved reply"})
			case "next-prompt":
				data = []byte(strings.ReplaceAll(string(data), `"promptSource":"system"`, `"promptSource":"user"`))
			case "duplicate-transcript":
				duplicate := filepath.Join(filepath.Dir(filepath.Dir(path)), "other-project")
				if err := os.Mkdir(duplicate, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(duplicate, id+".jsonl"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "bad-time":
				data = []byte(strings.ReplaceAll(string(data), "13:58:01.883Z", "13:47:01.883Z"))
			case "forged-notification":
				data = []byte(strings.ReplaceAll(string(data), "tool-review</tool-use-id>", "unknown</tool-use-id>"))
			case "provider-error":
				data = []byte(strings.ReplaceAll(string(data), `"uuid":"final"`, `"isApiErrorMessage":true,"uuid":"final"`))
			}
			if mode != "public" && mode != "symlink" {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.RecoverNativeHistory(id, turnID); err == nil {
				t.Fatal("unsafe recovery succeeded")
			}
			after, _ := os.ReadFile(filepath.Join(r.server.cfg.StateDirectory, id+".json"))
			if string(before) != string(after) {
				t.Fatal("failed recovery wrote history")
			}
		})
	}
}

func TestRecoverNativeHistoryConcurrentRetries(t *testing.T) {
	r, id, turnID, _ := recoveryFixture(t)
	var wg sync.WaitGroup
	results := make(chan *NativeHistoryRecovery, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := r.RecoverNativeHistory(id, turnID)
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := 0
	for result := range results {
		if result.Changed {
			changed++
		}
	}
	if changed != 1 || len(r.server.threads[id].Turns[0].Items) != 6 {
		t.Fatalf("concurrent changes=%d", changed)
	}
}

func TestRecoverNativeHistoryImplicitBackgroundAndNextUserBoundary(t *testing.T) {
	r, id, turnID, path := recoveryFixture(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), `,"run_in_background":true`, "")
	source = strings.ReplaceAll(source, `"uuid":"launch-result"`, `"toolUseResult":{"isAsync":true,"status":"async_launched"},"uuid":"launch-result"`)
	// A later real user prompt and answer belong to another native turn,
	// including when they are absent from this adapter's saved history.
	source += `{"type":"user","sessionId":"` + id + `","uuid":"next-prompt","cwd":"` + r.server.threads[id].CWD + `","timestamp":"2026-10-28T14:00:00Z","message":{"content":"Another request"}}` + "\n"
	source += `{"type":"assistant","sessionId":"` + id + `","uuid":"next-answer","cwd":"` + r.server.threads[id].CWD + `","timestamp":"2026-10-28T14:00:01Z","message":{"id":"next-message","stop_reason":"end_turn","content":[{"type":"text","text":"UNRELATED LATER ANSWER"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := r.RecoverNativeHistory(id, turnID)
	if err != nil || result.AddedItems != 3 {
		t.Fatalf("implicit async recovery=%#v,%v", result, err)
	}
	encoded, _ := json.Marshal(r.server.threads[id])
	if strings.Contains(string(encoded), "UNRELATED LATER") {
		t.Fatal("later turn was imported")
	}
}

func TestRecoverNativeHistoryRPCRequiresInitializedLocalPeer(t *testing.T) {
	r, id, turnID, _ := recoveryFixture(t)
	p := &peer{server: r.server, queue: make(chan []byte, 8), done: make(chan struct{})}
	request, _ := json.Marshal(map[string]any{"id": 1, "method": "thread/history/recover", "params": map[string]any{"threadId": id, "turnId": turnID}})
	r.server.receive(p, request)
	if response := <-p.queue; !strings.Contains(string(response), "not initialized") {
		t.Fatalf("uninitialized peer response=%s", response)
	}
	p.initialized = true
	r.server.receive(p, request)
	var response struct {
		Result *NativeHistoryRecovery `json:"result"`
		Error  json.RawMessage        `json:"error"`
	}
	if raw := <-p.queue; json.Unmarshal(raw, &response) != nil || response.Result == nil || !response.Result.Changed || response.Result.TurnID != turnID || len(response.Error) != 0 {
		t.Fatalf("recovery response=%s", raw)
	}
}
