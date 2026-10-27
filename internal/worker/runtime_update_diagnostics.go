package worker

import (
	"bytes"
	"encoding/json"

	"github.com/iaia/telegramgw/internal/protocol"
)

// snapshotWithUpdateSafety adds bounded RPC delivery diagnostics to hello and
// heartbeat runtime metadata. Diagnostics attach only when both observations
// agree on runtime identity and generation. This does not hold an update lease;
// the live preparation checks remain authoritative.
func (m *RuntimeManager) snapshotWithUpdateSafety() []protocol.Runtime {
	return mergeRuntimeUpdateSafety(m.Snapshot(), m.rpcUpdateSafety())
}

func mergeRuntimeUpdateSafety(runtimes []protocol.Runtime, safety []RuntimeRPCUpdateSafety) []protocol.Runtime {
	type identity struct {
		id         string
		generation uint64
	}
	byRuntime := make(map[identity]RuntimeRPCUpdateSafety, len(safety))
	for _, status := range safety {
		byRuntime[identity{status.RuntimeID, status.Generation}] = status
	}
	result := make([]protocol.Runtime, len(runtimes))
	copy(result, runtimes)
	for i := range result {
		runtime := &result[i]
		// Snapshot records may share raw metadata with the manager. Even records
		// without a matching client must remain independent of stored state.
		runtime.Metadata = bytes.Clone(runtime.Metadata)
		status, ok := byRuntime[identity{runtime.ID, runtime.Generation}]
		if !ok {
			continue
		}
		var metadata map[string]json.RawMessage
		if len(bytes.TrimSpace(runtime.Metadata)) != 0 {
			if err := json.Unmarshal(runtime.Metadata, &metadata); err != nil {
				continue
			}
		}
		if metadata == nil {
			metadata = make(map[string]json.RawMessage)
		}
		// Reuse only the adapter's sanitized status: no RPC parameters, response
		// bodies, thread identities or arbitrary server text enter metadata.
		diagnostic, err := json.Marshal(status.UpdateSafetyStatus)
		if err != nil {
			continue
		}
		metadata["rpc_update_safety"] = diagnostic
		merged, err := json.Marshal(metadata)
		if err == nil {
			runtime.Metadata = merged
		}
	}
	return result
}
