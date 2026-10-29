package registry

import "testing"

func TestIncomingTelegramRequiresPositiveUpdateID(t *testing.T) {
	for _, id := range []int64{-1, 0, 1, 10_000_000} {
		in := IncomingUpdate{BotID: "fixture", UpdateID: id, UserID: 7, ChatID: 7, Action: "help"}
		if err := validateIncoming(in); (err == nil) != (id > 0) {
			t.Fatal("incorrect update identifier admission")
		}
	}
}
