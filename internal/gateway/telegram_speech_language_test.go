package gateway

import "testing"

func TestSpeechRepliesFollowAutomaticLanguageAndExplicitPreferences(t *testing.T) {
	for _, tc := range []struct {
		name, preference, detected, want string
	}{
		{"English voice note", "auto", "en", "english"},
		{"Italian voice note", "auto", "it", "italian"},
		{"empty legacy preference", "", "en", "english"},
		{"explicit English", "english", "it", "english"},
		{"explicit Italian", "italian", "en", "italian"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, store, engine, api, settings, job := newSpeechProcessorTest()
			settings.value.TTSVoice = tc.preference
			settings.value.TTSEngine = "piper"
			job.Language = tc.detected
			p.process(t.Context(), job)
			if engine.calls != 1 || engine.language != tc.want || api.calls != 1 || !store.last || store.code != "" {
				t.Fatalf("wrong audio reply: language=%q calls=%d sent=%d error=%q", engine.language, engine.calls, api.calls, store.code)
			}
		})
	}
}
