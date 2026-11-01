package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Sender) renderSkillMenu(ctx context.Context, row registry.Delivery, identity string, event protocol.Event, result protocol.Result) (string, *TelegramKeyboard, error) {
	if err := result.SkillMenu.Validate(); err != nil {
		return "", nil, err
	}
	sessionID, err := requiredUUID("session", event.SessionID)
	if err != nil {
		return "", nil, err
	}
	runtimeID, err := requiredUUID("runtime", event.RuntimeID)
	if err != nil {
		return "", nil, err
	}
	commandID, err := requiredUUID("command", result.CommandID)
	if err != nil {
		return "", nil, err
	}
	if event.RuntimeGeneration == 0 {
		return "", nil, errors.New("render skill menu: missing runtime generation")
	}
	store, ok := s.store.(interface {
		SkillRequester(context.Context, uuid.UUID) (int64, error)
	})
	if !ok {
		return "", nil, errors.New("render skill menu: requester lookup unavailable")
	}
	requester, err := store.SkillRequester(ctx, commandID)
	if err != nil || requester == 0 {
		return "", nil, errors.New("render skill menu: requester unavailable")
	}
	keyboard := &TelegramKeyboard{}
	for _, option := range result.SkillMenu.Options {
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: s.sessionListField(option.Label, 80, 160), Data: strings.Repeat("x", 64)}})
	}
	markup, err := json.Marshal(keyboard)
	if err != nil || len(markup) > sessionKeyboardMaxBytes {
		return "", nil, errors.New("render skill menu: keyboard exceeds budget")
	}
	for i, option := range result.SkillMenu.Options {
		token, err := s.callback(ctx, row, registry.Callback{
			Action: "skill", UserID: requester, SessionID: sessionID, RuntimeID: runtimeID,
			Generation: int64(event.RuntimeGeneration), Decision: option.Args, QuestionID: result.CommandID,
		})
		if err != nil {
			return "", nil, err
		}
		keyboard.Rows[i][0].Data = token
	}
	text := identity + "\n\n" + result.Text
	for _, option := range result.SkillMenu.Options {
		if option.Description != "" {
			text += "\n\n" + option.Label + " — " + s.sessionListField(option.Description, 140, 560)
		}
	}
	return text, keyboard, nil
}
