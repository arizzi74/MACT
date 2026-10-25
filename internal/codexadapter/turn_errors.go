package codexadapter

import "encoding/json"

// Project only the public fields the Web UI displays. The error object can
// also contain private provider details, HTTP data, or future diagnostics.
func publicTurnError(method string, params json.RawMessage) (string, bool) {
	if method != "turn/completed" && method != "error" {
		return "", false
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(params, &value) != nil {
		return "", false
	}
	raw := value["error"]
	if method == "turn/completed" {
		var turn struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(value["turn"], &turn)
		raw = turn.Error
	}
	var public struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &public) != nil {
		public.Message = ""
	}
	if method == "error" && public.Message == "" {
		_ = json.Unmarshal(value["message"], &public.Message)
	}
	var retry bool
	_ = json.Unmarshal(value["willRetry"], &retry)
	return public.Message, retry
}
