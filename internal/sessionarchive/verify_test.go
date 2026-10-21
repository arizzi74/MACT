package sessionarchive

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVerifyRestoredThreadsReadsSubagentsAcrossRestart(t *testing.T) {
	m := verificationManifest()
	binary, logPath := verificationFixtureBinary(t, m, verificationFault{}, false)
	root, home, project := verificationScratch(t)
	for pass := 1; pass <= 2; pass++ {
		if err := verificationFixturePass(t, binary, root, home, project, m, pass); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	assertVerificationPasses(t, readVerificationLog(t, logPath), m, 2)
}

func TestVerifyRestoredThreadsFailsClosed(t *testing.T) {
	m := verificationManifest()
	child := m.ThreadIDs[len(m.ThreadIDs)-1]
	var faults []verificationFault
	for _, target := range []struct{ method, id string }{{"thread/resume", m.ThreadID}, {"thread/read", child}} {
		for _, kind := range []string{"wrong-id", "missing-id", "wrong-cwd", "missing-cwd", "rpc-error"} {
			faults = append(faults, verificationFault{Method: target.method, ThreadID: target.id, Kind: kind})
		}
	}
	for _, kind := range []string{"relative-cwd", "unclean-cwd", "sibling-cwd"} {
		faults = append(faults, verificationFault{Method: "thread/read", ThreadID: child, Kind: kind})
	}
	for _, id := range []string{m.ThreadID, child} {
		faults = append(faults, verificationFault{Method: "thread/turns/list", ThreadID: id, Kind: "rpc-error"})
	}
	for _, fault := range faults {
		for pass := 1; pass <= 2; pass++ {
			fault.Pass = pass
			t.Run(fmt.Sprintf("%s/%s/%s/pass-%d", fault.Method, fault.ThreadID, fault.Kind, pass), func(t *testing.T) {
				binary, logPath := verificationFixtureBinary(t, m, fault, false)
				root, home, project := verificationScratch(t)
				for current := 1; current <= pass; current++ {
					err := verificationFixturePass(t, binary, root, home, project, m, current)
					if current < pass {
						if err != nil {
							t.Fatalf("successful first pass: %v", err)
						}
						continue
					}
					if err == nil {
						t.Fatalf("accepted %s from %s", fault.Kind, fault.Method)
					}
					if !strings.Contains(err.Error(), fault.ThreadID) || !strings.Contains(err.Error(), fmt.Sprintf("pass %d", pass)) {
						t.Fatalf("failure lacks thread/pass context: %v", err)
					}
					if fault.Kind == "rpc-error" && !strings.Contains(err.Error(), "fixture native error") {
						t.Fatalf("native error was hidden: %v", err)
					}
				}
				events := readVerificationLog(t, logPath)
				last := events[len(events)-1]
				if last.Pass != pass || last.Method != fault.Method || verificationThreadID(t, last) != fault.ThreadID {
					t.Fatalf("verification continued after failure: %#v", last)
				}
			})
		}
	}
}

func TestVerifyRestoresAndRestartsWithoutCredentials(t *testing.T) {
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			o, _ := fixture(t, mode)
			m, err := Export(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{o.CodexHome, o.ProjectPath} {
				if err := os.RemoveAll(source); err != nil {
					t.Fatal(err)
				}
			}
			// These sentinel values must not reach either verification process.
			t.Setenv("OPENAI_API_KEY", "archive-test-credential-must-not-be-inherited")
			t.Setenv("CODEX_HOME", o.CodexHome)
			t.Setenv("CODEX_SQLITE_HOME", o.SQLiteHome)
			t.Setenv("HTTPS_PROXY", "http://archive-test-proxy.invalid")
			binary, logPath := verificationFixtureBinary(t, m, verificationFault{}, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			verified, err := Verify(ctx, m.ArchivePath, VerifyOptions{CodexBinary: binary})
			if err != nil {
				t.Fatal(err)
			}
			if !verified.VerifiedSelfContained || verified.ThreadID != m.ThreadID {
				t.Fatalf("verification result lost proof or root: %#v", verified)
			}
			events := readVerificationLog(t, logPath)
			assertVerificationPasses(t, events, m, 2)
			for _, event := range events {
				if event.Type == "start" {
					if event.Project == o.ProjectPath || event.CodexHome == o.CodexHome {
						t.Fatal("verification used the original source locations")
					}
					if _, err := os.Stat(filepath.Dir(event.Project)); !os.IsNotExist(err) {
						t.Fatalf("private verification restore was not removed: %v", err)
					}
				}
			}
			if _, err := Read(context.Background(), m.ArchivePath); err != nil {
				t.Fatalf("verification changed the archive: %v", err)
			}
		})
	}
}

