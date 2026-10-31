package claudeadapter

import (
	"encoding/json"

	"github.com/google/uuid"
)

// Compaction uses the same persistent native conversation and admission and
// completion fences as a normal turn. A successful result alone does not prove
// compaction: Claude also returns success when there is too little history.
func (p *process) startCompaction(t *savedThread) {
	if item := p.lifecycle.compaction; item != nil && item["status"] == "inProgress" {
		return
	}
	item := map[string]any{"id": uuid.NewString(), "type": "contextCompaction", "status": "inProgress"}
	p.lifecycle.compaction = item
	p.appendItem(t, item)
	p.notifyItem(t, "item/started", item)
	p.saveLifecycle(t)
}

func (p *process) handleCompaction(t *savedThread, raw []byte, subtype, inputID string, inputIDs []string, origin messageOrigin) bool {
	if subtype != "status" && subtype != "compact_boundary" {
		return false
	}
	if p.interrupting || !p.acceptTurnFrame(t, inputID, inputIDs, origin) {
		return true
	}
	if subtype == "status" {
		var event struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(raw, &event) == nil && event.Status == "compacting" {
			p.startCompaction(t)
		}
		return true
	}
	if item := p.lifecycle.compaction; item != nil && item["status"] == "completed" {
		return true
	}
	if p.lifecycle.compaction == nil {
		p.startCompaction(t)
	}
	item := p.lifecycle.compaction
	item["status"] = "completed"
	p.notifyItem(t, "item/completed", item)
	// Newer CLIs report post_tokens (observed in 2.1.316). On older CLIs,
	// invalidate the old count until fresh assistant usage arrives. Neither
	// pre_tokens nor cumulative result token totals describe the new context.
	var boundary struct {
		Metadata struct {
			PostTokens json.RawMessage `json:"post_tokens"`
		} `json:"compact_metadata"`
	}
	_ = json.Unmarshal(raw, &boundary)
	t.ensureStatus().Context.UsedTokens = rawTokens(boundary.Metadata.PostTokens, false)
	p.publishStatus(t)
	p.saveLifecycle(t)
	return true
}

func (p *process) settleCompaction(t *savedThread, status string) {
	item := p.lifecycle.compaction
	if item == nil || item["status"] != "inProgress" {
		return
	}
	if status == "completed" {
		status = "skipped"
	}
	item["status"] = status
	p.notifyItem(t, "item/completed", item)
}
