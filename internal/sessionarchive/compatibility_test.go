package sessionarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveCompatibilityUsesExactValidatedVersions(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0", "codex-cli 0.165.0"} {
		if err := CheckCompatibility(version); err != nil {
			t.Fatalf("validated version %q: %v", version, err)
		}
	}
	for _, version := range []string{"", "0.162.0", "0.164.0", "0.165.1", "0.166.0", "0.165.0-dev"} {
		if err := CheckCompatibility(version); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unvalidated version %q accepted: %v", version, err)
		}
	}
	for _, versions := range [][2]string{{"0.163.0", "0.163.0"}, {"codex-cli 0.165.0", "0.165.0"}} {
		if err := CheckVersionCompatibility(versions[0], versions[1]); err != nil {
			t.Fatalf("matching validated versions rejected: %v", err)
		}
	}
	for _, versions := range [][2]string{{"0.163.0", "0.165.0"}, {"0.165.0", "0.163.0"}, {"0.164.0", "0.165.0"}, {"0.165.0", "0.166.0"}} {
		if err := CheckVersionCompatibility(versions[0], versions[1]); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unproven version pair accepted: %v %v", versions, err)
		}
	}
}

func TestRestoreVersionMismatchPreservesDestination(t *testing.T) {
	for _, source := range []string{"0.163.0", "0.165.0"} {
		t.Run(source, func(t *testing.T) {
			o, root := fixture(t, "legacy")
			o.CodexVersion = source
			m, err := Export(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			target := "0.163.0"
			if source == target {
				target = "0.165.0"
			}
			home, project := filepath.Join(root, "target-home"), filepath.Join(root, "target-project")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "keep.txt"), []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = Restore(t.Context(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: home, Destination: project, CodexVersion: target})
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), target) {
				t.Fatalf("cross-version restore did not fail precisely: %v", err)
			}
			assertRestoreDidNotPublish(t, home, project)
			if got, err := os.ReadFile(filepath.Join(home, "keep.txt")); err != nil || string(got) != "unchanged" {
				t.Fatalf("existing home changed: %q %v", got, err)
			}
		})
	}
}

func TestRestoreRejectsUnvalidatedTargetSchemaBeforePublishing(t *testing.T) {
	o, root := fixture(t, "legacy")
	m, err := Export(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	home, project := filepath.Join(root, "target-home"), filepath.Join(root, "target-project")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(home, "state_5.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE existing_data(value TEXT); INSERT INTO existing_data VALUES ('preserve'); CREATE TRIGGER unexpected AFTER INSERT ON existing_data BEGIN DELETE FROM existing_data; END"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Restore(context.Background(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: home, Destination: project, CodexVersion: o.CodexVersion})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "target native schema") {
		t.Fatalf("unknown target schema accepted: %v", err)
	}
	assertRestoreDidNotPublish(t, home, project)
	db, err = openDB(filepath.Join(home, "state_5.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT value FROM existing_data").Scan(&value); err != nil || value != "preserve" {
		t.Fatalf("target data changed: %q %v", value, err)
	}
}

func assertRestoreDidNotPublish(t *testing.T, home, project string) {
	t.Helper()
	if _, err := os.Lstat(project); !os.IsNotExist(err) {
		t.Fatalf("project published before validation: %v", err)
	}
	for _, name := range []string{"sessions", "archived_sessions", "archive_attachments", ".archive-imports"} {
		if _, err := os.Lstat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("native %s published before validation: %v", name, err)
		}
	}
}
