package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/claudeadapter"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

// A minimal native streaming process lets worker integration tests use the real
// Claude bridge without an account, a model API call or an installed service.
func init() {
	if os.Getenv("MACT_WORKER_CLAUDE_HELPER") == "1" {
		TestClaudeWorkerProcessHelper(nil)
	}
}

func TestClaudeWorkerProcessHelper(t *testing.T) {
	if os.Getenv("MACT_WORKER_CLAUDE_HELPER") != "1" {
		return
	}
	for _, arg := range os.Args[1:] {
		if arg == "--help" {
			_, _ = os.Stdout.WriteString(`  --permission-mode <mode> (choices: "default", "acceptEdits", "plan", "dontAsk", "auto", "bypassPermissions")
  --effort <level> (choices: "low", "medium", "high", "xhigh", "max")
`)
			os.Exit(0)
		}
		if arg == "--version" {
			_, _ = os.Stdout.WriteString("9.8.7 (Claude Code)\n")
			os.Exit(0)
		}
	}
	if path := os.Getenv("MACT_WORKER_CLAUDE_START_LOG"); path != "" {
		_ = os.WriteFile(path, []byte("started"), 0o600)
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var msg struct {
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
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil {
			os.Exit(2)
		}
		if msg.Type == "control_request" {
			response := map[string]any{}
			if msg.Request.Subtype == "initialize" {
				response["models"] = []any{
					map[string]any{"value": "default", "resolvedModel": "claude-default-test", "supportedEffortLevels": []string{"low", "medium", "high"}},
					map[string]any{"value": "claude-opus-test", "resolvedModel": "claude-opus-resolved-test", "supportedEffortLevels": []string{"low", "medium", "high", "xhigh", "max"}},
					map[string]any{"value": "worker-initial", "supportedEffortLevels": []string{"low", "medium", "high"}},
					map[string]any{"value": "worker-next", "supportedEffortLevels": []string{"low", "medium", "high"}},
					map[string]any{"value": "no-effort-model"},
				}
			}
			send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": msg.RequestID, "response": response}})
			if msg.Request.Subtype == "interrupt" {
				send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true})
			}
		}
		if msg.Type == "user" {
			if path := os.Getenv("MACT_WORKER_CLAUDE_ENV_LOG"); path != "" {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					os.Exit(3)
				}
				_ = json.NewEncoder(f).Encode(map[string]any{"api_key": os.Getenv("ANTHROPIC_API_KEY") != "", "auth_token": os.Getenv("ANTHROPIC_AUTH_TOKEN") != "", "base_url": os.Getenv("ANTHROPIC_BASE_URL"), "model": os.Getenv("ANTHROPIC_MODEL"), "disable_autoupdater": os.Getenv("DISABLE_AUTOUPDATER"), "opus_model": os.Getenv("ANTHROPIC_DEFAULT_OPUS_MODEL"), "sonnet_model": os.Getenv("ANTHROPIC_DEFAULT_SONNET_MODEL"), "haiku_model": os.Getenv("ANTHROPIC_DEFAULT_HAIKU_MODEL")})
				_ = f.Close()
			}
			text := ""
			for _, block := range msg.Message.Content {
				text += block.Text
			}
			if text == "block" {
				continue
			}
			text = "Reply: " + text
			send(map[string]any{"type": "assistant", "message": map[string]any{"id": uuid.NewString(), "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": text}}}})
			send(map[string]any{"type": "result", "subtype": "success", "result": text})
		}
	}
	os.Exit(0)
}

