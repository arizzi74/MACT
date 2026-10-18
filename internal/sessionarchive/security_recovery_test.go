package sessionarchive

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func rewriteBundle(t *testing.T, m Manifest, change func(string, *Manifest)) string {
	t.Helper()
	ctx := context.Background()
	stage := t.TempDir()
	got, err := unpack(ctx, m.ArchivePath, stage)
	if err != nil {
		t.Fatal(err)
	}
	change(stage, &got)
	if err := inventoryStage(ctx, stage, &got); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), m.ArchiveID)
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack(ctx, f, stage); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	return filename
}

func TestRejectsNativeConfigInjectionAndDotManifestID(t *testing.T) {
	o, _ := fixture(t, "legacy")
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"auth.json", "config.toml", "hooks/run.sh", "plugins/evil.json"} {
		t.Run(bad, func(t *testing.T) {
			filename := rewriteBundle(t, m, func(stage string, _ *Manifest) {
				target := filepath.Join(stage, "native", "home", bad)
				os.MkdirAll(filepath.Dir(target), 0700)
				os.WriteFile(target, []byte("must not import"), 0600)
			})
			if _, err := Read(context.Background(), filename); err == nil {
				t.Fatal("native config injection accepted")
			}
			if _, err := ReadMetadata(context.Background(), filename); err == nil {
				t.Fatal("unsafe metadata offered for listing")
			}
		})
	}
	filename := rewriteBundle(t, m, func(_ string, m *Manifest) { m.ArchiveID = ".." })
	if _, err := Read(context.Background(), filename); err == nil {
		t.Fatal("dot-dot archive ID accepted")
	}
	link := filepath.Join(t.TempDir(), m.ArchiveID)
	if err := os.Symlink(m.ArchivePath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(context.Background(), link); err == nil {
		t.Fatal("archive symlink followed")
	}
}

func TestRejectsAttachmentCredentialTraversal(t *testing.T) {
	o, _ := fixture(t, "legacy")
	if err := os.MkdirAll(filepath.Join(o.CodexHome, "attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.CodexHome, "auth.json"), []byte("PRIVATE_AUTH_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(o.CodexHome, filepath.Join(o.CodexHome, "attachments", "link")); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{o.CodexHome + "/attachments/../auth.json", filepath.Join(o.CodexHome, "attachments", "link", "auth.json")} {
		data, _ := json.Marshal(map[string]any{"file_path": source})
		m := Manifest{}
		stage := t.TempDir()
		if err := collectAttachments(context.Background(), data, o, stage, &m); err == nil {
			t.Fatal("credential attachment traversal accepted")
		}
		if len(m.Files) != 0 {
			t.Fatal("credential payload copied before rejection")
		}
	}
}

func TestRejectsNativeSchemaAndUnrelatedScope(t *testing.T) {
	o, _ := fixture(t, "legacy")
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"schema", "extra-thread", "extra-rollout"} {
		t.Run(kind, func(t *testing.T) {
			filename := rewriteBundle(t, m, func(stage string, m *Manifest) {
				switch kind {
				case "schema":
					dir := filepath.Join(stage, "native", "db")
					os.MkdirAll(dir, 0700)
					db, err := openDB(filepath.Join(dir, "state_5.sqlite"), false)
					if err != nil {
						t.Fatal(err)
					}
					db.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT)")
					db.Exec("CREATE TRIGGER surprise AFTER INSERT ON threads BEGIN DELETE FROM threads; END")
					db.Close()
				case "extra-thread":
					id := uuid.NewString()
					m.ThreadIDs = append(m.ThreadIDs, id)
					writeFixtureRollout(t, filepath.Join(stage, "native", "home"), m.OriginalPath, id, "legacy", nil)
				case "extra-rollout":
					writeFixtureRollout(t, filepath.Join(stage, "native", "home"), m.OriginalPath, uuid.NewString(), "legacy", nil)
				}
			})
			home := filepath.Join(t.TempDir(), "home")
			if _, err := Restore(context.Background(), RestoreOptions{ArchivePath: filename, CodexHome: home, Destination: filepath.Join(t.TempDir(), "project"), CodexVersion: o.CodexVersion}); err == nil {
				t.Fatal("untrusted native schema/scope imported")
			}
			entries, _ := os.ReadDir(home)
			if len(entries) != 0 {
				t.Fatal("native live state changed before validation")
			}
		})
	}
}

func TestRejectsOversizedXZDictionaryBeforeDecoding(t *testing.T) {
	o, _ := fixture(t, "legacy")
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	size := (int(data[12]) + 1) * 4
	if data[13] != 0 || data[14] != 0x21 || data[15] != 1 {
		t.Fatal("unexpected test XZ block header")
	}
	data[16] = 40
	binary.LittleEndian.PutUint32(data[12+size-4:12+size], crc32.ChecksumIEEE(data[12:12+size-4]))
	filename := filepath.Join(t.TempDir(), m.ArchiveID)
	os.WriteFile(filename, data, 0600)
	if _, err := Read(context.Background(), filename); err == nil || !strings.Contains(err.Error(), "dictionary") {
		t.Fatalf("oversized dictionary was not rejected by framing guard: %v", err)
	}
}

