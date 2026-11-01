package gateway

import (
	"context"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"testing"
)

type skillRenderStore struct {
	*renderStoreFake
	commandID uuid.UUID
}

func (s *skillRenderStore) SkillRequester(_ context.Context, id uuid.UUID) (int64, error) {
	if id != s.commandID {
		return 0, registry.ErrTelegramTarget
	}
	return 77, nil
}

func TestSkillMenuKeepsRequesterAndSession(t *testing.T) {
	s := &skillRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New()}
	result := protocol.Result{CommandID: s.commandID.String(), Text: "Skills", SkillMenu: &protocol.SkillMenu{Options: []protocol.SkillOption{{Args: "plugin:review", Label: "Review", Description: "Review source"}, {Args: "--page 1", Label: "Next"}}}}
	sender := NewSender(s, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	_, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil || keyboard == nil || len(s.callbacks) != 2 {
		t.Fatalf("render: %#v %v", keyboard, err)
	}
	for i, cb := range s.callbacks {
		if cb.Action != "skill" || cb.UserID != 77 || cb.SessionID != testSessionID || cb.Decision != result.SkillMenu.Options[i].Args {
			t.Fatalf("lost target: %#v", cb)
		}
	}
}