func claudeSettingsAgent(t *testing.T) (*Agent, *claudeadapter.Runtime) {
	t.Helper()
	root, err := os.MkdirTemp("", "mact-cc-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	id := uuid.NewString()
	cfg := config.WorkerConfig{
		AgentKind: "claude", WorkerID: id, StateFile: filepath.Join(root, "worker.db"), AllowedWorkspaceRoots: []string{root},
		Runtimes: []config.RuntimeProfile{{ID: "primary", Name: "Claude", ClaudeBinary: os.Args[0], ClaudeSettingsFile: filepath.Join(root, "runtime-settings.json"), WorkingDirectory: root}},
	}
	store, err := OpenStore(cfg.StateFile, id)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAgent(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.ctx = ctx
	bridge, err := claudeadapter.Start(ctx, claudeadapter.Config{
		Command:          os.Args[0],
		WorkingDirectory: root, StateDirectory: filepath.Join(root, "bridge"), SocketPath: filepath.Join(root, "s.sock"),
		Env: append(os.Environ(), "MACT_WORKER_CLAUDE_HELPER=1"), Stderr: io.Discard, RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		cancel()
		_ = store.Close()
		t.Fatal(err)
	}
	a.manager.claude["primary"] = bridge
	t.Cleanup(func() { cancel(); a.group.Wait(); _ = bridge.Close(); _ = store.Close() })
	return a, bridge
}

func claudeSettingsRequest(a *Agent, action string, update *protocol.WorkerRuntimeSettingsUpdate) protocol.WorkerRuntimeSettingsRequest {
	return protocol.WorkerRuntimeSettingsRequest{RequestID: uuid.NewString(), WorkerID: a.cfg.WorkerID, Action: action, Settings: update}
}

func TestClaudeRuntimeSettingsKeepClearAndLoginCredentialTransitions(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	path := claudeSettingsPath(a.cfg)
	initial := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "get", nil))
	if initial.ErrorCode != "" || initial.Settings == nil || initial.Settings.AuthMode != "login" || initial.Settings.APIKeyConfigured {
		t.Fatalf("unexpected inherited login: %#v", initial)
	}
	secret := "private-account-token-not-a-standard-key-pattern"
	update := &protocol.WorkerRuntimeSettingsUpdate{Model: "claude-test", BaseURL: "https://api.example.test/anthropic", AuthMode: "auth_token", PermissionMode: "default", APIKey: &secret}
	put := func() protocol.WorkerRuntimeSettingsResult {
		return a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	}
	response := put()
	if response.ErrorCode != "" || response.Settings == nil || !response.Settings.APIKeyConfigured {
		t.Fatalf("credential not saved: %#v", response)
	}
	for _, redactor := range []*auth.Redactor{a.redactor, a.manager.statsRedactor, a.store.redactor.Load()} {
		if got := redactor.Redact(secret); got != "[REDACTED]" {
			t.Fatalf("new account credential bypasses a shared redactor: %q", got)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("settings permissions: %v %v", info, err)
	}
	for _, operation := range []string{"put", "get"} {
		if operation == "get" {
			response = a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "get", nil))
		}
		data, _ := json.Marshal(response)
		if strings.Contains(string(data), secret) || strings.Contains(string(data), "credential") {
			t.Fatalf("%s exposed credential: %s", operation, data)
		}
	}
	update.APIKey = nil
	update.Model = "different-model"
	if response = put(); response.ErrorCode != "" {
		t.Fatal(response)
	}
	settings, err := readClaudeSettings(path)
	if err != nil || settings.Credential != secret || settings.Model != update.Model {
		t.Fatalf("omitted credential not retained: %#v %v", settings, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	update.APIKey = &empty
	if response = put(); response.ErrorCode != "invalid_settings" {
		t.Fatalf("empty API credential accepted: %#v", response)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("invalid settings partially replaced saved account")
	}
	update.AuthMode = "login"
	update.APIKey = nil
	if response = put(); response.ErrorCode != "" || !response.Settings.APIKeyConfigured {
		t.Fatalf("login did not preserve the omitted inactive credential: %#v", response)
	}
	update.APIKey = &empty
	if response = put(); response.ErrorCode != "" || response.Settings.APIKeyConfigured {
		t.Fatalf("explicit remove did not clear account key: %#v", response)
	}
	settings, err = readClaudeSettings(path)
	if err != nil || settings.Credential != "" {
		t.Fatalf("login retained account credential: %#v %v", settings, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".runtime-settings-") {
			t.Fatal("atomic settings write left a temporary credential file")
		}
	}
	if err := a.store.db.View(func(tx *workerdb.Tx) error {
		for _, name := range [][]byte{bucketCommands, bucketOutbox, bucketSessions, bucketRuntimes, bucketMeta} {
			if err := tx.Bucket(name).ForEach(func(_, value []byte) error {
				if strings.Contains(string(value), secret) {
					t.Errorf("account credential entered durable bucket %q", name)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeRuntimeSettingsIdleFenceAndCodexRejection(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	secret := "private-token"
	update := &protocol.WorkerRuntimeSettingsUpdate{AuthMode: "api_key", PermissionMode: "default", APIKey: &secret}
	request := claudeSettingsRequest(a, "put", update)
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: uuid.NewString(), ThreadID: uuid.NewString(), CWD: a.cfg.AllowedWorkspaceRoots[0], State: "active", ActiveTurnID: "active-turn"})
	if err != nil {
		t.Fatal(err)
	}
	if response := a.runtimeSettings(t.Context(), request); response.ErrorCode != "busy" {
		t.Fatalf("changed account during worker turn: %#v", response)
	}
	if _, err := os.Stat(claudeSettingsPath(a.cfg)); !os.IsNotExist(err) {
		t.Fatalf("busy account update wrote settings: %v", err)
	}
	session.State, session.ActiveTurnID = "idle", ""
	if _, err := a.store.UpsertSession(session); err != nil {
		t.Fatal(err)
	}
	thread, err := bridge.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: a.cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.StartTurn(t.Context(), thread.ID, "block"); err != nil {
		t.Fatal(err)
	}
	if response := a.runtimeSettings(t.Context(), request); response.ErrorCode != "busy" {
		t.Fatalf("changed account during native browser turn: %#v", response)
	}
	if _, err := os.Stat(claudeSettingsPath(a.cfg)); !os.IsNotExist(err) {
		t.Fatalf("native busy update wrote settings: %v", err)
	}
	a.cfg.AgentKind = "codex"
	if response := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); response.ErrorCode != "unsupported" {
		t.Fatalf("Codex accepted Claude runtime settings: %#v", response)
	}
	if response := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "get", nil)); response.ErrorCode != "" || response.Settings == nil || response.Settings.Provider != "codex" {
		t.Fatalf("Codex defaults unavailable: %#v", response)
	}
}

func TestClaudeWorkerCreatesDiscoversAndReadsNativeConversation(t *testing.T) {
	a, bridge := claudeSettingsAgent(t)
	runtime, err := a.store.BeginRuntime("primary", "Claude", a.cfg.AllowedWorkspaceRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	runtime.State = "running"
	a.manager.install(runtime, bridge.Client)
	stop := a.manager.forwardAdapter(a.ctx, runtime, bridge.Client)
	defer stop()
	if err := a.manager.discover(t.Context(), runtime, bridge.Client); err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	command.Arguments = protocol.Arguments{CWD: runtime.DefaultCWD, SessionName: "Native Claude session"}
	if ack, err := a.HandleCommand(t.Context(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("create admission: %#v %v", ack, err)
	}
	var created protocol.Session
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		if err != nil || !found || record.State != CommandCompleted || record.Result == nil || record.Result.Session == nil {
			return false
		}
		created = *record.Result.Session
		return true
	})
	if created.ThreadID == "" || created.CWD != runtime.DefaultCWD || created.Name != "Native Claude session" {
		t.Fatalf("incorrect native session: %#v", created)
	}
	turn := agentCommand(runtime, created, protocol.StartTurn)
	turn.Arguments.Text = "hello Claude bridge"
	if ack, err := a.HandleCommand(t.Context(), turn); err != nil || ack.Status != "accepted" {
		t.Fatalf("turn admission: %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		events, err := a.store.OutboxAfter(0)
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "turn_completed" && strings.Contains(string(event.Data), "Reply: hello Claude bridge") {
				return true
			}
		}
		return false
	})
	if err := a.manager.discover(t.Context(), runtime, bridge.Client); err != nil {
		t.Fatal(err)
	}
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].ID != created.ID || sessions[0].ThreadID != created.ThreadID {
		t.Fatalf("native discovery lost session identity: %#v %v", sessions, err)
	}
	history := agentCommand(runtime, created, protocol.ReadHistory)
	history.Arguments.History = &protocol.HistoryRequest{Limit: 10, Messages: true, NewestFirst: false}
	if ack, err := a.HandleCommand(t.Context(), history); err != nil || ack.Status != "accepted" {
		t.Fatalf("history admission: %#v %v", ack, err)
	}
	var record CommandRecord
	waitFor(t, func() bool {
		var found bool
		record, found, err = a.store.LoadCommand(history.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	if record.Result == nil || record.Result.History == nil {
		t.Fatalf("missing native conversation: %#v", record)
	}
	messages := record.Result.History.Messages
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Text != turn.Arguments.Text || messages[1].Role != "assistant" || messages[1].Text != "Reply: hello Claude bridge" {
		t.Fatalf("incorrect native conversation: %#v", messages)
	}
}

func TestClaudeLoginClearsAmbientSecretsAndAccountModesUseDistinctVariables(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	root := a.cfg.AllowedWorkspaceRoots[0]
	logPath := filepath.Join(root, "env.jsonl")
	t.Setenv("MACT_WORKER_CLAUDE_ENV_LOG", logPath)
	t.Setenv("ANTHROPIC_API_KEY", "ambient-api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-auth-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ambient.example.test")
	t.Setenv("ANTHROPIC_MODEL", "ambient-model")
	t.Setenv("MACT_WORKER_CLAUDE_HELPER", "1")
	profile := a.cfg.Runtimes[0]
	client, err := a.manager.startClaudeProfile(a.ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := os.Stat(claudeSettingsPath(a.cfg)); !os.IsNotExist(err) {
		t.Fatalf("initial settings unexpectedly required: %v", err)
	}
	thread, err := client.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	secret := "private-configured-account"
	for index, mode := range []string{"login", "auth_token", "api_key", "login"} {
		if index > 0 {
			update := &protocol.WorkerRuntimeSettingsUpdate{AuthMode: mode, PermissionMode: "default"}
			if mode != "login" {
				update.APIKey = &secret
				update.Model = "configured-model"
				update.BaseURL = "https://configured.example.test"
			}
			response := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
			if response.ErrorCode != "" {
				t.Fatalf("%s account configuration: %#v", mode, response)
			}
		}
		if _, err := client.StartTurn(t.Context(), thread.ID, "probe "+mode); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(3 * time.Second)
	completed:
		for {
			select {
			case event := <-client.Events():
				if event.Kind == "turn_completed" {
					break completed
				}
			case <-deadline:
				t.Fatalf("native %s process did not finish", mode)
			}
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("unexpected process launches: %s", data)
	}
	for index, mode := range []string{"login", "auth_token", "api_key", "login"} {
		var got struct {
			APIKey    bool   `json:"api_key"`
			AuthToken bool   `json:"auth_token"`
			BaseURL   string `json:"base_url"`
			Model     string `json:"model"`
		}
		if err := json.Unmarshal([]byte(lines[index]), &got); err != nil {
			t.Fatal(err)
		}
		if got.APIKey != (mode == "api_key") || got.AuthToken != (mode == "auth_token") {
			t.Fatalf("%s inherited or selected the wrong account variable: %#v", mode, got)
		}
		if mode == "login" && (got.BaseURL != "" || got.Model != "") {
			t.Fatalf("login retained ambient model or endpoint: %#v", got)
		}
		if mode != "login" && (got.BaseURL != "https://configured.example.test" || got.Model != "configured-model") {
			t.Fatalf("configured endpoint/model was not used: %#v", got)
		}
	}
}

func TestClaudeSavedCredentialProtectsStartupRedactors(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	secret := "saved-credential-with-regexp-$[characters]"
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), claudeSettingsFile{AuthMode: "auth_token", PermissionMode: "default", Credential: secret}); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewAgent(a.cfg, a.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, redactor := range []*auth.Redactor{fresh.redactor, fresh.manager.statsRedactor, fresh.store.redactor.Load()} {
		if got := redactor.Redact(secret); got != "[REDACTED]" {
			t.Fatalf("saved account credential was exposed on startup: %q", got)
		}
	}
	cfg := fresh.cfg
	cfg.GatewayURL = "wss://gateway.example.test/tgw/api/v1/workers/connect"
	cfg.TokenFile = filepath.Join(a.cfg.AllowedWorkspaceRoots[0], "worker.token")
	if _, err := NewConnection(cfg, a.store, nil, nil, func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		return protocol.CommandAck{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := a.store.redactor.Load().Redact(secret); got != "[REDACTED]" {
		t.Fatalf("connection startup removed saved account protection: %q", got)
	}
}

func TestClaudeCredentialRotationProtectsOldAndNewQueuedText(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	old, next := "synthetic-old.$[credential]", "synthetic-next.$[credential]"
	update := &protocol.WorkerRuntimeSettingsUpdate{AuthMode: "auth_token", PermissionMode: "default", APIKey: &old}
	if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "" {
		t.Fatal(result.ErrorCode)
	}
	data, _ := json.Marshal(protocol.Result{Text: old + " " + next})
	before, err := a.store.AppendEvent(protocol.Event{Kind: "command_completed", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	// Account switches require an acknowledged outbox. Delayed native text
	// can still mention the prior credential after the switch completes.
	if err := a.store.AckThrough(before.Seq); err != nil {
		t.Fatal(err)
	}
	update.APIKey = &next
	if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "" {
		t.Fatal(result.ErrorCode)
	}
	for _, redactor := range []*auth.Redactor{a.redactor, a.manager.statsRedactor, a.store.redactor.Load()} {
		for _, credential := range []string{old, next} {
			if redactor.Redact(credential) != "[REDACTED]" {
				t.Fatal("credential rotation lost literal protection")
			}
		}
	}
	if _, err := a.store.AppendEvent(protocol.Event{Kind: "command_completed", Data: data}); err != nil {
		t.Fatal(err)
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Seq != before.Seq+1 {
		t.Fatal("rotation changed queued sequence", err)
	}
	if strings.Contains(string(events[0].Data), old) || strings.Contains(string(events[0].Data), next) {
		t.Fatal("rotation left a queued credential")
	}
}

func TestClaudeRejectsInsecureAssignedAndPersistedEndpointsBeforeNativeLaunch(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	root := a.cfg.AllowedWorkspaceRoots[0]
	marker := filepath.Join(root, "started")
	t.Setenv("MACT_WORKER_CLAUDE_HELPER", "1")
	t.Setenv("MACT_WORKER_CLAUDE_START_LOG", marker)
	credential := "synthetic-persisted-private-credential"
	for _, endpoint := range []string{"http://remote.example.test", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		update := &protocol.WorkerRuntimeSettingsUpdate{BaseURL: endpoint, AuthMode: "api_key", PermissionMode: "default", APIKey: &credential}
		if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "invalid_settings" {
			t.Fatal("insecure endpoint assignment accepted")
		}
		if _, err := os.Stat(claudeSettingsPath(a.cfg)); !os.IsNotExist(err) {
			t.Fatal("rejected assignment wrote settings")
		}
	}
	client, err := a.manager.startClaudeProfile(a.ctx, a.cfg.Runtimes[0])
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	thread, err := client.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	// StartThread can inspect native initialization metadata without inference.
	// The launch fence below concerns the subsequently rejected persisted file.
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	// Model an older installation's existing insecure file. Persisted reads
	// must fail even though no new settings assignment occurs.
	data, _ := json.Marshal(claudeSettingsFile{BaseURL: "http://remote.example.test", AuthMode: "api_key", PermissionMode: "default", Credential: credential})
	if err := os.WriteFile(claudeSettingsPath(a.cfg), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readClaudeSettings(claudeSettingsPath(a.cfg)); err == nil || !strings.Contains(err.Error(), "HTTPS") || strings.Contains(err.Error(), credential) {
		t.Fatal("persisted endpoint validation failed or exposed credential")
	}
	if _, err := client.StartTurn(t.Context(), thread.ID, "probe"); err == nil || strings.Contains(err.Error(), credential) || strings.Contains(err.Error(), "remote.example") {
		t.Fatal("persisted insecure configuration used or exposed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("native process launched with rejected persisted settings")
	}
	if _, err := NewAgent(a.cfg, a.store, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatal("worker startup accepted insecure persisted settings or exposed credential")
	}
}

func TestClaudeRuntimeReasoningAndEnterpriseBudgetPersistPrivateAndPreserveOmitted(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	key := "synthetic-admin-key-in-private-worker-file"
	update := &protocol.WorkerRuntimeSettingsUpdate{Model: "claude-opus-test", ReasoningEffort: "xhigh", AuthMode: "login", PermissionMode: "default", EnterpriseBudget: &protocol.ClaudeEnterpriseBudgetUpdate{Enabled: true, UserID: "user_Ab123", APIKey: &key}}
	result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	if result.ErrorCode != "" || result.Settings == nil || result.Settings.ReasoningEffort != "xhigh" || result.Settings.EnterpriseBudget == nil || !result.Settings.EnterpriseBudget.APIKeyConfigured {
		t.Fatalf("public projection: %#v", result)
	}
	settings, err := readClaudeSettings(claudeSettingsPath(a.cfg))
	if err != nil || settings.ReasoningEffort != "xhigh" || settings.EnterpriseBudgetAPIKey != key {
		t.Fatalf("private settings not persisted: %v", err)
	}
	update.EnterpriseBudget = nil
	update.ReasoningEffort = "low"
	result = a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	if result.ErrorCode != "" || result.Settings == nil || result.Settings.EnterpriseBudget == nil || !result.Settings.EnterpriseBudget.Enabled {
		t.Fatalf("omitted Enterprise enrollment lost: %#v", result)
	}
	// Disabling polling preserves the secret unless removal is explicit.
	update.EnterpriseBudget = &protocol.ClaudeEnterpriseBudgetUpdate{Enabled: false, UserID: "user_Ab123"}
	result = a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	if result.ErrorCode != "" || result.Settings.EnterpriseBudget.Enabled || !result.Settings.EnterpriseBudget.APIKeyConfigured {
		t.Fatalf("disable lost enrollment/key: %#v", result)
	}
	for _, redactor := range []*auth.Redactor{a.redactor, a.manager.statsRedactor, a.store.redactor.Load()} {
		if redactor.Redact(key) != "[REDACTED]" {
			t.Fatal("Enterprise Admin credential was not redacted")
		}
	}
	fresh, err := NewAgent(a.cfg, a.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, redactor := range []*auth.Redactor{fresh.redactor, fresh.manager.statsRedactor, fresh.store.redactor.Load()} {
		if redactor.Redact(key) != "[REDACTED]" {
			t.Fatal("saved Admin credential unprotected on startup")
		}
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), key) || strings.Contains(string(data), `"api_key":`) {
		t.Fatal("public settings exposed Enterprise credential")
	}
	if err := a.store.db.View(func(tx *workerdb.Tx) error {
		for _, name := range [][]byte{bucketCommands, bucketOutbox, bucketSessions, bucketRuntimes, bucketMeta} {
			if err := tx.Bucket(name).ForEach(func(_, value []byte) error {
				if strings.Contains(string(value), key) {
					t.Errorf("Enterprise key persisted in durable bucket %q", name)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudePrivateSettingsRoundTripMaximumCredentialSizes(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	inferenceKey, adminKey := strings.Repeat("i", 8192), strings.Repeat("a", 8192)
	update := &protocol.WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: "default", APIKey: &inferenceKey, EnterpriseBudget: &protocol.ClaudeEnterpriseBudgetUpdate{Enabled: true, UserID: "user_Ab123", APIKey: &adminKey}}
	result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	if result.ErrorCode != "" {
		t.Fatalf("valid maximum credentials not saved: %s", result.ErrorCode)
	}
	saved, err := readClaudeSettings(claudeSettingsPath(a.cfg))
	if err != nil || saved.Credential != inferenceKey || saved.EnterpriseBudgetAPIKey != adminKey {
		t.Fatal("valid PUT created settings that cannot be read", err)
	}
	info, err := os.Stat(claudeSettingsPath(a.cfg))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private settings lost file permissions: %v %v", info, err)
	}
}

func TestClaudeOfficialAccountKeepsNativeFamilyAliasesAndCustomProviderOverrides(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	root := a.cfg.AllowedWorkspaceRoots[0]
	logPath := filepath.Join(root, "alias-env.jsonl")
	t.Setenv("MACT_WORKER_CLAUDE_HELPER", "1")
	t.Setenv("MACT_WORKER_CLAUDE_ENV_LOG", logPath)
	for _, name := range []string{"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		t.Setenv(name, "ambient-alias-model")
	}
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), claudeSettingsFile{Model: "opus", AuthMode: "login", PermissionMode: "default"}); err != nil {
		t.Fatal(err)
	}
	client, err := a.manager.startClaudeProfile(a.ctx, a.cfg.Runtimes[0])
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	thread, err := client.StartThread(t.Context(), codexadapter.ThreadOptions{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	for index, baseURL := range []string{"", "https://provider.example.test"} {
		if index > 0 {
			update := &protocol.WorkerRuntimeSettingsUpdate{Model: "provider-model", BaseURL: baseURL, AuthMode: "login", PermissionMode: "default"}
			if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "" {
				t.Fatal(result.ErrorCode)
			}
		}
		if _, err := client.StartTurn(t.Context(), thread.ID, "probe aliases"); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(3 * time.Second)
	completed:
		for {
			select {
			case event := <-client.Events():
				if event.Kind == "turn_completed" {
					break completed
				}
			case <-deadline:
				t.Fatal("native helper did not finish")
			}
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected native helper launches: %s", data)
	}
	for i, line := range lines {
		var env map[string]any
		if json.Unmarshal([]byte(line), &env) != nil {
			t.Fatal(line)
		}
		for _, key := range []string{"opus_model", "sonnet_model", "haiku_model"} {
			want := ""
			if i == 1 {
				want = "provider-model"
			}
			if env[key] != want {
				t.Fatalf("native family alias %s got %v, want %q", key, env[key], want)
			}
		}
	}
}

func TestClaudeNewAndClearUseWorkerDefaultsAndKeepSessionOverrides(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	t.Setenv("MACT_WORKER_CLAUDE_HELPER", "1")
	initial := claudeSettingsFile{Model: "worker-initial", ReasoningEffort: "high", AuthMode: "login", PermissionMode: "acceptEdits"}
	if err := saveClaudeSettings(claudeSettingsPath(a.cfg), initial); err != nil {
		t.Fatal(err)
	}
	client, err := a.manager.startClaudeProfile(a.ctx, a.cfg.Runtimes[0])
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	runtime, err := a.store.BeginRuntime("primary", "Claude", a.cfg.AllowedWorkspaceRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	runtime.State = "running"
	a.manager.install(runtime, client)
	options, err := a.newThreadOptions(runtime.DefaultCWD, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if options.Model != "" || options.ReasoningEffort != "" || options.PermissionProfile != "" || options.Sandbox != "" {
		t.Fatalf("Claude default settings were mapped to Codex options: %#v", options)
	}
	parent, err := client.StartThread(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	assertSettings := func(threadID, model, effort, mode string) {
		t.Helper()
		status, err := client.ReadClaudeStatus(t.Context(), threadID)
		if err != nil || status.Model == nil || *status.Model != model || (status.ReasoningEffort == nil && effort != "") || (status.ReasoningEffort != nil && *status.ReasoningEffort != effort) || status.PermissionMode == nil || *status.PermissionMode != mode {
			t.Fatalf("thread settings model=%s effort=%s mode=%s: %#v %v", model, effort, mode, status, err)
		}
	}
	assertSettings(parent.ID, initial.Model, initial.ReasoningEffort, initial.PermissionMode)
	model, effort, mode := "provider/session-override", "", "plan"
	if err := client.UpdateClaudeThreadSettings(t.Context(), parent.ID, &model, &effort, &mode); err != nil {
		t.Fatal(err)
	}
	saved, err := a.store.UpsertRuntimeSession(runtime, sessionFromThread(runtime, parent, true))
	if err != nil {
		t.Fatal(err)
	}
	actor := &sessionActor{agent: a, runtime: runtime, session: saved}
	// Existing session preferences survive a worker default change.
	update := &protocol.WorkerRuntimeSettingsUpdate{Model: "worker-next", ReasoningEffort: "medium", AuthMode: "login", PermissionMode: "dontAsk"}
	if result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update)); result.ErrorCode != "" {
		t.Fatal(result.ErrorCode)
	}
	assertSettings(parent.ID, model, effort, mode)
	result, err := actor.webUINewSession(t.Context(), client, `{"name":"Cleared Claude session"}`)
	if err != nil || result.Session == nil {
		t.Fatalf("clear/new did not create session: %#v %v", result, err)
	}
	assertSettings(result.Session.ThreadID, update.Model, update.ReasoningEffort, update.PermissionMode)
	assertSettings(parent.ID, model, effort, mode)
}

func TestClaudeOversizedEscapedCredentialsPreserveReadableSavedSettings(t *testing.T) {
	a, _ := claudeSettingsAgent(t)
	original := claudeSettingsFile{Model: "saved-model", AuthMode: "login", PermissionMode: "default"}
	path := claudeSettingsPath(a.cfg)
	if err := saveClaudeSettings(path, original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inferenceKey, adminKey := strings.Repeat("&", 8192), strings.Repeat("<", 8192)
	update := &protocol.WorkerRuntimeSettingsUpdate{Model: "oversized-model", AuthMode: "login", PermissionMode: "default", APIKey: &inferenceKey, EnterpriseBudget: &protocol.ClaudeEnterpriseBudgetUpdate{Enabled: true, UserID: "user_Ab123", APIKey: &adminKey}}
	if err := update.Validate(); err != nil {
		t.Fatal("valid individual credential bounds were rejected")
	}
	result := a.runtimeSettings(t.Context(), claudeSettingsRequest(a, "put", update))
	if result.ErrorCode != "save_failed" {
		t.Fatalf("encoded private file limit not enforced: %s", result.ErrorCode)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("oversized settings partially replaced saved configuration", err)
	}
	got, err := readClaudeSettings(path)
	if err != nil || got != original {
		t.Fatal("oversized settings made the previous file unreadable", err)
	}
}
