package worker

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIRejectsWorkerAndClaudeCredentialFilesBeforeTransport(t *testing.T) {
	f := newWebUIFileFixture(t)
	home := f.session.CWD
	t.Setenv("HOME", home)
	configuration := filepath.Join(home, "custom-worker.json")
	settings := filepath.Join(home, "custom-settings", "account.json")
	state := filepath.Join(home, "private-state", "worker.db")
	native := filepath.Join(home, "custom-native")
	for _, directory := range []string{filepath.Dir(settings), filepath.Dir(state), native} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(home, "settings-alias")
	if err := os.Symlink(filepath.Dir(settings), alias); err != nil {
		t.Fatal(err)
	}
	nativeAlias := filepath.Join(home, "native-alias")
	if err := os.Symlink(native, nativeAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", nativeAlias)
	f.pool.c.cfg.ConfigFile = configuration
	f.pool.c.cfg.StateFile = state
	f.pool.c.cfg.Runtimes = []config.RuntimeProfile{{ClaudeSettingsFile: filepath.Join(alias, "account.json"), WorkingDirectory: home}}
	paths := []string{
		configuration, settings, filepath.Join(filepath.Dir(settings), ".runtime-settings-synthetic"), state,
		filepath.Join(home, "private-state", "unlisted-state.json"), filepath.Join(home, "private-state", "worker.db-wal"),
		filepath.Join(home, ".local", "state", "claude-worker", "runtime-settings.json"),
		filepath.Join(home, ".local", "state", "codex-worker", "setup", "enrolled-worker.json"),
		filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude.json.backup"), filepath.Join(native, ".credentials.json"),
		filepath.Join(home, ".env"), filepath.Join(home, ".env.local"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic private file bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		file, _, err := openWebUIFile(home, home, path, []string{home}, workerPrivateFilePaths(f.pool.c.cfg, home, home))
		if err == nil {
			file.Close()
			t.Fatal("credential file opened")
		}
		// The actual worker file_open route is shared by browser open,
		// preview and download. Its first frame must contain no file bytes.
		id := f.open(t, path)
		frame := nextWebUIFrame(t, f.frames)
		if frame.ID != id || frame.Action != "error" || len(frame.Data) != 0 {
			t.Fatal("credential file reached gateway transport")
		}
		f.pool.wg.Wait()
		for _, link := range []string{"symbolic", "hard"} {
			alias := filepath.Join(home, "innocent-"+link)
			var err error
			if link == "symbolic" {
				err = os.Symlink(path, alias)
			} else {
				err = os.Link(path, alias)
			}
			if err != nil {
				t.Fatal(err)
			}
			if file, _, err := openWebUIFile(home, home, alias, []string{home}, workerPrivateFilePaths(f.pool.c.cfg, home, home)); err == nil {
				file.Close()
				t.Fatal("credential alias opened")
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"report.txt", "ordinary-project.json", "source.go"} {
		path := filepath.Join(home, name)
		if err := os.WriteFile(path, []byte("intentionally opened project bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		file, _, err := openWebUIFile(home, home, path, []string{home}, workerPrivateFilePaths(f.pool.c.cfg, home, home))
		if err != nil {
			t.Fatal("ordinary project file blocked", err)
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil || string(data) != "intentionally opened project bytes" {
			t.Fatal("ordinary raw file bytes changed")
		}
	}
}

func TestWebUIProtectsRelativeClaudeConfigDirectoryAndPrivateUploadTargets(t *testing.T) {
	for _, scenario := range []string{"relative-native", "state-directory", "default-native"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			workspace := runtime.DefaultCWD
			switch scenario {
			case "relative-native":
				t.Setenv("CLAUDE_CONFIG_DIR", "account")
				workspace = filepath.Join(workspace, "account")
			case "state-directory":
				a.cfg.StateFile = filepath.Join(workspace, "private-state", "state.db")
				workspace = filepath.Dir(a.cfg.StateFile)
			case "default-native":
				workspace = filepath.Join(workspace, ".claude")
			}
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			// A relative native config path resolves within the active thread's
			// CWD. Test its raw-read projection from the parent workspace too.
			if scenario == "relative-native" {
				path := filepath.Join(workspace, "credentials.json")
				if err := os.WriteFile(path, []byte("synthetic private bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
				if !protectedWebUIFilePath(path, workerPrivateFilePaths(a.cfg, "", runtime.DefaultCWD)) {
					t.Fatal("relative config path not protected")
				}
				// Make this native directory the explicit configured root for
				// the following upload admission check.
				t.Setenv("CLAUDE_CONFIG_DIR", workspace)
			}
			session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "private-upload", CWD: workspace, State: "idle", Loaded: true})
			if err != nil {
				t.Fatal(err)
			}
			a.onSession(runtime, session)
			server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": workspace, "status": "idle"}}, nil)
			upload := protocol.WebUIUpload{UploadID: uuid.NewString(), Name: "report.txt", Data: []byte("synthetic upload")}
			if _, err := a.saveWebUIUpload(t.Context(), runtime, session, upload); err == nil {
				t.Fatal("upload into private directory accepted")
			}
			if _, err := os.Stat(filepath.Join(workspace, "inbound")); !os.IsNotExist(err) {
				t.Fatal("rejected upload created files")
			}
		})
	}
}
