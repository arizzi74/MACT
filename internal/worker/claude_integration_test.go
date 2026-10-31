package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/workerdb"
)

// A local subprocess exercises the real Claude CLI control and stream format.
// It never opens a network connection or calls an installed model provider.
func TestClaudeWorkerCLIHelper(t *testing.T) {
	if os.Getenv("MACT_CLAUDE_WORKER_TEST_HELPER") != "1" {
		return
	}
	for _, arg := range os.Args {
		if arg == "--version" {
			fmt.Println("9.8.7 (Claude Code)")
			os.Exit(0)
		}
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	spent := 0.0
	finish := func(text string) {
		spent += 0.125
		send(map[string]any{"type": "assistant", "message": map[string]any{"id": uuid.NewString(), "model": "test", "usage": map[string]int{"input_tokens": 1200, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 0}, "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": text}}}})
		send(map[string]any{"type": "result", "subtype": "success", "result": text, "total_cost_usd": spent, "modelUsage": map[string]any{"test": map[string]any{"contextWindow": 100000}}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			Response struct {
				RequestID string `json:"request_id"`
				Response  struct {
					Behavior     string `json:"behavior"`
					UpdatedInput struct {
						Answers map[string]string `json:"answers"`
					} `json:"updatedInput"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			os.Exit(31)
		}
		switch message.Type {
		case "control_request":
			send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": message.RequestID, "response": map[string]any{}}})
			if message.Request.Subtype == "interrupt" {
				finish("interrupted")
			}
		case "user":
			text := ""
			if len(message.Message.Content) > 0 {
				text = message.Message.Content[0].Text
			}
			switch text {
			case "/compact":
				send(map[string]any{"type": "system", "subtype": "status", "status": "compacting"})
				send(map[string]any{"type": "system", "subtype": "compact_boundary", "compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 2000}})
				finish("Context compacted.")
			case "approval":
				send(map[string]any{"type": "control_request", "request_id": "approval", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "approval-tool", "input": map[string]any{"command": "printf hello"}}})
			case "question":
				send(map[string]any{"type": "control_request", "request_id": "question", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": "question-tool", "input": map[string]any{"questions": []any{map[string]any{"question": "Which database?", "header": "Database", "options": []any{map[string]any{"label": "SQLite", "description": "Local"}, map[string]any{"label": "Postgres", "description": "Server"}}}}}}})
			case "echo-secret":
				finish("Credential echo: " + os.Getenv("ANTHROPIC_AUTH_TOKEN"))
			default:
				finish("Answer: " + text)
			}
		case "control_response":
			if message.Response.RequestID == "approval" {
				finish("Approval: " + message.Response.Response.Behavior)
			} else if message.Response.RequestID == "question" {
				finish("Database: " + message.Response.Response.UpdatedInput.Answers["Which database?"])
			}
		}
	}
	os.Exit(0)
}

type claudeTestBrowser struct {
	t       *testing.T
	ctx     context.Context
	stream  *gateway.WebUIStream
	seq     int
	backlog []webUIRPC
}

func (b *claudeTestBrowser) next(match func(webUIRPC) bool) webUIRPC {
	b.t.Helper()
	for index, rpc := range b.backlog {
		if match(rpc) {
			b.backlog = append(b.backlog[:index], b.backlog[index+1:]...)
			return rpc
		}
	}
	for {
		select {
		case frame, ok := <-b.stream.Frames():
			if !ok {
				b.t.Fatal("WebUI frame channel closed")
			}
			b.stream.Consumed(frame)
			if frame.Action == "error" {
				b.t.Fatalf("WebUI relay failed: %s", frame.Error)
			}
			var rpc webUIRPC
			if frame.Action == "output" && json.Unmarshal(frame.Data, &rpc) == nil {
				if match(rpc) {
					return rpc
				}
				b.backlog = append(b.backlog, rpc)
			}
		case <-b.stream.Done():
			b.t.Fatal("WebUI stream closed", b.stream.Err())
		case <-b.ctx.Done():
			b.t.Fatal("timed out waiting for Claude WebUI output")
		}
	}
}

func (b *claudeTestBrowser) request(method string, params any) webUIRPC {
	b.t.Helper()
	b.seq++
	id := fmt.Sprintf("browser-%d", b.seq)
	data, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := b.stream.Send(b.ctx, data); err != nil {
		b.t.Fatal(err)
	}
	return b.next(func(rpc webUIRPC) bool { return string(rpc.ID) == `"`+id+`"` && rpc.Method == "" })
}

func (b *claudeTestBrowser) success(method string, params any) webUIRPC {
	b.t.Helper()
	result := b.request(method, params)
	if len(result.Error) > 0 && string(result.Error) != "null" {
		b.t.Fatalf("Claude WebUI %s failed: %s", method, result.Error)
	}
	return result
}

// The real gateway registry, dispatcher, authenticated worker socket, worker
// Agent, native CLI bridge, WebUI relay, history fallback and outbox run here.
func TestClaudeWorkerIntegration(t *testing.T) {
	t.Setenv("MACT_CLAUDE_WORKER_TEST_HELPER", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	root := t.TempDir()
	// Native cleanup must never inspect the test runner's real Claude data.
	nativeRoot := filepath.Join(root, "native-claude")
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", nativeRoot)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(root, "claude")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\\''") + "'"
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexec "+quoted+" -test.run=TestClaudeWorkerCLIHelper -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	registryStore, probe := controlPlaneRegistry(t)
	workerID := uuid.New()
	const workerToken = "cwk_claude_integration_fixture"
	hash := sha256.Sum256([]byte(workerToken))
	if _, err := registryStore.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "Claude integration", OS: "linux", Arch: "amd64", TokenHash: hash[:]}); err != nil {
		t.Fatal(err)
	}
	tokenPath, statePath := filepath.Join(root, "token"), filepath.Join(root, "state", "worker.db")
	if err := os.WriteFile(tokenPath, []byte(workerToken), 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := OpenStore(statePath, workerID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	cfg := config.WorkerConfig{AgentKind: "claude", WorkerID: workerID.String(), Name: "Claude integration", GatewayURL: "wss://gateway.example/tgw/api/v1/workers/connect", TokenFile: tokenPath, StateFile: statePath,
		AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "claude", Name: "Claude Code", ClaudeBinary: cli, WorkingDirectory: root, Autostart: true, RestartPolicy: "on-failure"}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := NewAgent(cfg, local, logger)
	if err != nil {
		t.Fatal(err)
	}
	gw := startControlGateway(t, registryStore, &telegramRecorder{}, logger)
	defer gw.close()
	slot := &gatewaySlot{}
	slot.set(gw)
	agent.newConnection = dialTestServer(slot)
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("Claude worker stopped", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Claude worker did not stop")
		}
	}()
	waitControl(t, func() bool { return len(agent.manager.Snapshot()) == 1 && len(gw.hub.ConnectedWorkers()) == 1 })
	runtime := agent.manager.Snapshot()[0]
	create := func(name string) registry.AdminSession {
		t.Helper()
		requestID := uuid.New()
		target := registry.WebUICreationTarget{WorkerID: workerID, RuntimeID: uuid.MustParse(runtime.ID), RuntimeGeneration: runtime.Generation, RequestID: requestID}
		if _, err := registryStore.QueueWebUISessionCreation(ctx, target, name, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		var selected registry.AdminSession
		waitControl(t, func() bool {
			result, err := registryStore.WebUICreation(ctx, workerID, requestID, protocol.NewSession)
			if err != nil || result.Pending {
				return false
			}
			if result.ErrorCode != "" {
				record, _, _ := local.LoadCommand(requestID.String())
				detail, _ := json.Marshal(record.Result)
				t.Fatalf("Claude session creation failed: %s %s; %s", result.ErrorCode, result.Message, detail)
			}
			dashboard, err := registryStore.AdminDashboardSnapshot(ctx)
			if err != nil {
				return false
			}
			for _, session := range dashboard.Sessions {
				if session.Name == name {
					selected = session
					return true
				}
			}
			return false
		})
		return selected
	}
	a, b := create("alpha"), create("beta")
	dashboard, err := registryStore.AdminDashboardSnapshot(ctx)
	if err != nil || len(dashboard.Workers) != 1 || dashboard.Workers[0].AgentKind != "claude" || !dashboard.Workers[0].SupportsWorkerRuntimeSettings || !dashboard.Workers[0].SupportsSessionDeletion || dashboard.Workers[0].SupportsSessionArchives || len(dashboard.Sessions) != 2 {
		t.Fatal("Claude worker inventory or capabilities were not published", err)
	}
	open := func(session registry.AdminSession) *claudeTestBrowser {
		t.Helper()
		stream, err := gw.hub.OpenWebUI(ctx, session)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stream.Close)
		browser := &claudeTestBrowser{t: t, ctx: ctx, stream: stream}
		browser.success("thread/resume", map[string]any{"threadId": session.ThreadID})
		return browser
	}
	browserA, browserB := open(a), open(b)
	foreign := browserA.request("turn/start", map[string]any{"threadId": b.ThreadID, "input": []any{map[string]any{"type": "text", "text": "foreign"}}})
	if len(foreign.Error) == 0 {
		t.Fatal("browser started a turn in another Claude session")
	}
	turn := func(prompt string) {
		t.Helper()
		browserA.success("turn/start", map[string]any{"threadId": a.ThreadID, "input": []any{map[string]any{"type": "text", "text": prompt}}})
	}
	complete := func() {
		t.Helper()
		browserA.next(func(rpc webUIRPC) bool { return rpc.Method == "turn/completed" })
		waitControl(t, func() bool {
			sessions, _ := local.ListSessions(runtime.ID)
			for _, session := range sessions {
				if session.ID == a.ID {
					return session.ActiveTurnID == ""
				}
			}
			return false
		})
	}
	turn("hello")
	complete()
	statusReply := browserA.success("thread/status/read", map[string]any{"threadId": a.ThreadID})
	var status struct {
		ClaudeStatus struct {
			Context struct {
				UsedTokens int64 `json:"usedTokens"`
				MaxTokens  int64 `json:"maxTokens"`
			} `json:"context"`
			Cost struct {
				SessionUSD *float64 `json:"sessionUSD"`
			} `json:"cost"`
		} `json:"claudeStatus"`
	}
	if json.Unmarshal(statusReply.Result, &status) != nil || status.ClaudeStatus.Cost.SessionUSD == nil || *status.ClaudeStatus.Cost.SessionUSD != .125 || status.ClaudeStatus.Context.UsedTokens != 2000 || status.ClaudeStatus.Context.MaxTokens != 100000 {
		t.Fatalf("Claude status lost reported usage across gateway: %s", statusReply.Result)
	}
	if foreign := browserA.request("thread/status/read", map[string]any{"threadId": b.ThreadID}); len(foreign.Error) == 0 {
		t.Fatal("another session status was readable")
	}
	browserA.next(func(rpc webUIRPC) bool {
		return rpc.Method == "thread/claudeStatus/updated" && strings.Contains(string(rpc.Params["claudeStatus"]), `"sessionUSD":0.125`)
	})
	history := browserA.success("thread/items/list", map[string]any{"threadId": a.ThreadID, "limit": 20, "sortDirection": "desc"})
	if !strings.Contains(string(history.Result), "Answer: hello") {
		t.Fatal("Claude WebUI history fallback did not include the completed native response")
	}
	if foreign := browserA.request("thread/compact/start", map[string]any{"threadId": b.ThreadID}); len(foreign.Error) == 0 {
		t.Fatal("browser compacted another Claude session")
	}
	compact := browserA.success("gateway/command", map[string]any{"name": "compact", "args": ""})
	if !strings.Contains(string(compact.Result), "Claude Code will report") {
		t.Fatalf("Claude compact command did not start: %s", compact.Result)
	}
	complete()
	history = browserA.success("thread/items/list", map[string]any{"threadId": a.ThreadID, "limit": 20, "sortDirection": "desc"})
	if !strings.Contains(string(history.Result), "contextCompaction") || !strings.Contains(string(history.Result), "Context compacted.") {
		t.Fatal("native compaction result was not available through WebUI history")
	}
	waitControl(t, func() bool {
		lease, err := agent.prepareUpdate(ctx, time.Second)
		if err != nil {
			if strings.Contains(err.Error(), "a native CLI thread is active") {
				t.Fatalf("completed Claude compaction blocked worker updates: %v", err)
			}
			return false
		}
		if err := agent.abortUpdate(lease.Token); err != nil {
			t.Fatal(err)
		}
		return true
	})
	turn("approval")
	approval := browserA.next(func(rpc webUIRPC) bool { return rpc.Method == "item/commandExecution/requestApproval" })
	// A second browser connects during the approval, so its attachment proxy
	// observes an active-thread snapshot as well as subsequent turn events.
	resumedDuringApproval := open(a)
	var activeTurn string
	_ = json.Unmarshal(approval.Params["turnId"], &activeTurn)
	queued := browserA.success("turn/steer", map[string]any{"threadId": a.ThreadID, "expectedTurnId": activeTurn, "input": []any{map[string]any{"type": "text", "text": "queued follow-up"}}})
	if !strings.Contains(string(queued.Result), `"queued":true`) {
		t.Fatal("Claude active-turn follow-up was not explicitly queued")
	}
	spoof, _ := json.Marshal(map[string]any{"id": json.RawMessage(approval.ID), "result": map[string]string{"decision": "accept"}})
	if err := browserB.stream.Send(ctx, spoof); err != nil {
		t.Fatal(err)
	}
	rejected := browserB.next(func(rpc webUIRPC) bool { return string(rpc.ID) == string(approval.ID) && len(rpc.Error) > 0 })
	if len(rejected.Error) == 0 {
		t.Fatal("another Claude session answered a permission request")
	}
	if err := browserA.stream.Send(ctx, spoof); err != nil {
		t.Fatal(err)
	}
	complete()
	complete() // The adapter-owned successor turn runs after approval completion.
	waitControl(t, func() bool {
		lease, err := agent.prepareUpdate(ctx, time.Second)
		if err != nil {
			if strings.Contains(err.Error(), "a native CLI thread is active") {
				t.Fatalf("completed Claude turn left an active browser snapshot blocking updates: %v", err)
			}
			return false // Durable event acknowledgements can still be in flight.
		}
		if err := agent.abortUpdate(lease.Token); err != nil {
			t.Fatal(err)
		}
		return true
	})
	resumedDuringApproval.stream.Close()
	turn("question")
	question := browserA.next(func(rpc webUIRPC) bool { return rpc.Method == "item/tool/requestUserInput" })
	var questions []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(question.Params["questions"], &questions) != nil || len(questions) != 1 {
		t.Fatal("Claude question was not normalized")
	}
	var restored struct {
		Questions []webUIQuestion `json:"questions"`
	}
	waitControl(t, func() bool {
		snapshot := browserA.success("gateway/questions", map[string]any{})
		return json.Unmarshal(snapshot.Result, &restored) == nil && len(restored.Questions) == 1
	})
	answer := map[string]any{"approvalId": restored.Questions[0].ApprovalID, "result": map[string]any{"answers": map[string]any{questions[0].ID: map[string]any{"answers": []string{"SQLite"}}}}}
	if rejected := browserB.request("gateway/answer", answer); len(rejected.Error) == 0 {
		t.Fatal("another Claude session answered a restored worker question")
	}
	browserA.success("gateway/answer", answer)
	complete()
	pending := browserA.success("gateway/questions", map[string]any{})
	if !strings.Contains(string(pending.Result), `"questions":[]`) {
		t.Fatal("completed Claude questions remained actionable")
	}
	waitControl(t, func() bool { return local.checkUpdateIdle() == nil })
	credential := "claude-integration-private-token"
	settings, err := gw.hub.WorkerRuntimeSettings(ctx, workerID, &protocol.WorkerRuntimeSettingsUpdate{Model: "custom[1m]", AuthMode: "auth_token", PermissionMode: "default", APIKey: &credential})
	if err != nil || !settings.APIKeyConfigured {
		t.Fatal("live worker runtime settings could not be saved", err)
	}
	turn("echo-secret")
	complete()
	history = browserA.success("thread/items/list", map[string]any{"threadId": a.ThreadID, "limit": 20, "sortDirection": "desc"})
	combined := ""
	for pages := 0; ; pages++ {
		if strings.Contains(string(history.Result), credential) {
			t.Fatal("Claude browser history leaked a credential")
		}
		combined += string(history.Result)
		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if json.Unmarshal(history.Result, &page) != nil || len(page.Data) > 20 {
			t.Fatal("Claude history was not a bounded item page")
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if pages >= 5 {
			t.Fatal("Claude history pagination did not terminate")
		}
		history = browserA.success("thread/items/list", map[string]any{"threadId": a.ThreadID, "limit": 20, "sortDirection": "desc", "cursor": *page.NextCursor})
	}
	for _, text := range []string{"REDACTED", "Database: SQLite", "Approval: allow", "Answer: queued follow-up", "Answer: hello"} {
		if !strings.Contains(combined, text) {
			t.Fatal("Claude browser history lost expected completed content", text)
		}
	}
	var durableLeaks int
	if err := probe.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM events WHERE instr(payload,?)>0) + (SELECT count(*) FROM commands WHERE instr(payload,?)>0)`, credential, credential).Scan(&durableLeaks); err != nil || durableLeaks != 0 {
		t.Fatal("Claude runtime credential entered gateway commands/events", err)
	}
	// Exercise the actual durable registry -> dispatcher -> worker -> native
	// deletion route. The fake native transcript/sidecar share a directory with
	// a sibling, while both project CWDs and their files must survive.
	project := filepath.Join(nativeRoot, "projects", "fixture-project")
	selectedSidecar := filepath.Join(project, a.ThreadID, "subagents")
	if err := os.MkdirAll(selectedSidecar, 0o700); err != nil {
		t.Fatal(err)
	}
	selectedTranscript := filepath.Join(project, a.ThreadID+".jsonl")
	siblingTranscript := filepath.Join(project, b.ThreadID+".jsonl")
	keepFile := filepath.Join(a.CWD, "keep-project.txt")
	for _, path := range []string{selectedTranscript, filepath.Join(selectedSidecar, "agent-child.jsonl"), siblingTranscript, keepFile} {
		if err := os.WriteFile(path, []byte("fixture conversation or project content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitControl(t, func() bool { return local.checkUpdateIdle() == nil })
	requestID := uuid.New()
	if _, err := registryStore.QueueWebUISessionDeletion(ctx, uuid.MustParse(a.ID), requestID); err != nil {
		t.Fatal(err)
	}
	waitControl(t, func() bool {
		result, err := registryStore.WebUISessionDeletion(ctx, uuid.MustParse(a.ID), requestID)
		if err != nil || result.Pending {
			return false
		}
		if !result.Deleted || result.Status != "completed" {
			t.Fatalf("Claude gateway deletion failed: %#v", result)
		}
		return true
	})
	for _, path := range []string{selectedTranscript, filepath.Join(project, a.ThreadID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("selected native artifact remains: %s (%v)", path, err)
		}
	}
	for _, path := range []string{siblingTranscript, keepFile, a.CWD, b.CWD} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("deletion affected sibling/project: %s (%v)", path, err)
		}
	}
	stateFiles, err := filepath.Glob(filepath.Join(filepath.Dir(statePath), "claude", "*", a.ThreadID+".json"))
	if err != nil || len(stateFiles) != 0 {
		t.Fatal("selected adapter conversation still exists", stateFiles, err)
	}
	duplicate, err := registryStore.QueueWebUISessionDeletion(ctx, uuid.MustParse(a.ID), requestID)
	if err != nil || duplicate.Pending || !duplicate.Deleted {
		t.Fatal("gateway deletion receipt was not idempotent", err)
	}
	if _, err := registryStore.QueueWebUISessionDeletion(ctx, uuid.MustParse(a.ID), uuid.New()); err == nil {
		t.Fatal("deleted session accepted a new deletion command")
	}
	waitControl(t, func() bool { return local.checkUpdateIdle() == nil })
	sessions, err := local.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 2 {
		t.Fatal("worker session identities changed", err)
	}
	for _, session := range sessions {
		if session.ID == a.ID && (!session.Deleted || session.Name != "" || session.CWD != "" || session.Preview != "") {
			t.Fatal("deleted worker conversation metadata remains")
		}
		if session.ID == b.ID && (session.Deleted || session.Name != b.Name || session.CWD != b.CWD) {
			t.Fatal("worker sibling was changed")
		}
	}
	if err := local.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketCommands).ForEach(func(_, data []byte) error {
			record, err := decodeCommand(data)
			if err != nil {
				return err
			}
			if record.Command.SessionID == a.ID && (strings.Contains(string(data), "Answer: hello") || strings.Contains(string(data), a.CWD) || strings.Contains(string(data), "Security")) {
				t.Fatal("deleted session command payload survived purge")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	// The other browser remains attached to its own live native session.
	browserB.success("turn/start", map[string]any{"threadId": b.ThreadID, "input": []any{map[string]any{"type": "text", "text": "sibling after deletion"}}})
	browserB.next(func(rpc webUIRPC) bool { return rpc.Method == "turn/completed" })
}