func TestVerifyDoesNotRecordProofWhenRestartFails(t *testing.T) {
	o, _ := fixture(t, "paginated")
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	fault := verificationFault{Pass: 2, Method: "thread/turns/list", ThreadID: m.ThreadID, Kind: "rpc-error"}
	binary, logPath := verificationFixtureBinary(t, m, fault, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	verified, err := Verify(ctx, m.ArchivePath, VerifyOptions{CodexBinary: binary})
	if err == nil || !strings.Contains(err.Error(), "fixture native error") {
		t.Fatalf("restart failure accepted or hidden: %v", err)
	}
	if verified.VerifiedSelfContained {
		t.Fatal("failed restart was recorded as a self-contained proof")
	}
	assertVerificationPasses(t, readVerificationLog(t, logPath), m, 2)
}

// The root deliberately follows a descendant in this synthetic manifest. The
// verifier must use ThreadID to resume the parent before inspecting descendants.
func verificationManifest() Manifest {
	ids := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
		"00000000-0000-4000-8000-000000000004",
		"00000000-0000-4000-8000-000000000005",
	}
	return Manifest{ThreadID: ids[1], ThreadIDs: ids}
}

type verificationFault struct {
	Pass                   int
	Method, ThreadID, Kind string
}

type verificationFixture struct {
	Manifest     Manifest
	LogPath      string
	Fault        verificationFault
	CheckRestore bool
}

type verificationEvent struct {
	Type, Method string
	Pass, PID    int
	Params       json.RawMessage
	Project      string
	Home         string
	CodexHome    string
	SQLiteHome   string
	Credentials  []string
}

func verificationFixtureBinary(t *testing.T, m Manifest, fault verificationFault, checkRestore bool) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("verification subprocess fixture requires a POSIX shell")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "fixture.json")
	logPath := filepath.Join(dir, "requests.jsonl")
	data, err := json.Marshal(verificationFixture{Manifest: m, LogPath: logPath, Fault: fault, CheckRestore: checkRestore})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	wrapper := filepath.Join(dir, "app-server-fixture")
	script := "#!/bin/sh\nexec " + quote(binary) + " -test.run=^TestArchiveVerificationAppServerProcess$ -- --archive-verification-fixture " + quote(configPath) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper, logPath
}

func verificationScratch(t *testing.T) (root, home, project string) {
	t.Helper()
	root = t.TempDir()
	home, project = filepath.Join(root, "codex"), filepath.Join(root, "project")
	for _, dir := range []string{home, filepath.Join(project, "src", "pkg")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root, home, project
}

func verificationFixturePass(t *testing.T, binary, root, home, project string, m Manifest, pass int) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := startVerificationServer(ctx, binary, home, project, root)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	return verifyRestoredThreads(client, m, project, pass)
}

func readVerificationLog(t *testing.T, filename string) []verificationEvent {
	t.Helper()
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []verificationEvent
	decoder := json.NewDecoder(f)
	for {
		var event verificationEvent
		if err := decoder.Decode(&event); err == io.EOF {
			return events
		} else if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func verificationThreadID(t *testing.T, event verificationEvent) string {
	t.Helper()
	var params struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(event.Params, &params); err != nil {
		t.Fatal(err)
	}
	return params.ThreadID
}

func assertVerificationPasses(t *testing.T, events []verificationEvent, m Manifest, passes int) {
	t.Helper()
	starts := map[int]verificationEvent{}
	requests := map[int][]verificationEvent{}
	for _, event := range events {
		if event.Type == "start" {
			if _, exists := starts[event.Pass]; exists {
				t.Fatalf("duplicate process for pass %d", event.Pass)
			}
			starts[event.Pass] = event
			root := filepath.Dir(event.Project)
			if event.Home != filepath.Join(root, "user-home") || event.CodexHome != filepath.Join(root, "codex") || event.SQLiteHome != event.CodexHome || len(event.Credentials) != 0 {
				t.Fatalf("verification process was not isolated: %#v", event)
			}
		} else {
			requests[event.Pass] = append(requests[event.Pass], event)
		}
	}
	if len(starts) != passes {
		t.Fatalf("got %d app-server processes, want %d", len(starts), passes)
	}
	for pass := 1; pass <= passes; pass++ {
		if starts[pass].PID == 0 || pass > 1 && starts[pass].PID == starts[pass-1].PID {
			t.Fatalf("pass %d did not start an independent process", pass)
		}
		if pass > 1 && starts[pass].Project != starts[pass-1].Project {
			t.Fatal("restart changed the restored project")
		}
		want := map[string]int{"initialize:": 1, "initialized:": 1, "thread/resume:" + m.ThreadID: 1, "thread/turns/list:" + m.ThreadID: 1}
		for _, id := range m.ThreadIDs {
			if id != m.ThreadID {
				want["thread/read:"+id], want["thread/turns/list:"+id] = 1, 1
			}
		}
		got := map[string]int{}
		for _, event := range requests[pass] {
			got[event.Method+":"+verificationThreadID(t, event)]++
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d calls:\n got %v\nwant %v", pass, got, want)
		}
	}
}

// This subprocess uses the real stdio transport and loses its loaded-thread
// state on every restart. It rejects standalone v2 subagent resumes and any
// request that could start model work, and records requests before responding.
func TestArchiveVerificationAppServerProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--archive-verification-fixture" && i+1 < len(os.Args) {
			if err := runVerificationFixture(os.Args[i+1], os.Args[i+2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(70)
			}
			os.Exit(0)
		}
	}
}

