package codexadapter

import (
	"encoding/json"
	"testing"
)

func TestPublicTurnErrorProjection(t *testing.T) {
	for _, tc := range []struct {
		name, method, params, kind, message string
		retry                               bool
	}{
		{
			name: "terminal public reason", method: "turn/completed", kind: "turn_failed", message: "Public failure\nTry again at https://example.com/access.",
			params: `{"threadId":"thread","turn":{"id":"turn","status":"failed","error":{"message":"Public failure\nTry again at https://example.com/access.","additionalDetails":"PRIVATE_PROVIDER_BODY","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":403}}}}}`,
		},
		{
			name: "terminal no public message", method: "turn/completed", kind: "turn_failed",
			params: `{"threadId":"thread","turn":{"id":"turn","status":"failed","error":{"additionalDetails":"PRIVATE_PROVIDER_BODY"}},"message":"UNRELATED_TEXT"}`,
		},
		{
			name: "malformed public message keeps failed state", method: "turn/completed", kind: "turn_failed",
			params: `{"threadId":"thread","turn":{"id":"turn","status":"failed","error":{"message":{"private":"PRIVATE_PROVIDER_BODY"}}}}`,
		},
		{
			name: "nonretry notice", method: "error", kind: "turn_error", message: "Public provider rejection",
			params: `{"threadId":"thread","turnId":"turn","willRetry":false,"message":"UNRELATED_TEXT","error":{"message":"Public provider rejection","data":"PRIVATE_PROVIDER_BODY"}}`,
		},
		{
			name: "retry notice", method: "error", kind: "turn_error", message: "Reconnecting... 2/5", retry: true,
			params: `{"threadId":"thread","turnId":"turn","willRetry":true,"error":{"message":"Reconnecting... 2/5"}}`,
		},
		{
			name: "legacy notice", method: "error", kind: "turn_error", message: "Public legacy reason",
			params: `{"threadId":"thread","turnId":"turn","message":"Public legacy reason"}`,
		},
		{
			name: "unrelated notification", method: "item/completed", kind: "item_completed",
			params: `{"threadId":"thread","turnId":"turn","error":{"message":"PRIVATE_UNRELATED_ERROR"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := newEvent(tc.method, json.RawMessage(tc.params))
			if event.Kind != tc.kind || event.PublicErrorMessage != tc.message || event.WillRetry != tc.retry || event.Unknown {
				t.Fatalf("public error projection: %+v; want kind=%q message=%q retry=%v", event, tc.kind, tc.message, tc.retry)
			}
			if event.ThreadID != "thread" || event.TurnID != "turn" {
				t.Fatalf("public error lost turn ownership: %+v", event)
			}
		})
	}
}
