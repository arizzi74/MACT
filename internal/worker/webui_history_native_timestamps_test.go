package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestWebUIHistoryProjectsNativeSavedMessageTimes(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	client := mustClient(t, a, runtime)
	useWebUIHistoryFixture(a, client)
	session := installSession(a, runtime, "native-recovery-history", "")
	const promptMS = int64(1793195306672)
	const toolStartMS = int64(1793195339818)
	const toolCompleteMS = int64(1793195550385)
	const finalMS = int64(1793195881883)
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "turns": []map[string]any{{"id": "original-turn", "status": "completed", "startedAt": 1793195301, "completedAt": 1793195881, "items": []map[string]any{
		{"id": "original-prompt", "type": "userMessage", "recordedAtMs": promptMS, "content": []map[string]any{{"type": "text", "text": "Assess code"}}},
		{"id": "tool-review", "type": "dynamicToolCall", "namespace": "claude", "tool": "Agent", "status": "completed", "recordedAtMs": toolStartMS, "startedAtMs": toolStartMS, "completedAtMs": toolCompleteMS, "output": "Review finished"},
		{"id": "native-final-1", "type": "agentMessage", "phase": "final_answer", "recordedAtMs": finalMS, "text": "Full assessment"},
	}}}}}, []string{session.ThreadID})
	raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	if len(page.Data) != 3 {
		t.Fatalf("page=%s", raw)
	}
	for i, want := range []int64{promptMS, toolStartMS, finalMS} {
		if page.Data[i].RecordedAtMS == nil || *page.Data[i].RecordedAtMS != want {
			t.Fatalf("entry %d timestamp=%#v", i, page.Data[i])
		}
	}
	if page.Data[1].StartedAtMS == nil || *page.Data[1].StartedAtMS != toolStartMS || page.Data[1].CompletedAtMS == nil || *page.Data[1].CompletedAtMS != toolCompleteMS {
		t.Fatalf("tool lifecycle=%#v", page.Data[1])
	}
	if page.Data[2].CompletedAtMS != nil {
		t.Fatal("message record date was fabricated into a lifecycle completion")
	}
}

func TestNativeHistoryRecoveryInvalidatesOnlyItsCachedThread(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	for _, thread := range []string{"repaired", "other"} {
		_, err := a.webHistory.insert(webUIHistoryScope{Runtime: runtime.ID, Thread: thread, Generation: runtime.Generation}, codexadapter.TranscriptTurn{ID: "turn", Status: "completed", Items: []json.RawMessage{json.RawMessage(`{"id":"answer","type":"agentMessage","text":"old"}`)}}, []rolloutTimestampQuery{{}})
		if err != nil {
			t.Fatal(err)
		}
	}
	a.onEvent(runtime, codexadapter.Event{Method: "thread/history/recovered", ThreadID: "repaired"})
	if got := a.webHistory.completed(webUIHistoryScope{Runtime: runtime.ID, Thread: "repaired", Generation: runtime.Generation}, "turn"); got != nil {
		t.Fatal("repaired thread retained stale history")
	}
	if got := a.webHistory.completed(webUIHistoryScope{Runtime: runtime.ID, Thread: "other", Generation: runtime.Generation}, "turn"); got == nil {
		t.Fatal("unrelated thread history was invalidated")
	}
}

func TestNativeHistoryRecoveryDoesNotCreateExecutionActivity(t *testing.T) {
	var activity nativeActivity
	activity.clientMessage([]byte(`{"id":1,"method":"thread/history/recover","params":{"threadId":"thread","turnId":"turn"}}`))
	activity.serverMessage([]byte(`{"method":"thread/history/recovered","params":{"threadId":"thread","turnId":"turn","changed":true}}`))
	activity.serverMessage([]byte(`{"id":1,"result":{"threadId":"thread","turnId":"turn","changed":true}}`))
	if activity.uncertain || activity.settledError() != nil {
		t.Fatalf("history maintenance poisoned activity: %#v", activity)
	}
}

func TestWebUIHistoryLargeNativeItemRetainsVerifiedTiming(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"id": "native-large", "type": "agentMessage", "text": strings.Repeat("report", webUIHistoryItemBytes), "recordedAtMs": 1793195881883, "startedAtMs": "invalid", "completedAtMs": -1})
	items, err := webUISanitizeHistoryItems(nil, []json.RawMessage{raw})
	if err != nil {
		t.Fatal(err)
	}
	entry := webUIHistoryEntry{Item: items[0]}
	webUIHistoryNativeItemTiming(&entry)
	if entry.RecordedAtMS == nil || *entry.RecordedAtMS != 1793195881883 || entry.StartedAtMS != nil || entry.CompletedAtMS != nil {
		t.Fatalf("timing=%#v", entry)
	}
	if len(entry.Item) > webUIHistoryItemBytes || !strings.Contains(string(entry.Item), "displayNotice") {
		t.Fatal("large output did not retain bounded projection")
	}
}
