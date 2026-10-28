//go:build linux

package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Exercises the installed app-server with a private HOME and offline provider.
// No real credentials, existing conversations, or model turns are accessed.
// CODEX_INVENTORY_NATIVE_TEST=1 go test ./internal/worker -run '^TestNativeInventoryPreservesUnloadedEmptySessions$' -count=1
func TestNativeInventoryPreservesUnloadedEmptySessions(t *testing.T) {
	if os.Getenv("CODEX_INVENTORY_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_INVENTORY_NATIVE_TEST=1 for isolated installed-Codex inventory proof")
	}
	binary := os.Getenv("CODEX_REAL_BINARY")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	codexHome, project := filepath.Join(home, ".codex"), filepath.Join(home, "project")
	for _, dir := range []string{codexHome, project} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	configuration := "model_provider = \"isolated\"\nmodel = \"inventory-test\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Offline inventory proof\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[features]\nplugins = false\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := func() (*codexadapter.Client, *inventoryNativeWrites) {
		t.Helper()
		command := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
		command.Dir = project
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + codexHome, "CODEX_SQLITE_HOME=" + codexHome}
		command.Stderr = io.Discard
		in, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		wait, stopped := make(chan error, 1), make(chan struct{})
		go func() { wait <- command.Wait(); close(stopped) }()
		recorder := &inventoryNativeWrites{WriteCloser: in, methods: make(map[string]int)}
		var closeOnce sync.Once
		client := codexadapter.New(codexadapter.Transport{In: recorder, Out: out, Wait: wait, Close: func() error {
			closeOnce.Do(func() {
				_ = in.Close()
				_ = out.Close()
				_ = command.Process.Kill()
				<-stopped
			})
			return nil
		}}, codexadapter.Config{Command: binary, ClientInfo: codexadapter.ClientInfo{Name: "telegramgw", Version: "inventory-native-test"}, RequestTimeout: 10 * time.Second})
		t.Cleanup(func() { _ = client.Close() })
		if err := client.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
		return client, recorder
	}
	first, _ := start()
	fixtures := []struct {
		name     string
		hidden   bool
		threadID string
		path     string
	}{
		{name: "visible", hidden: false},
		{name: "previously-hidden", hidden: true},
	}
	for index := range fixtures {
		fixture := &fixtures[index]
		thread, err := first.StartThread(ctx, codexadapter.ThreadOptions{CWD: project, HistoryMode: "legacy"})
		if err != nil {
			t.Fatal(err)
		}
		if err := first.RenameThread(ctx, thread.ID, "Empty inventory "+fixture.name); err != nil {
			t.Fatal(err)
		}
		thread, err = first.ReadThread(ctx, thread.ID, false)
		if err != nil || !thread.UserSession() || thread.Preview != "" {
			t.Fatalf("empty user-thread precondition: source=%q preview=%q error=%v", thread.Source, thread.Preview, err)
		}
		var wire struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(thread.Raw, &wire); err != nil || wire.Path == "" {
			t.Fatalf("native persisted path missing: %v", err)
		}
		fixture.threadID, fixture.path = thread.ID, wire.Path
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	client, recorded := start()
	assertUnloaded := func(threadID string) {
		t.Helper()
		loaded, complete, err := loadedThreadIDs(ctx, client)
		if err != nil || !complete || loaded[threadID] {
			t.Fatalf("empty thread was loaded: loaded=%v complete=%v error=%v", loaded[threadID], complete, err)
		}
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			assertUnloaded(fixture.threadID)
			threads, complete, err := client.ListAllThreadsComplete(ctx, 100, 1000)
			if err != nil || !complete {
				t.Fatalf("native listing failed: complete=%v error=%v", complete, err)
			}
			for _, thread := range threads {
				if thread.ID == fixture.threadID {
					t.Fatal("native no longer omits empty threads; update the regression precondition")
				}
			}
			before, err := os.ReadFile(fixture.path)
			if err != nil {
				t.Fatal(err)
			}
			store, cfg := testConnectionStore(t, uuid.NewString())
			defer store.Close()
			cfg.AllowedWorkspaceRoots = []string{project}
			runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: cfg.WorkerID, Generation: 1, State: "running", DefaultCWD: project}
			zero := int64(0)
			prior, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: fixture.threadID, Name: "Empty inventory " + fixture.name, CWD: project, State: "not_loaded", Archived: fixture.hidden, Stats: &protocol.SessionStats{PromptCount: &zero, AssistantMessageCount: &zero, HistoryComplete: true}})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := NewAgent(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			agentContext, stopAgent := context.WithCancel(ctx)
			agent.ctx = agentContext
			defer func() { stopAgent(); agent.group.Wait() }()
			agent.manager.install(runtime, client)
			for attempt := 0; attempt < 2; attempt++ {
				if err := agent.manager.discover(ctx, runtime, client); err != nil {
					t.Fatal(err)
				}
				saved := inventoryByThread(t, store, runtime)[fixture.threadID]
				if saved.ID != prior.ID || saved.ThreadID != prior.ThreadID || saved.Archived || saved.Deleted || saved.Loaded || saved.State != "not_loaded" || saved.Name != prior.Name {
					t.Fatalf("empty session identity or cold visibility changed: id=%q archived=%v loaded=%v state=%q", saved.ID, saved.Archived, saved.Loaded, saved.State)
				}
			}
			assertUnloaded(fixture.threadID)
			after, err := os.ReadFile(fixture.path)
			if err != nil || sha256.Sum256(after) != sha256.Sum256(before) {
				t.Fatalf("read-only discovery changed empty rollout: %v", err)
			}
			if err := client.ArchiveThread(ctx, fixture.threadID); err != nil {
				t.Fatal(err)
			}
			archived, err := client.ReadThread(ctx, fixture.threadID, false)
			var wire struct {
				Path string `json:"path"`
			}
			if err != nil || json.Unmarshal(archived.Raw, &wire) != nil || !filepath.IsAbs(wire.Path) || filepath.Dir(wire.Path) != filepath.Join(codexHome, "archived_sessions") {
				t.Fatalf("archived thread must remain readable with an archived path: path=%q error=%v", wire.Path, err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := agent.manager.discover(ctx, runtime, client); err != nil {
					t.Fatal(err)
				}
				saved := inventoryByThread(t, store, runtime)[fixture.threadID]
				if saved.ID != prior.ID || !saved.Archived || saved.Loaded {
					t.Fatalf("native archived thread was resurrected: id=%q archived=%v loaded=%v", saved.ID, saved.Archived, saved.Loaded)
				}
			}
			assertUnloaded(fixture.threadID)
		})
	}
	for _, method := range []string{"thread/start", "thread/resume", "turn/start", "thread/unarchive", "thread/settings/update", "thread/name/set"} {
		if count := recorded.count(method); count != 0 {
			t.Errorf("restarted app-server received forbidden mutation %s %d times", method, count)
		}
	}
	t.Log("Native empty threads survived restart and remained cold with original IDs; native archival stayed hidden, with no resumes or model turns.")
}

// Records method names only, never parameters, responses, or conversation text.
type inventoryNativeWrites struct {
	io.WriteCloser
	mu      sync.Mutex
	buffer  []byte
	methods map[string]int
}

func (w *inventoryNativeWrites) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.buffer = append(w.buffer, data...)
	for {
		line, remainder, found := bytes.Cut(w.buffer, []byte{'\n'})
		if !found {
			break
		}
		var request struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(line, &request) == nil && request.Method != "" {
			w.methods[request.Method]++
		}
		w.buffer = remainder
	}
	w.mu.Unlock()
	return w.WriteCloser.Write(data)
}

func (w *inventoryNativeWrites) count(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.methods[method]
}