func appendLocalAttachment(t *testing.T, o ExportOptions, source string) {
	t.Helper()
	filename := filepath.Join(o.CodexHome, "sessions", "2026", "10", "18", "rollout-2026-10-18T12-00-00-"+o.ThreadID+".jsonl")
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(map[string]any{"timestamp": "2026-10-18T12:00:00Z", "type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "image fixture", "local_images": []string{source}}})
}

func archiveFixtureNative(t *testing.T, o ExportOptions) {
	t.Helper()
	all, err := scanRollouts(context.Background(), o.CodexHome)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(o.CodexHome, "archived_sessions")
	os.MkdirAll(dir, 0700)
	for _, r := range all {
		if r.owner == o.ThreadID {
			if err := os.Rename(r.path, filepath.Join(dir, filepath.Base(r.path))); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRetainedNativeAttachmentRepairPresentMissingAndInterrupted(t *testing.T) {
	for _, condition := range []string{"present", "missing", "interrupted", "changed-preimage"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			o, root := fixture(t, "legacy")
			source := filepath.Join(root, "temporary-image.png")
			os.WriteFile(source, []byte("restorable attachment nonce"), 0600)
			appendLocalAttachment(t, o, source)
			m, err := Export(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			archiveFixtureNative(t, o)
			os.RemoveAll(o.ProjectPath)
			if condition != "present" {
				os.Remove(source)
			}
			restore := RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: o.CodexHome, SQLiteHome: o.SQLiteHome, Destination: o.ProjectPath, CodexVersion: o.CodexVersion}
			if condition == "interrupted" || condition == "changed-preimage" {
				stage := t.TempDir()
				unpack(ctx, m.ArchivePath, stage)
				before := t.TempDir()
				copyTree(ctx, filepath.Join(stage, "native"), filepath.Join(before, "native"))
				restore.repairNative = true
				restore.rolloutAliases, _ = repairAliases(ctx, m, before)
				if err := rebindNative(ctx, restore, m, stage); err != nil {
					t.Fatal(err)
				}
				if err := beginImportIntent(ctx, restore, m); err != nil {
					t.Fatal(err)
				}
				old, _ := scanRollouts(ctx, filepath.Join(before, "native", "home"))
				desired, _ := scanRollouts(ctx, filepath.Join(stage, "native", "home"))
				live, _ := scanRollouts(ctx, o.CodexHome)
				if condition == "interrupted" {
					if err := replaceNativeRollout(ctx, old[o.ThreadID], desired[o.ThreadID], live[o.ThreadID]); err != nil {
						t.Fatal(err)
					}
				} else {
					f, _ := os.OpenFile(live[o.ThreadID].path, os.O_WRONLY|os.O_APPEND, 0600)
					f.WriteString("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[]}}\n")
					f.Close()
					if _, err := Restore(ctx, restore); !errors.Is(err, ErrConflict) {
						t.Fatalf("changed repair preimage overwritten: %v", err)
					}
					return
				}
			}
			if _, err := Restore(ctx, restore); err != nil {
				t.Fatal(err)
			}
			live, err := scanRollouts(ctx, o.CodexHome)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(live[o.ThreadID].path)
			if strings.Contains(string(data), source) || !strings.Contains(string(data), "archive_attachments") {
				t.Fatal("retained native attachment was not rebound")
			}
			if _, err := Restore(ctx, restore); err != nil {
				t.Fatalf("repaired restore replay: %v", err)
			}
			found := false
			filepath.WalkDir(filepath.Join(o.CodexHome, "archive_attachments"), func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					data, _ := os.ReadFile(path)
					found = string(data) == "restorable attachment nonce"
				}
				return err
			})
			if !found {
				t.Fatal("archived attachment bytes not recovered")
			}
			if condition == "missing" {
				if _, err := os.Stat(source); !os.IsNotExist(err) {
					t.Fatal("restore wrote to original external attachment location")
				}
			}
		})
	}
}

func TestInterruptedImportReconcilesIdenticalFilesAndRejectsChangedFiles(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "identical", true: "changed"}[changed], func(t *testing.T) {
			ctx := context.Background()
			o, root := fixture(t, "legacy")
			m, err := Export(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			unpack(ctx, m.ArchivePath, stage)
			restore := RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: filepath.Join(root, "target-home"), SQLiteHome: filepath.Join(root, "target-home"), Destination: filepath.Join(root, "target-project"), CodexVersion: o.CodexVersion}
			os.MkdirAll(restore.CodexHome, 0700)
			if err := rebindNative(ctx, restore, m, stage); err != nil {
				t.Fatal(err)
			}
			if err := beginImportIntent(ctx, restore, m); err != nil {
				t.Fatal(err)
			}
			all, _ := scanRollouts(ctx, filepath.Join(stage, "native", "home"))
			r := all[o.ThreadID]
			target := filepath.Join(restore.CodexHome, r.relative)
			if _, err := publishNativeFile(ctx, r.path, target); err != nil {
				t.Fatal(err)
			}
			if changed {
				f, _ := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0600)
				f.WriteString("changed\n")
				f.Close()
			}
			_, err = Restore(ctx, restore)
			if changed {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("changed interrupted import accepted: %v", err)
				}
				data, _ := os.ReadFile(target)
				if !strings.HasSuffix(string(data), "changed\n") {
					t.Fatal("changed native data overwritten")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFailedCopyRemovesPartialTarget(t *testing.T) {
	o, _ := fixture(t, "legacy")
	source := filepath.Join(o.ProjectPath, "keep.txt")
	info, _ := os.Stat(source)
	target := filepath.Join(t.TempDir(), "copy")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(ctx, source, target, info); err == nil {
		t.Fatal("cancelled copy unexpectedly succeeded")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("partial copy left a target behind")
	}
}

func TestRejectsInvalidManifestMetadataBeforeLiveImport(t *testing.T) {
	o, _ := fixture(t, "legacy")
	m, err := Export(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*Manifest){"zero-date": func(m *Manifest) { m.CreatedAt = time.Time{} }, "long-title": func(m *Manifest) { m.DisplayName = strings.Repeat("x", 1025) }, "long-id": func(m *Manifest) { m.ArchiveID = strings.Repeat("x", 256) + ".tar.xz" }, "hidden-id": func(m *Manifest) { m.ArchiveID = ".hidden.tar.xz" }, "control-path": func(m *Manifest) { m.OriginalPath = "/project\nother" }, "unclean-home": func(m *Manifest) { m.OriginalCodexHome = "/codex/../other" }, "long-version": func(m *Manifest) { m.CodexVersion = strings.Repeat("x", 129) }}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			filename := rewriteBundle(t, m, func(_ string, m *Manifest) { change(m) })
			if _, err := ReadMetadata(context.Background(), filename); err == nil {
				t.Fatal("invalid metadata accepted for inventory")
			}
			home := filepath.Join(t.TempDir(), "codex")
			if _, err := Restore(context.Background(), RestoreOptions{ArchivePath: filename, CodexHome: home, Destination: filepath.Join(t.TempDir(), "project"), CodexVersion: o.CodexVersion}); err == nil {
				t.Fatal("invalid metadata restored")
			}
			files, _ := os.ReadDir(home)
			if len(files) != 0 {
				t.Fatal("invalid metadata changed live native state")
			}
		})
	}
}

