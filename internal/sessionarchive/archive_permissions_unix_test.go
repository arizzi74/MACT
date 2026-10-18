//go:build unix

package sessionarchive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestArchivePermissionsUnderRestrictiveUmask(t *testing.T) {
	const childEnv = "SESSIONARCHIVE_TEST_UMASK_0077"
	if os.Getenv(childEnv) != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestArchivePermissionsUnderRestrictiveUmask$", "-test.v")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("umask 0077 subprocess: %v\n%s", err, output)
		}
		return
	}
	// Umask is process-wide: only this dedicated subprocess may change it.
	syscall.Umask(0077)
	t.Run("export-and-restore", testArchivePermissionRoundTrip)
	t.Run("unpack", testUnpackPermissions)
	t.Run("source-permission-changes", testCopyRejectsSourcePermissionChanges)
}

func permissionFixture(t *testing.T) (ExportOptions, string, map[string]os.FileMode) {
	t.Helper()
	o, root := fixture(t, "legacy")
	modes := map[string]os.FileMode{
		".":               0755,
		"keep.txt":        0640,
		"README.md":       0644,
		"bin":             0755,
		"bin/run.sh":      0755,
		"bin/private.txt": 0640,
	}
	if err := os.Mkdir(filepath.Join(o.ProjectPath, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README.md", "bin/run.sh", "bin/private.txt"} {
		if err := os.WriteFile(filepath.Join(o.ProjectPath, name), []byte("payload: "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, mode := range modes {
		// Explicit chmod makes these the actual source permissions even under 0077.
		if err := os.Chmod(filepath.Join(o.ProjectPath, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	assertProjectModes(t, o.ProjectPath, modes)
	return o, root, modes
}

func assertProjectModes(t *testing.T, project string, modes map[string]os.FileMode) {
	t.Helper()
	for name, want := range modes {
		info, err := os.Lstat(filepath.Join(project, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions = %04o, want %04o", name, got, want)
		}
	}
}

func assertManifestProjectModes(t *testing.T, m Manifest, modes map[string]os.FileMode) {
	t.Helper()
	entries := make(map[string]Entry)
	for _, entry := range m.Files {
		entries[entry.Path] = entry
	}
	for name, want := range modes {
		entry, ok := entries[filepath.ToSlash(filepath.Join("project", name))]
		if !ok {
			t.Errorf("manifest is missing project entry %q", name)
			continue
		}
		if got := os.FileMode(entry.Mode); got != want {
			t.Errorf("manifest %s permissions = %04o, want %04o", name, got, want)
		}
	}
}

func testArchivePermissionRoundTrip(t *testing.T) {
	ctx := context.Background()
	o, root, modes := permissionFixture(t)
	o.Progress = func(message string) {
		if message != "Compressing and checking archive" {
			return
		}
		stages, err := filepath.Glob(filepath.Join(o.ArchiveDir, ".archive-stage-*"))
		if err != nil || len(stages) != 1 {
			t.Fatalf("find private archive stage: %v, %v", stages, err)
		}
		assertProjectModes(t, stages[0], map[string]os.FileMode{".": 0700})
	}
	m, err := Export(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	assertManifestProjectModes(t, m, modes)
	verified, err := Read(ctx, m.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	assertManifestProjectModes(t, verified, modes)
	assertProjectModes(t, o.ArchiveDir, map[string]os.FileMode{".": 0700, o.ArchiveID: 0600})
	if err := CheckProject(ctx, o.ProjectPath, verified); err != nil {
		t.Fatalf("unchanged live project must match archive: %v", err)
	}
	if err := os.RemoveAll(o.ProjectPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(o.CodexHome); err != nil {
		t.Fatal(err)
	}
	restore := RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: filepath.Join(root, "restored-home"), Destination: filepath.Join(root, "restored-project"), CodexVersion: o.CodexVersion}
	if _, err := Restore(ctx, restore); err != nil {
		t.Fatalf("restore without original project or native source: %v", err)
	}
	assertProjectModes(t, restore.Destination, modes)
	if err := CheckProject(ctx, restore.Destination, verified); err != nil {
		t.Fatalf("restored project must match archive: %v", err)
	}
	if _, err := Restore(ctx, restore); err != nil {
		t.Fatalf("identical restore replay: %v", err)
	}
	assertProjectModes(t, restore.Destination, modes)
	for _, mutation := range []string{"contents", "permissions", "extra-entry"} {
		t.Run(mutation, func(t *testing.T) {
			filename := filepath.Join(restore.Destination, "README.md")
			switch mutation {
			case "contents":
				original, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				changed := append([]byte(nil), original...)
				changed[0] ^= 1
				if err := os.WriteFile(filename, changed, 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(filename, original, 0600); err != nil {
						t.Error(err)
					}
				})
			case "permissions":
				if err := os.Chmod(filename, 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(filename, modes["README.md"]); err != nil {
						t.Error(err)
					}
				})
			case "extra-entry":
				filename = filepath.Join(restore.Destination, "new.txt")
				if err := os.WriteFile(filename, []byte("new file"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(filename); err != nil {
						t.Error(err)
					}
				})
			}
			if err := CheckProject(ctx, restore.Destination, verified); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed project must fail verification: %v", err)
			}
			if _, err := Restore(ctx, restore); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed project must reject restore replay: %v", err)
			}
		})
	}
}

func testUnpackPermissions(t *testing.T) {
	ctx := context.Background()
	o, _, modes := permissionFixture(t)
	m, err := Export(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	// Set the archive modes explicitly so this catches extraction regressions
	// independently of whether Export preserved the source permissions.
	filename := rewriteBundle(t, m, func(stage string, _ *Manifest) {
		for name, mode := range modes {
			if err := os.Chmod(filepath.Join(stage, "project", name), mode); err != nil {
				t.Fatal(err)
			}
		}
	})
	stage := t.TempDir()
	if _, err := unpack(ctx, filename, stage); err != nil {
		t.Fatal(err)
	}
	assertProjectModes(t, stage, map[string]os.FileMode{".": 0700})
	assertProjectModes(t, filepath.Join(stage, "project"), modes)
}

type permissionChangeContext struct {
	context.Context
	once   sync.Once
	change func()
}

func (c *permissionChangeContext) Err() error {
	c.once.Do(c.change)
	return c.Context.Err()
}

func testCopyRejectsSourcePermissionChanges(t *testing.T) {
	for _, when := range []string{"before-open", "during-copy"} {
		t.Run(when, func(t *testing.T) {
			root := t.TempDir()
			src, dst := filepath.Join(root, "source"), filepath.Join(root, "copy")
			if err := os.WriteFile(src, []byte("unchanged payload"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(src, 0644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			change := func() {
				if err := os.Chmod(src, 0640); err != nil {
					t.Fatal(err)
				}
			}
			var ctx context.Context = context.Background()
			if when == "before-open" {
				change()
			} else {
				ctx = &permissionChangeContext{Context: ctx, change: change}
			}
			if err := copyFile(ctx, src, dst, before); err == nil || !strings.Contains(err.Error(), "source changed during archive") {
				t.Fatalf("source chmod must reject snapshot: %v", err)
			}
			if _, err := os.Lstat(dst); !os.IsNotExist(err) {
				t.Fatalf("failed copy must not leave a target: %v", err)
			}
		})
	}
}
