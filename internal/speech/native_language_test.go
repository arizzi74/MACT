package speech

import (
	"os/exec"
	"testing"
)

func TestPiperLanguageConfirmationRejectsItalianOnlyHelpersForEnglish(t *testing.T) {
	for _, tc := range []struct {
		name, requested, reported string
		wantError                 bool
	}{
		{"old helper remains usable for Italian", "italian", "", false},
		{"old helper cannot impersonate English", "english", "", true},
		{"Italian voice cannot confirm English", "english", "italian", true},
		{"English voice cannot confirm Italian", "italian", "english", true},
		{"English confirmed", "english", "english", false},
		{"Italian confirmed", "italian", "italian", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fakeService(t, "language-ready")
			s.checkLanguage = true
			command := s.command
			s.command = func(language string, threads int) *exec.Cmd {
				cmd := command(language, threads)
				cmd.Env = append(cmd.Env, "TELEGRAMGW_SPEECH_TEST_LANGUAGE="+tc.reported)
				return cmd
			}
			_, err := s.start(t.Context(), tc.requested, 2)
			if (err != nil) != tc.wantError {
				t.Fatalf("language confirmation: requested=%s reported=%q err=%v", tc.requested, tc.reported, err)
			}
			if tc.wantError && speechProcess(s) != nil {
				t.Fatal("incompatible voice process was not reaped")
			}
		})
	}
}

func TestNativePreparationChecksSelectedLanguagesForBothEngines(t *testing.T) {
	for _, engine := range []string{"piper", "qwen3"} {
		for _, preference := range []string{"auto", "english", "italian"} {
			t.Run(engine+"/"+preference, func(t *testing.T) {
				s := nativeTestService(t)
				var checked []string
				s.nativeFactory = func(string) *Service {
					backend := fakeService(t, "ok")
					command := backend.command
					backend.command = func(language string, threads int) *exec.Cmd {
						checked = append(checked, language)
						return command(language, threads)
					}
					return backend
				}
				settings := nativeTestSettings(engine)
				settings.TTSVoice = preference
				if err := s.Prepare(t.Context(), settings, nil); err != nil {
					t.Fatal(err)
				}
				if preference == "auto" {
					if len(checked) != 2 || checked[0] != "italian" || checked[1] != "english" {
						t.Fatalf("automatic preparation did not check both voices: %v", checked)
					}
				} else if len(checked) != 1 || checked[0] != preference {
					t.Fatalf("wrong prepared voice: %v", checked)
				}
			})
		}
	}
}
