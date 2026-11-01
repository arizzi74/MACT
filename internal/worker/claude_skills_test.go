package worker

import "testing"

func TestClaudeSkillActivityKeepsUpdatesFencedUntilCompletion(t *testing.T) {
	var a nativeActivity
	a.clientMessage([]byte(`{"id":1,"method":"thread/skills/list","params":{"threadId":"thread"}}`))
	a.serverMessage([]byte(`{"id":1,"result":{"skills":[]}}`))
	assertNativeIdle(t, &a)
	a.clientMessage([]byte(`{"id":2,"method":"thread/skill/start","params":{"threadId":"thread"}}`))
	a.serverMessage([]byte(`{"id":2,"result":{"turn":{"id":"skill-turn","status":"inProgress"}}}`))
	if a.idle() {
		t.Fatal("running skill failed to block update")
	}
	a.serverMessage(nativeTurnMessage("turn/completed", "thread", "skill-turn"))
	assertNativeIdle(t, &a)
	if claudeSkillInvocation("skill", "") || claudeSkillInvocation("skills", "--page 1") || !claudeSkillInvocation("skill", "plugin:review src") {
		t.Fatal("skill routing confused discovery and invocation")
	}
}
