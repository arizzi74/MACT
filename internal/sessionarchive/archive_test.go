package sessionarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ulikunitz/xz"
)

func fixture(t *testing.T, mode string) (ExportOptions, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "source-codex")
	project := filepath.Join(root, "source-project")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "keep.txt"), []byte("project payload\n"), 0640); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	writeFixtureRollout(t, home, project, id, mode, nil)
	return ExportOptions{CodexHome: home, SQLiteHome: home, ThreadID: id, ProjectPath: project, ArchiveDir: filepath.Join(root, "archives"), ArchiveID: uuid.NewString() + ".tar.xz", DisplayName: "Archive test", CodexVersion: "0.163.0"}, root
}

func fixtureRecords(project, id, mode string, base any, versions ...string) []map[string]any {
	version := "0.163.0"
	if len(versions) > 0 {
		version = versions[0]
	}
	meta := map[string]any{"id": id, "session_id": id, "timestamp": "2026-10-18T12:00:00Z", "cwd": project, "originator": "codex_cli_rs", "cli_version": version, "source": "cli", "model_provider": "archive_verify", "history_mode": mode, "base_instructions": map[string]any{"text": "Preserve the native archive fixture exactly.", "version": 1}}
	if base != nil {
		meta["history_base"] = base
	}
	records := []map[string]any{{"type": "session_meta", "payload": meta}, {"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "turn-fixture", "model_context_window": 128000, "started_at": 1792324800}}, {"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Native archive model context sentinel."}}}}, {"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Native reply sentinel."}}, "phase": "final_answer"}}, {"type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-fixture", "last_agent_message": "Native reply sentinel.", "started_at": 1792324800, "completed_at": 1792324801}}}
	if mode == "paginated" {
		records = append(records[:3], append([]map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": id, "turn_id": "turn-fixture", "item": map[string]any{"type": "UserMessage", "id": "user-fixture", "content": []any{map[string]any{"type": "text", "text": "Native archive model context sentinel."}}}, "completed_at_ms": 1792324800000}}}, records[3:]...)...)
	}
	for i := range records {
		records[i]["timestamp"] = "2026-10-18T12:00:00Z"
		if mode == "paginated" {
			records[i]["ordinal"] = i
		}
	}
	return records
}

func writeFixtureRollout(t *testing.T, home, project, id, mode string, base any, versions ...string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "18")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "rollout-2026-10-18T12-00-00-"+id+".jsonl")
	var b bytes.Buffer
	for _, record := range fixtureRecords(project, id, mode, base, versions...) {
		if err := json.NewEncoder(&b).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filename, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestArchiveRestoresWithNativeSourceUnavailable(t *testing.T) {
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			o, root := fixture(t, mode)
			if err := os.Symlink("/usr/bin/python3", filepath.Join(o.ProjectPath, "python")); err != nil {
				t.Fatal(err)
			}
			m, err := Export(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if m.ArchiveID != o.ArchiveID || strings.HasSuffix(m.ArchivePath, ".tar.xz.tar.xz") {
				t.Fatalf("archive ID mismatch: %#v", m)
			}
			if err := os.RemoveAll(o.CodexHome); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(o.ProjectPath); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "restored-project")
			newHome := filepath.Join(root, "restored-codex")
			if _, err := Restore(context.Background(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: newHome, Destination: target, CodexVersion: o.CodexVersion}); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(target, "keep.txt")); err != nil || string(b) != "project payload\n" {
				t.Fatalf("restored project %q %v", b, err)
			}
			link, err := os.Readlink(filepath.Join(target, "python"))
			if err != nil || link != "/usr/bin/python3" {
				t.Fatalf("external symlink not preserved: %q %v", link, err)
			}
			all, err := scanRollouts(context.Background(), newHome)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 {
				t.Fatalf("native history missing: %#v", all)
			}
			r := all[o.ThreadID]
			rdata, _ := os.ReadFile(r.path)
			if !bytes.Contains(rdata, []byte("Native archive model context sentinel.")) || bytes.Contains(rdata, []byte(o.CodexHome)) || bytes.Contains(rdata, []byte(o.ProjectPath)) {
				t.Fatalf("native history not self-contained: %s", rdata)
			}
			if _, err := Restore(context.Background(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: newHome, Destination: target, CodexVersion: o.CodexVersion}); err != nil {
				t.Fatalf("identical restore replay: %v", err)
			}
			if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("new changes"), 0640); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(context.Background(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: newHome, Destination: target, CodexVersion: o.CodexVersion}); !errors.Is(err, ErrConflict) {
				t.Fatalf("modified project must conflict: %v", err)
			}
		})
	}
}

func TestNativeSnapshotsExcludeUnrelatedSessionsAndPreserveAttachment(t *testing.T) {
	o, root := fixture(t, "legacy")
	unrelated := uuid.NewString()
	writeFixtureRollout(t, o.CodexHome, o.ProjectPath, unrelated, "legacy", nil)
	attachment := filepath.Join(root, "user-image.png")
	if err := os.WriteFile(attachment, []byte("attachment sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(o.CodexHome, "sessions", "2026", "10", "18", "rollout-2026-10-18T12-00-00-"+o.ThreadID+".jsonl")
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "image", "local_images": []string{attachment}}}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(o.CodexHome); err != nil {
		t.Fatal(err)
	}
	os.Remove(attachment)
	newHome := filepath.Join(root, "new-home")
	if _, err := Restore(context.Background(), RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: newHome, Destination: filepath.Join(root, "restore"), CodexVersion: o.CodexVersion}); err != nil {
		t.Fatal(err)
	}
	all, err := scanRollouts(context.Background(), newHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[unrelated] != nil {
		t.Fatal("unrelated session bundled")
	}
	restoredData, _ := os.ReadFile(all[o.ThreadID].path)
	if bytes.Contains(restoredData, []byte(attachment)) {
		t.Fatal("attachment path not rebound")
	}
	found := false
	filepath.WalkDir(filepath.Join(newHome, "archive_attachments"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			found = string(b) == "attachment sentinel"
		}
		return err
	})
	if !found {
		t.Fatal("attachment content missing")
	}
}

func TestRejectsArchiveTraversalAndSymlinkDescendants(t *testing.T) {
	for _, headers := range [][]*tar.Header{{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0600}}, {{Name: "project/link/file", Typeflag: tar.TypeReg, Mode: 0600}, {Name: "project/link", Typeflag: tar.TypeSymlink, Linkname: "/tmp", Mode: 0777}}} {
		t.Run(headers[0].Name, func(t *testing.T) {
			var b bytes.Buffer
			x, _ := xz.NewWriter(&b)
			tr := tar.NewWriter(x)
			for _, h := range headers {
				tr.WriteHeader(h)
			}
			tr.Close()
			x.Close()
			filename := filepath.Join(t.TempDir(), "bad.tar.xz")
			os.WriteFile(filename, b.Bytes(), 0600)
			if _, err := Read(context.Background(), filename); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestArchiveDetectsCorruptedPayloadAndLeavesSource(t *testing.T) {
	o, _ := fixture(t, "legacy")
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(m.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(m.ArchivePath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), m.ArchivePath); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if _, err := os.Stat(filepath.Join(o.ProjectPath, "keep.txt")); err != nil {
		t.Fatal("source changed")
	}
}
