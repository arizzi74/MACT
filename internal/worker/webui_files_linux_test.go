package worker

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIFileOpenSearchOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions")
	}
	for _, restricted := range []string{"ancestor", "file-parent"} {
		t.Run(restricted, func(t *testing.T) {
			allowed := canonicalWebUIFileTempDir(t)
			ancestor := filepath.Join(allowed, "ancestor")
			cwd := filepath.Join(ancestor, "project")
			parent := filepath.Join(cwd, "results")
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "report.txt")
			content := []byte("assessment\x00\xff\n")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			directory := ancestor
			if restricted == "file-parent" {
				directory = parent
			}
			if err := os.Chmod(directory, 0o100); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(directory, 0o700); err != nil {
					t.Error(err)
				}
			})
			if opened, err := os.Open(directory); err == nil {
				opened.Close()
				t.Fatal("test directory unexpectedly permits reading")
			} else if !os.IsPermission(err) {
				t.Fatalf("reading test directory: %v", err)
			}
			// A known file remains readable without permission to list its
			// ancestor directories, and workspace validation already allows it.
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, content) {
				t.Fatalf("ordinary file read = %x, %v", got, err)
			}
			if canonical, err := auth.CanonicalWorkspace(cwd, []string{allowed}); err != nil || canonical != cwd {
				t.Fatalf("canonical workspace = %q, %v", canonical, err)
			}
			file, info, err := openWebUIFile(cwd, allowed, "results/report.txt", []string{allowed}, nil)
			if err != nil {
				t.Fatalf("download through search-only %s: %v", restricted, err)
			}
			defer file.Close()
			got, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(got, content) || info.Path != path || info.Size != int64(len(content)) {
				t.Fatalf("download = %#v, %x, %v", info, got, err)
			}
		})
	}
}

func TestInboundFilePublishThroughSearchOnlyAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions")
	}
	allowed := canonicalWebUIFileTempDir(t)
	ancestor := filepath.Join(allowed, "ancestor")
	cwd := filepath.Join(ancestor, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ancestor, 0o700); err != nil {
			t.Error(err)
		}
	})
	input := protocol.File{Name: "report.txt", Data: []byte("uploaded assessment\x00\xff\n")}
	var recorded *inboundFileRecord
	published := false
	// Creating a new inbound directory exercises workspace.Sync(), which
	// needs a readable directory handle even when ancestors only allow search.
	path, prompt, err := publishInboundFile(input, uuid.NewString(), cwd, []string{allowed}, nil, "read this assessment",
		func(record *inboundFileRecord) error { recorded = record; return nil },
		func() error { published = true; return nil })
	if err != nil {
		t.Fatalf("publish through search-only ancestor: %v", err)
	}
	if recorded == nil || !published || prompt != "read this assessment" || filepath.Dir(path) != filepath.Join(cwd, "inbound") {
		t.Fatalf("published upload = %q, %q, recorded=%v, published=%v", path, prompt, recorded != nil, published)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, input.Data) {
		t.Fatalf("published file = %x, %v", got, err)
	}
}

func TestWebUIFileOpenRejectsSymlinkReplacementUnderSearchOnlyAncestor(t *testing.T) {
	for _, replacement := range []string{"workspace", "file-parent"} {
		t.Run(replacement, func(t *testing.T) {
			allowed, outside := canonicalWebUIFileTempDir(t), canonicalWebUIFileTempDir(t)
			ancestor := filepath.Join(allowed, "ancestor")
			cwd := filepath.Join(ancestor, "project")
			parent := filepath.Join(cwd, "results")
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "report.txt"), []byte("outside-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			directory, target := cwd, "report.txt"
			if replacement == "file-parent" {
				directory, target = parent, "results/report.txt"
			}
			if err := os.Rename(directory, directory+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, directory); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ancestor, 0o100); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(ancestor, 0o700); err != nil {
					t.Error(err)
				}
			})
			// The known pathname resolves and is readable, but downloads must
			// reject the swapped directory rather than follow its symbolic link.
			if got, err := os.ReadFile(filepath.Join(cwd, target)); err != nil || string(got) != "outside-secret" {
				t.Fatalf("symlink replacement fixture = %q, %v", got, err)
			}
			if file, _, err := openWebUIFile(cwd, allowed, target, []string{allowed}, nil); err == nil {
				file.Close()
				t.Fatalf("%s symbolic link exposed an outside file", replacement)
			}
		})
	}
}
