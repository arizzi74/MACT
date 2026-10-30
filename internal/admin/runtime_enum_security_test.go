package admin

import (
	"regexp"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestClaudeAdminPermissionChoicesMatchValidatedSubset(t *testing.T) {
	page, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	selectHTML := regexp.MustCompile(`<select id="worker-runtime-permissions">(.*?)</select>`).FindSubmatch(page)
	if len(selectHTML) != 2 {
		t.Fatal("missing permission selector")
	}
	options := regexp.MustCompile(`<option value="([^"]+)">`).FindAllSubmatch(selectHTML[1], -1)
	want := map[string]bool{"default": true, "acceptEdits": true, "plan": true, "dontAsk": true, "auto": true, "bypassPermissions": true}
	for _, option := range options {
		mode := string(option[1])
		if !want[mode] {
			t.Fatal("unexpected permission choice")
		}
		delete(want, mode)
		if err := (&protocol.WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: mode}).Validate(); err != nil {
			t.Fatal("UI offered rejected permission")
		}
	}
	if len(want) != 0 || strings.Contains(string(selectHTML[1]), `selected`) {
		t.Fatal("missing choices or unexpected bypass default")
	}
	for _, mode := range []string{"read-only", "full-access", "manual", "BYPASSPERMISSIONS", "invented"} {
		if err := (&protocol.WorkerRuntimeSettingsUpdate{AuthMode: "login", PermissionMode: mode}).Validate(); err == nil {
			t.Fatal("unreviewed remote permission mode accepted")
		}
	}
}
