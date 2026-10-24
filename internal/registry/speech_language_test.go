package registry

import "testing"

func TestTelegramSpeechRetainsEnglishVoiceLanguage(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, result := completeSpeechVoice(t, env, true, "en")
	progressEvent(t, env, 2, "turn_started", "english-voice-turn", result.CommandID)
	progressEvent(t, env, 3, "turn_completed", "english-voice-turn", "")
	job := claimSpeechTest(t, env.store)
	if job.Language != "en" || job.TurnID != "english-voice-turn" {
		t.Fatalf("English voice reply lost its language or turn: %q %q", job.Language, job.TurnID)
	}
}
