package registry

import (
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"testing"
	"time"
)

func TestSkillMenuFrozenTargetChoiceAndSingleUse(t *testing.T) {
	for _, alias := range []string{"skill", "skills"} {
		t.Run(alias, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			in := telegramUpdate(env, 2)
			in.Action, in.Target, in.TopicID = "codex", alias, 77
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" {
				t.Fatalf("accept: %#v %v", accepted, err)
			}
			menu := &protocol.SkillMenu{Options: []protocol.SkillOption{{Args: "plugin:review", Label: "Review"}, {Args: "--page 1", Label: "Next"}}}
			event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, State: "completed", SkillMenu: menu})
			if err = env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
				t.Fatal(err)
			}
			cb := Callback{Action: "skill", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77, SessionID: env.session, RuntimeID: env.runtime, Generation: 1, QuestionID: accepted.CommandID, Decision: "plugin:review", ExpiresAt: time.Now().Add(time.Minute)}
			bad := cb
			bad.Decision = "not-offered"
			if _, err = env.store.CreateCallback(ctx, bad); err == nil {
				t.Fatal("unoffered skill accepted")
			}
			bad = cb
			bad.UserID = 11
			if _, err = env.store.CreateCallback(ctx, bad); err == nil {
				t.Fatal("another user can use menu")
			}
			token, err := env.store.CreateCallback(ctx, cb)
			if err != nil {
				t.Fatal(err)
			}
			cb.Decision = "--page 1"
			sibling, err := env.store.CreateCallback(ctx, cb)
			if err != nil {
				t.Fatal(err)
			}
			other := uuid.New()
			insertRouteSession(t, env, other, "other-skill-thread", "")
			selection := telegramUpdate(env, 3)
			selection.Action, selection.Target, selection.TopicID = "connect", other.String(), 77
			if _, err = env.store.AcceptTelegram(ctx, selection); err != nil {
				t.Fatal(err)
			}
			in = telegramUpdate(env, 4)
			in.CallbackToken, in.TopicID = token, 77
			chosen, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || chosen.CommandID == "" || chosen.SessionID != env.session.String() {
				t.Fatalf("lost target: %#v %v", chosen, err)
			}
			in.UpdateID, in.CallbackToken = 5, sibling
			if r, err := env.store.AcceptTelegram(ctx, in); err != nil || r.ErrorCode != "callback_invalid" {
				t.Fatalf("menu replay accepted: %#v %v", r, err)
			}
		})
	}
}
