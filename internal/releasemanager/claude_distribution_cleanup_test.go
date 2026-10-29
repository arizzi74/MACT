package releasemanager

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func claudeOrphanStageFixture(t *testing.T, versionsDir, name string, directoryTime, binaryTime time.Time, binaryMode os.FileMode) string {
	t.Helper()
	stage := filepath.Join(versionsDir, name)
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if binaryMode != 0 {
		binary := filepath.Join(stage, "claude")
		if err := os.WriteFile(binary, []byte("orphaned private candidate"), binaryMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(binary, binaryMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(binary, binaryTime, binaryTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stage, directoryTime, directoryTime); err != nil {
		t.Fatal(err)
	}
	return stage
}

func TestClaudeDistributionCleanupRemovesOnlyStalePrivateStages(t *testing.T) {
	_, launcher, _, native := claudeDistributionFixture(t)
	versionsDir := filepath.Dir(native)
	now := time.Now().UTC().Truncate(time.Second)
	old, recent := now.Add(-25*time.Hour), now.Add(-time.Hour)
	inputs := []struct {
		name       string
		dir, file  time.Time
		permission os.FileMode
		removed    bool
	}{
		{".mact-stage-empty", old, old, 0, true},
		{".mact-stage-complete", old, old, 0700, true},
		{".mact-stage-partial", old, old, 0600, true},
		{".mact-stage-fresh", recent, recent, 0700, false},
		{".mact-stage-recent-binary", old, recent, 0600, false},
		{".mact-stage-recent-directory", recent, old, 0700, false},
		{".mact-stage-exact-boundary", now.Add(-24 * time.Hour), old, 0700, false},
		{".mact-stage-shared-binary", old, old, 0644, false},
		{".mact-stage-", old, old, 0700, false},
		{"unrelated-stage", old, old, 0700, false},
	}
	for _, input := range inputs {
		claudeOrphanStageFixture(t, versionsDir, input.name, input.dir, input.file, input.permission)
	}
	if err := cleanupClaudeRuntimeStages(versionsDir, now); err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		_, err := os.Lstat(filepath.Join(versionsDir, input.name))
		if input.removed && !errors.Is(err, os.ErrNotExist) || !input.removed && err != nil {
			t.Errorf("cleanup %s removed=%v, error=%v", input.name, input.removed, err)
		}
	}
	if link, err := os.Readlink(launcher); err != nil || link != native {
		t.Fatalf("cleanup changed native launcher=%q, %v", link, err)
	}
	if _, err := os.Lstat(native); err != nil {
		t.Fatalf("cleanup removed native version: %v", err)
	}
}

func TestClaudeDistributionCleanupPreservesUnknownEntriesAndSymlinks(t *testing.T) {
	for _, kind := range []string{"unknown-file", "extra-file", "nested-directory", "stage-symlink", "binary-symlink", "shared-directory", "stage-regular-file", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			_, _, _, native := claudeDistributionFixture(t)
			versionsDir := filepath.Dir(native)
			now := time.Now().UTC().Truncate(time.Second)
			old := now.Add(-25 * time.Hour)
			stage := claudeOrphanStageFixture(t, versionsDir, ".mact-stage-preserve", old, old, 0700)
			binary := filepath.Join(stage, "claude")
			external := filepath.Join(t.TempDir(), "external")
			if err := os.WriteFile(external, []byte("unrelated external data"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unknown-file":
				if err := os.Rename(binary, filepath.Join(stage, "unknown")); err != nil {
					t.Fatal(err)
				}
			case "extra-file":
				if err := os.WriteFile(filepath.Join(stage, "receipt"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nested-directory", "binary-symlink":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if kind == "nested-directory" {
					if err := os.Mkdir(binary, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(external, binary); err != nil {
					t.Fatal(err)
				}
			case "stage-symlink":
				if err := os.Rename(stage, stage+".target"); err != nil {
					t.Fatal(err)
				}
				// Keep the target outside the reserved stage namespace as well.
				if err := os.Rename(stage+".target", filepath.Join(versionsDir, "unrelated-directory")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("unrelated-directory", stage); err != nil {
					t.Fatal(err)
				}
			case "shared-directory":
				if err := os.Chmod(stage, 0755); err != nil {
					t.Fatal(err)
				}
			case "stage-regular-file":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(stage); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stage, []byte("unrelated file"), 0700); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				if os.Geteuid() != 0 {
					t.Skip("changing ownership requires root")
				}
				if err := os.Chown(binary, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(stage, old, old); err != nil {
				t.Fatal(err)
			}
			if err := cleanupClaudeRuntimeStages(versionsDir, now); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(stage); err != nil {
				t.Fatalf("cleanup removed an unrecognized stage: %v", err)
			}
			if data, err := os.ReadFile(external); err != nil || string(data) != "unrelated external data" {
				t.Fatalf("cleanup changed external data=%q, %v", data, err)
			}
			if kind != "unknown-file" && kind != "stage-regular-file" {
				if _, err := os.Lstat(binary); err != nil {
					t.Fatalf("cleanup changed an unrecognized stage entry: %v", err)
				}
			}
		})
	}
}

func TestClaudeDistributionCleanupUsesOnlyExplicitVerifiedRoot(t *testing.T) {
	_, _, _, native := claudeDistributionFixture(t)
	versionsDir := filepath.Dir(native)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)
	stage := claudeOrphanStageFixture(t, versionsDir, ".mact-stage-orphan", old, old, 0600)
	// Recovery does not need a launcher, native executable, or today's policy.
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "/unrelated/config")
	t.Setenv("DISABLE_UPDATES", "1")
	if err := cleanupClaudeRuntimeStages(versionsDir, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan stage was retained despite explicit root: %v", err)
	}
	if err := os.Rename(versionsDir, versionsDir+".target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versionsDir+".target", versionsDir); err != nil {
		t.Fatal(err)
	}
	if err := cleanupClaudeRuntimeStages(versionsDir, now); err == nil {
		t.Fatal("accepted a symbolic staging root")
	}
	if _, err := os.Readlink(versionsDir); err != nil {
		t.Fatalf("cleanup changed symbolic staging root: %v", err)
	}
}

func TestClaudeDistributionStageCleansOldOrphansBeforeNetworking(t *testing.T) {
	m, binary, _, native := claudeDistributionFixture(t)
	distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-25 * time.Hour)
	stage := claudeOrphanStageFixture(t, filepath.Dir(native), ".mact-stage-interrupted", old, old, 0600)
	m.Now = func() time.Time { return now }
	m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})
	if _, err := m.stageClaudeRuntime(t.Context(), distribution, "2.1.312"); err == nil {
		t.Fatal("offline staging unexpectedly succeeded")
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging did not clean its old orphan: %v", err)
	}
}
