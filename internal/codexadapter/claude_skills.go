package codexadapter

import (
	"context"
	"github.com/iaia/telegramgw/internal/protocol"
)

func (c *Client) ListClaudeSkills(ctx context.Context, threadID string) ([]protocol.Skill, error) {
	var result struct {
		Skills []protocol.Skill `json:"skills"`
	}
	err := c.request(ctx, "thread/skills/list", map[string]any{"threadId": threadID}, &result, false)
	return result.Skills, err
}

func (c *Client) StartClaudeSkill(ctx context.Context, threadID, name, args string, queueIfBusy bool) (Turn, bool, error) {
	var result struct {
		Turn   Turn `json:"turn"`
		Queued bool `json:"queued"`
	}
	err := c.request(ctx, "thread/skill/start", map[string]any{"threadId": threadID, "name": name, "args": args, "queueIfBusy": queueIfBusy}, &result, false)
	return result.Turn, result.Queued, err
}