func TestInterruptedRepairNewAttachmentRowPreservesNativePreimage(t *testing.T) {
	ctx := context.Background()
	o, root := fixture(t, "legacy")
	source := filepath.Join(root, "image.png")
	os.WriteFile(source, []byte("saved-image"), 0600)
	appendLocalAttachment(t, o, source)
	m, err := Export(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	archiveFixtureNative(t, o)
	after := t.TempDir()
	unpack(ctx, m.ArchivePath, after)
	before := t.TempDir()
	copyTree(ctx, filepath.Join(after, "native"), filepath.Join(before, "native"))
	restore := RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: o.CodexHome, SQLiteHome: o.SQLiteHome, Destination: o.ProjectPath, CodexVersion: o.CodexVersion, repairNative: true}
	if err := rebindNative(ctx, restore, m, after); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(before, "native", "db"), filepath.Join(after, "native", "db"), o.SQLiteHome} {
		os.MkdirAll(dir, 0700)
		db, err := openDB(filepath.Join(dir, "state_5.sqlite"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE thread_attachments(id TEXT PRIMARY KEY,thread_id TEXT,payload TEXT)"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if dir == o.SQLiteHome {
			if _, err := db.Exec("INSERT INTO thread_attachments VALUES(?,?,?)", "new-user-attachment", o.ThreadID, "newer attachment data"); err != nil {
				db.Close()
				t.Fatal(err)
			}
		}
		db.Close()
	}
	live, _ := scanRollouts(ctx, o.CodexHome)
	hash, err := Digest(ctx, live[o.ThreadID].path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repairRetainedNative(ctx, restore, m, before, after); !errors.Is(err, ErrConflict) {
		t.Fatalf("new selected native row accepted during repair: %v", err)
	}
	current, _ := Digest(ctx, live[o.ThreadID].path)
	if current != hash {
		t.Fatal("native preimage changed before row-set conflict")
	}
	if _, err := os.Stat(filepath.Join(o.CodexHome, "archive_attachments")); !os.IsNotExist(err) {
		t.Fatal("managed assets published before preflight completed")
	}
}