func runVerificationFixture(configPath string, args []string) error {
	if !reflect.DeepEqual(args, []string{"app-server", "--listen", "stdio://", "-c", "features.plugins=false"}) {
		return fmt.Errorf("unexpected app-server arguments: %v", args)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var config verificationFixture
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	pass := 1
	if previous, err := os.Open(config.LogPath); err == nil {
		decoder := json.NewDecoder(previous)
		for {
			var event verificationEvent
			if err := decoder.Decode(&event); err != nil {
				previous.Close()
				if err != io.EOF {
					return err
				}
				break
			}
			if event.Type == "start" {
				pass++
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	log, err := os.OpenFile(config.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	project, err := os.Getwd()
	if err != nil {
		return err
	}
	start := verificationEvent{Type: "start", Pass: pass, PID: os.Getpid(), Project: project, Home: os.Getenv("HOME"), CodexHome: os.Getenv("CODEX_HOME"), SQLiteHome: os.Getenv("CODEX_SQLITE_HOME")}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_AUTH_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if os.Getenv(key) != "" {
			start.Credentials = append(start.Credentials, key)
		}
	}
	if err := json.NewEncoder(log).Encode(start); err != nil {
		return err
	}
	if config.CheckRestore {
		if content, err := os.ReadFile(filepath.Join(project, "keep.txt")); err != nil || string(content) != "project payload\n" {
			return fmt.Errorf("restored project missing: %v", err)
		}
		content, err := os.ReadFile(filepath.Join(start.CodexHome, "config.toml"))
		if err != nil || !strings.Contains(string(content), "base_url = \"http://127.0.0.1:1\"") || !strings.Contains(string(content), "requires_openai_auth = false") {
			return fmt.Errorf("offline verification configuration missing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(start.CodexHome, "auth.json")); !os.IsNotExist(err) {
			return fmt.Errorf("unexpected authentication file: %v", err)
		}
	}
	loaded := map[string]bool{}
	resumed := false
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return err
		}
		if err := json.NewEncoder(log).Encode(verificationEvent{Type: "request", Pass: pass, PID: os.Getpid(), Method: request.Method, Params: request.Params}); err != nil {
			return err
		}
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return err
		}
		id, _ := params["threadId"].(string)
		response := map[string]any{"id": request.ID}
		result := map[string]any{}
		failure := ""
		switch request.Method {
		case "initialize":
		case "initialized":
			continue
		case "thread/resume":
			if id != config.Manifest.ThreadID {
				failure = "cannot resume unloaded multi-agent v2 sub-agent through parent; resume parent first or thread/read"
			} else if resumed {
				failure = "parent resumed more than once"
			} else if params["cwd"] != project || params["excludeTurns"] != true || params["modelProvider"] != "archive_verify" || params["model"] != "archive-proof" {
				failure = "invalid isolated parent resume parameters"
			} else {
				resumed, loaded[id] = true, true
				result["thread"] = map[string]any{"id": id, "cwd": project}
			}
		case "thread/read":
			known := false
			for _, archived := range config.Manifest.ThreadIDs {
				known = known || archived == id
			}
			if !resumed || id == config.Manifest.ThreadID || !known || params["includeTurns"] != false {
				failure = "descendant metadata read must follow parent resume and exclude turns"
			} else {
				loaded[id] = true
				cwd := project
				if id == config.Manifest.ThreadIDs[len(config.Manifest.ThreadIDs)-1] {
					cwd = filepath.Join(project, "src", "pkg")
				}
				result["thread"] = map[string]any{"id": id, "cwd": cwd}
			}
		case "thread/turns/list":
			if !loaded[id] || params["limit"] != float64(1) || params["itemsView"] != "notLoaded" {
				failure = "history must be bounded and follow metadata validation"
			} else {
				// An old failed turn is history, not a failed history-read RPC.
				result["data"] = []any{map[string]any{"id": "historical-failed-turn", "status": "failed", "error": map[string]any{"message": "historical model failure"}, "items": []any{}}}
				result["nextCursor"] = nil
			}
		default:
			failure = "unexpected request; verification may not start model work: " + request.Method
		}
		fault := config.Fault
		if fault.Pass == pass && fault.Method == request.Method && fault.ThreadID == id && failure == "" {
			if fault.Kind == "rpc-error" {
				failure = "fixture native error"
			} else {
				thread := result["thread"].(map[string]any)
				switch fault.Kind {
				case "wrong-id":
					other := config.Manifest.ThreadID
					if id == other {
						other = config.Manifest.ThreadIDs[0]
					}
					thread["id"] = other
				case "missing-id":
					delete(thread, "id")
				case "wrong-cwd":
					thread["cwd"] = filepath.Dir(project)
				case "missing-cwd":
					delete(thread, "cwd")
				case "relative-cwd":
					thread["cwd"] = "src/pkg"
				case "unclean-cwd":
					thread["cwd"] = project + "/src/../pkg"
				case "sibling-cwd":
					thread["cwd"] = project + "-neighbor"
				}
			}
		}
		if failure != "" {
			response["error"] = map[string]any{"code": -32600, "message": failure}
		} else {
			response["result"] = result
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}
