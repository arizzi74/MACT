package protocol

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// WebUIFrame is an ephemeral, session-bound relay. Frames are never
// persisted in the command outbox or replayed following a reconnect.
type WebUIFrame struct {
	ID                string          `json:"id"`
	Action            string          `json:"action"`
	RuntimeID         string          `json:"runtime_id,omitempty"`
	RuntimeGeneration uint64          `json:"runtime_generation,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	Data              json.RawMessage `json:"data,omitempty"`
	Error             string          `json:"error,omitempty"`
}

// One maximum file expands to ~26.7 MiB of base64. Reserve space for
// metadata and both relay/envelope wrappers beneath the 32 MiB transport cap.
const MaxWebUIInputBytes = 30 << 20
const LegacyMaxWebUIInputBytes = 15 << 20

// A larger advertised file limit also establishes support for the enlarged
// relay frame. Keep legacy inputs bounded even when padded with JSON whitespace.
func WorkerWebUIInputByteLimit(advertisedFileBytes int) int {
	if WorkerFileByteLimit(advertisedFileBytes) <= LegacyMaxFileBytes {
		return LegacyMaxWebUIInputBytes
	}
	return MaxWebUIInputBytes
}

func (f WebUIFrame) Validate() error {
	if _, err := uuid.Parse(f.ID); err != nil {
		return errors.New("invalid web UI connection identity")
	}
	switch f.Action {
	case "open", "file_open":
		if _, err := uuid.Parse(f.RuntimeID); err != nil {
			return errors.New("invalid web UI runtime identity")
		}
		if _, err := uuid.Parse(f.SessionID); err != nil || f.RuntimeGeneration == 0 {
			return errors.New("invalid web UI session target")
		}
		if f.Action == "file_open" {
			if _, err := DecodeWebUIFileRequest(f.Data); err != nil {
				return err
			}
		}
	case "input":
		if len(f.Data) == 0 || len(f.Data) > MaxWebUIInputBytes || !json.Valid(f.Data) {
			return errors.New("invalid or oversized web UI input")
		}
	case "close", "ready", "output", "error":
	default:
		return errors.New("unknown web UI action")
	}
	return nil
}
