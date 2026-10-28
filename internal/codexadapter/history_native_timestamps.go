package codexadapter

import (
	"encoding/json"
	"time"
)

// A native adapter can retain exact persisted message dates. Malformed
// optional dates retain the established turn-time fallback.
func historyRecordedTimestamp(raw json.RawMessage) *time.Time {
	var milliseconds int64
	if len(raw) == 0 || json.Unmarshal(raw, &milliseconds) != nil || milliseconds <= 0 || milliseconds > 253402300799999 {
		return nil
	}
	stamp := time.UnixMilli(milliseconds).UTC()
	return &stamp
}
