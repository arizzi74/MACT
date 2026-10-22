package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagementCommandLayoutsPreserveInstallationPaths(t *testing.T) {
	for _, platform := range []struct{ component, system, home, command, alias string }{
		{"gateway", "linux", "", "/usr/local/bin/codex-telegramgw", "/usr/local/bin/mact"},
		{"worker", "linux", "/home/worker", "/home/worker/.local/bin/codex-telegramgw", "/home/worker/.local/bin/mact"},
		{"worker", "darwin", "/Users/worker", "/Users/worker/.local/bin/codex-telegramgw", "/Users/worker/.local/bin/mact"},
	} {
		t.Run(platform.component+"-"+platform.system, func(t *testing.T) {
			l, err := newLayout(platform.component, platform.system, "arm64", platform.home)
			if err != nil {
				t.Fatal(err)
			}
			if l.Command != platform.command || l.AliasCommand != platform.alias || filepath.Base(l.Manager) != "codex-telegramgw" || filepath.Base(l.Binary) != "codex-"+platform.component {
				t.Fatalf("management alias changed canonical installation paths: %+v", l)
			}
		})
	}
}

func TestManagerCommandsShareOneExecutableAcrossUpdates(t *testing.T) {
	for _, system := range []string{"linux", "darwin"} {
		t.Run(system, func(t *testing.T) {
			l := platformWorker(t, system)
			source := filepath.Join(t.TempDir(), "manager")
			for _, version := range []string{"old", "new"} {
				platformWrite(t, source, "#!/bin/sh\nprintf '"+version+"\\n'\n", 0755)
				if err := (&Manager{}).InstallManager(l, source); err != nil {
					t.Fatal(err)
				}
				for _, command := range []string{l.Command, l.AliasCommand} {
					if link, err := os.Readlink(command); err != nil || link != l.Manager {
						t.Fatalf("%s links to %q: %v", command, link, err)
					}
					if output, err := exec.Command(command, "version").Output(); err != nil || string(output) != version+"\n" {
						t.Fatalf("%s output = %q: %v", command, output, err)
					}
				}
			}
			entries, err := os.ReadDir(l.Bin)
			if err != nil || len(entries) != 2 {
				t.Fatalf("duplicate management commands or temporary files: %v, %v", entries, err)
			}
			original, err := os.Stat(l.Command)
			if err != nil {
				t.Fatal(err)
			}
			alias, err := os.Stat(l.AliasCommand)
			if err != nil || !os.SameFile(original, alias) {
				t.Fatalf("management names do not share the native executable: %v", err)
			}
		})
	}
}

func TestManagerMigratesOwnedMACTLinks(t *testing.T) {
	for _, target := range []string{"manager", "legacy", "original-command", "relative-command"} {
		t.Run(target, func(t *testing.T) {
			l := platformWorker(t, "linux")
			if err := os.MkdirAll(l.Bin, 0755); err != nil {
				t.Fatal(err)
			}
			link := map[string]string{"manager": l.Manager, "legacy": l.LegacyManager, "original-command": l.Command, "relative-command": "codex-telegramgw"}[target]
			if err := os.Symlink(link, l.AliasCommand); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "manager")
			platformWrite(t, source, "native manager", 0755)
			if err := (&Manager{}).InstallManager(l, source); err != nil {
				t.Fatal(err)
			}
			if installed, err := os.Readlink(l.AliasCommand); err != nil || installed != l.Manager {
				t.Fatalf("alias links to %q: %v", installed, err)
			}
		})
	}
}

func TestManagerPreservesOccupiedMACTWithoutBlockingInstallation(t *testing.T) {
	for _, collision := range []string{"file", "directory", "symlink", "dangling-symlink"} {
		t.Run(collision, func(t *testing.T) {
			l := platformWorker(t, "linux")
			if err := os.MkdirAll(l.Bin, 0755); err != nil {
				t.Fatal(err)
			}
			switch collision {
			case "file":
				platformWrite(t, l.AliasCommand, "another mact command", 0755)
			case "directory":
				if err := os.Mkdir(l.AliasCommand, 0755); err != nil {
					t.Fatal(err)
				}
			default:
				target := filepath.Join(t.TempDir(), "unrelated")
				if collision == "symlink" {
					platformWrite(t, target, "another mact command", 0755)
				}
				if err := os.Symlink(target, l.AliasCommand); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(l.AliasCommand)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "manager")
			platformWrite(t, source, "new manager", 0755)
			var output bytes.Buffer
			if err := (&Manager{Out: &output}).InstallManager(l, source); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(l.AliasCommand)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("unrelated mact command was replaced: %v", err)
			}
			if data := updateRead(t, l.Command); data != "new manager" {
				t.Fatalf("original command did not update: %q", data)
			}
			if !strings.Contains(output.String(), l.AliasCommand+" is already occupied") || !strings.Contains(output.String(), l.Command) {
				t.Fatalf("missing actionable alias warning: %s", output.String())
			}
		})
	}
}

func TestUpdateRestoresManagementAliasBeforeServiceRestart(t *testing.T) {
	for _, target := range []string{"absent", "legacy", "original-command", "unrelated-directory"} {
		t.Run(target, func(t *testing.T) {
			l, packages, release := updateFixture(t, "gateway")
			oldLink := map[string]string{"legacy": l.LegacyManager, "original-command": "codex-telegramgw"}[target]
			if oldLink != "" {
				if err := os.Symlink(oldLink, l.AliasCommand); err != nil {
					t.Fatal(err)
				}
			} else if target == "unrelated-directory" {
				if err := os.Mkdir(l.AliasCommand, 0755); err != nil {
					t.Fatal(err)
				}
			}
			checkAlias := func() {
				t.Helper()
				if target == "absent" {
					if _, err := os.Lstat(l.AliasCommand); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("new alias survived rollback: %v", err)
					}
				} else if target == "unrelated-directory" {
					if info, err := os.Lstat(l.AliasCommand); err != nil || !info.IsDir() {
						t.Fatalf("foreign alias directory changed: %v", err)
					}
				} else if link, err := os.Readlink(l.AliasCommand); err != nil || link != oldLink {
					t.Fatalf("original alias not restored: %q %v", link, err)
				}
			}
			m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "runuser" {
					return CommandResult{ExitCode: 1}, nil
				}
				if args[0] != "systemctl" {
					t.Fatalf("unexpected command: %v", args)
				}
				if args[1] == "start" {
					checkAlias()
				}
				return CommandResult{}, nil
			}}
			if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err == nil {
				t.Fatal("migration failure was ignored")
			}
			checkAlias()
		})
	}
}

func TestManagementHelpUsesMACTAndExplainsCompatibility(t *testing.T) {
	var output, errorOutput bytes.Buffer
	if code := Execute(context.Background(), []string{"--help"}, &output, &errorOutput); code != 0 || errorOutput.Len() != 0 {
		t.Fatalf("help returned %d: %s", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "Install and update MACT") || !strings.Contains(output.String(), "mact update gateway|worker") || !strings.Contains(output.String(), "codex-telegramgw command remains supported") {
		t.Fatalf("help omits MACT or compatibility command: %s", output.String())
	}
}
