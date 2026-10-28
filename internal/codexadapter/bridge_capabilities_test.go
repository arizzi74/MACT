package codexadapter

import (
	"context"
	"testing"
)

func TestBridgeAdvertisedCapabilitiesReplaceOptimisticDefaults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     map[string]any
		wantStart bool
		wantSteer bool
	}{
		{"native legacy", map[string]any{"userAgent": "codex/1.0.0"}, true, true},
		{"bridge subset", map[string]any{"supportedMethods": []string{"initialize", "thread/start", "turn/start"}}, true, false},
		{"empty subset", map[string]any{"supportedMethods": []string{}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			done := make(chan error, 1)
			go func() { done <- client.Initialize(context.Background()) }()
			request := fake.next(t)
			fake.respond(t, request, tc.reply)
			if method(t, fake.next(t)) != "initialized" {
				t.Fatal("missing handshake notification")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if client.Supports("turn/start") != tc.wantStart || client.Supports("turn/steer") != tc.wantSteer {
				t.Fatal("advertised methods were not respected")
			}
		})
	}
}
