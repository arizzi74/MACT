package protocol

import (
	"strings"
	"testing"
)

func TestSessionTitleIsIndependentOfProjectDirectory(t *testing.T) {
	for _, name := range []string{"Project / review", "Assessment: follow-up", strings.Repeat("è", 120)} {
		if _, err := NormalizeSessionTitle(name); err != nil {
			t.Fatalf("valid title %q: %v", name, err)
		}
	}
	for _, name := range []string{" ", "line\nbreak", string([]byte{0xff}), strings.Repeat("è", 121)} {
		if _, err := NormalizeSessionTitle(name); err == nil {
			t.Fatalf("invalid title accepted: %q", name)
		}
	}
}
func TestArchiveIdentityCannotSelectArbitraryPath(t *testing.T) {
	for _, id := range []string{"../project.tar.xz", "/tmp/project.tar.xz", "dir/project.tar.xz", "dir\\project.tar.xz", ".hidden.tar.xz", "project.zip", ".tar.xz", "project\n.tar.xz"} {
		if ValidSessionArchiveID(id) {
			t.Fatalf("invalid archive ID accepted: %q", id)
		}
	}
	for _, id := range []string{"Project_20261018.tar.xz", "Progetto_è.tar.xz"} {
		if !ValidSessionArchiveID(id) {
			t.Fatalf("valid archive ID rejected: %q", id)
		}
	}
}

func TestArchiveCleanupWarningRequiresSafeRetainedPath(t *testing.T) {
	for _, path := range []string{"relative", "/project\nother", "/" + strings.Repeat("a", 4096)} {
		result := SessionArchiveResult{WarningCode: "project_cleanup_failed", RetainedProjectPath: path}
		if result.Validate() == nil {
			t.Fatalf("unsafe retained path accepted: %q", path)
		}
	}
	result := SessionArchiveResult{RetainedProjectPath: "/work/.codex-archived-123"}
	if result.Validate() == nil {
		t.Fatal("retained path without cleanup warning accepted")
	}
	result.WarningCode = "project_cleanup_failed"
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
}
