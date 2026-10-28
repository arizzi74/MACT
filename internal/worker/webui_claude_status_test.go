package worker

import (
	"strings"
	"testing"
)

func TestWebUIClaudeStatusIsScopedAndRequiresResume(t *testing.T) {
	r := testWebUIRelay()
	request := []byte(`{"id":1,"method":"thread/status/read","params":{}}`)
	if _, err := r.clientMessage(request); err == nil {
		t.Fatal("status allowed before validated resume")
	}
	r.resumed = true
	for _, params := range []string{`{"threadId":"other"}`, `{"credential":true}`, `{"includeAuth":true}`} {
		if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/status/read","params":` + params + `}`)); err == nil {
			t.Fatalf("unsafe status parameters accepted: %s", params)
		}
	}
	data, err := r.clientMessage(request)
	if err != nil || !strings.Contains(string(data), `"threadId":"thread"`) {
		t.Fatalf("status scope = %s, %v", data, err)
	}
	update := []byte(`{"method":"thread/claudeStatus/updated","params":{"threadId":"thread","claudeStatus":{"provider":"claude","cost":{"sessionUSD":0}}}}`)
	if data, err := r.serverMessage(update); err != nil || len(data) == 0 {
		t.Fatalf("selected session status dropped: %s, %v", data, err)
	}
	for _, raw := range []string{
		strings.ReplaceAll(string(update), `"threadId":"thread"`, `"threadId":"other"`),
		strings.ReplaceAll(string(update), `"threadId":"thread",`, ""),
	} {
		if data, err := r.serverMessage([]byte(raw)); err != nil || len(data) != 0 {
			t.Fatalf("unscoped status exposed: %s, %v", data, err)
		}
	}
}
