package codexadapter

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestNativeMessageTimestampsSurviveConversationAndPromptProjection(t *testing.T) {
	const promptMS = int64(1793195306672)
	const finalMS = int64(1793195881883)
	raw := json.RawMessage(fmt.Sprintf(`{"id":"original-turn","startedAt":1793195301,"completedAt":1793195881,"items":[
		{"id":"original-prompt","type":"userMessage","recordedAtMs":%d,"content":[{"type":"text","text":"Assess the code"}]},
		{"id":"early","type":"agentMessage","phase":"commentary","recordedAtMs":1793195377247,"text":"Waiting for agents"},
		{"id":"native-final-1","type":"agentMessage","phase":"final_answer","recordedAtMs":%d,"text":"Full assessment"}
	]}`, promptMS, finalMS))
	messages, err := decodeConversationTurn(raw)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages=%#v, err=%v", messages, err)
	}
	for i, want := range []int64{promptMS, finalMS} {
		if messages[i].Timestamp == nil || messages[i].Timestamp.UnixMilli() != want || messages[i].TimestampSource != "message" {
			t.Fatalf("message %d timing = %#v", i, messages[i])
		}
	}
	prompts, err := decodeUserPromptTurns([]json.RawMessage{raw})
	if err != nil || len(prompts) != 1 || prompts[0].Timestamp == nil || prompts[0].Timestamp.UnixMilli() != promptMS {
		t.Fatalf("prompts=%#v, err=%v", prompts, err)
	}
}

func TestMalformedNativeMessageDatesKeepTurnFallback(t *testing.T) {
	for _, value := range []string{`null`, `0`, `-1`, `253402300800000`, `1.5`, `"1793195881883"`, `{}`} {
		raw := json.RawMessage(fmt.Sprintf(`{"id":"turn","startedAt":1700000000,"completedAt":1700000010,"items":[{"id":"answer","type":"agentMessage","text":"Answer","recordedAtMs":%s}]}`, value))
		messages, err := decodeConversationTurn(raw)
		if err != nil || len(messages) != 1 || messages[0].Timestamp == nil || messages[0].Timestamp.Unix() != 1700000010 || messages[0].TimestampSource != "" {
			t.Fatalf("%s: %#v, %v", value, messages, err)
		}
	}
}
