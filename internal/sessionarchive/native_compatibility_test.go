package sessionarchive

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNativeSelfContainedVersionAndSchemaGuards(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_ARCHIVE_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_ARCHIVE_TEST=1 for exact native schema and compatibility proof")
	}
	o, _ := nativeFixture(t, "legacy")
	for _, name := range nativeDBs {
		db, err := openDB(filepath.Join(o.SQLiteHome, name), true)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, _, err := nativeSchemaFingerprint(t.Context(), db)
		db.Close()
		if err != nil || fingerprint != nativeSchemaFingerprints[o.CodexVersion][name] {
			t.Fatalf("native %s schema changed: %s %v", name, fingerprint, err)
		}
		t.Logf("Codex %s %s schema %s", o.CodexVersion, name, fingerprint)
	}
	m, err := Export(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if m.CodexVersion != o.CodexVersion {
		t.Fatalf("manifest mislabels native runtime: %q", m.CodexVersion)
	}
	if o.CodexVersion != "0.165.0" {
		return
	}

	t.Run("forged-version", func(t *testing.T) {
		filename := rewriteBundle(t, m, func(_ string, manifest *Manifest) { manifest.CodexVersion = "0.163.0" })
		home, project := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "project")
		if _, err := Restore(t.Context(), RestoreOptions{ArchivePath: filename, CodexHome: home, Destination: project, CodexVersion: "0.163.0"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("0.165 schema accepted under 0.163 label: %v", err)
		}
		assertRestoreDidNotPublish(t, home, project)
	})
	t.Run("forged-attached-parent", func(t *testing.T) {
		filename := rewriteBundle(t, m, func(stage string, _ *Manifest) {
			db, err := openDB(filepath.Join(stage, "native", "db", "state_5.sqlite"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec("UPDATE threads SET attached_thread_parent_id=? WHERE id=?", uuid.NewString(), o.ThreadID); err != nil {
				t.Fatal(err)
			}
		})
		home, project := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "project")
		if _, err := Restore(t.Context(), RestoreOptions{ArchivePath: filename, CodexHome: home, Destination: project, CodexVersion: o.CodexVersion}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("attached archive restored: %v", err)
		}
		assertRestoreDidNotPublish(t, home, project)
	})
	t.Run("attached-target-child", func(t *testing.T) {
		target, targetRoot := nativeFixture(t, "legacy")
		db, err := openDB(filepath.Join(target.SQLiteHome, "state_5.sqlite"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE threads SET attached_thread_parent_id=? WHERE id=?", o.ThreadID, target.ThreadID); err != nil {
			t.Fatal(err)
		}
		db.Close()
		before, err := os.ReadFile(nativeFixtureRollout(t, target.CodexHome, target.ThreadID))
		if err != nil {
			t.Fatal(err)
		}
		project := filepath.Join(targetRoot, "unpublished-project")
		if _, err := Restore(t.Context(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: target.CodexHome, Destination: project, CodexVersion: o.CodexVersion}); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "attached") {
			t.Fatalf("target attached relationship accepted: %v", err)
		}
		all, err := scanRollouts(t.Context(), target.CodexHome)
		if err != nil || len(all) != 1 || all[o.ThreadID] != nil {
			t.Fatalf("native files published into attached target: %#v %v", all, err)
		}
		after, err := os.ReadFile(nativeFixtureRollout(t, target.CodexHome, target.ThreadID))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("existing target rollout changed: %v", err)
		}
		if _, err := os.Stat(project); !os.IsNotExist(err) {
			t.Fatalf("project published into attached target: %v", err)
		}
	})
	t.Run("attached-source-preserved", func(t *testing.T) {
		db, err := openDB(filepath.Join(o.SQLiteHome, "state_5.sqlite"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE threads SET attached_thread_parent_id=? WHERE id=?", uuid.NewString(), o.ThreadID); err != nil {
			t.Fatal(err)
		}
		db.Close()
		attached := o
		attached.ArchiveID = uuid.NewString() + ".tar.xz"
		if _, err := Export(t.Context(), attached); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("attached source exported: %v", err)
		}
		if _, err := os.Stat(filepath.Join(o.ArchiveDir, attached.ArchiveID)); !os.IsNotExist(err) {
			t.Fatalf("unsupported archive published: %v", err)
		}
		if content, err := os.ReadFile(filepath.Join(o.ProjectPath, "keep.txt")); err != nil || string(content) != "project payload\n" {
			t.Fatalf("source project changed: %q %v", content, err)
		}
		if _, err := os.Stat(m.ArchivePath); err != nil {
			t.Fatalf("prior archive lost: %v", err)
		}
	})
}

func TestNativeSelfContainedOlderSessionMetadataOnNewRuntime(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_ARCHIVE_TEST") != "1" || os.Getenv("CODEX_NATIVE_ARCHIVE_VERSION") != "0.165.0" {
		t.Skip("requires exact native 0.165 proof")
	}
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			o, _ := nativeFixture(t, mode)
			filename := nativeFixtureRollout(t, o.CodexHome, o.ThreadID)
			data, err := os.ReadFile(filename)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.SplitN(data, []byte{'\n'}, 2)
			var metadata map[string]any
			if err := json.Unmarshal(lines[0], &metadata); err != nil {
				t.Fatal(err)
			}
			metadata["payload"].(map[string]any)["cli_version"] = "0.163.0"
			first, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, append(append(first, '\n'), lines[1]...), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := Export(t.Context(), o)
			if err != nil || m.CodexVersion != "0.165.0" {
				t.Fatalf("historical metadata changed current snapshot version: %#v %v", m, err)
			}
			if err := os.RemoveAll(o.CodexHome); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(o.ProjectPath); err != nil {
				t.Fatal(err)
			}
			if proof, err := Verify(t.Context(), m.ArchivePath, VerifyOptions{CodexBinary: os.Getenv("CODEX_REAL_BINARY")}); err != nil || !proof.VerifiedSelfContained {
				t.Fatalf("older-origin session failed native0.165 restore/restart: %v", err)
			}
		})
	}
}
