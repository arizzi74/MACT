package codexadapter

import (
	"context"
	"errors"
)

// ArchiveInventory includes subagents. The public user-session list deliberately
// filters these out, which cannot establish safety for a project snapshot.
func (c *Client) ArchiveInventory(ctx context.Context, max int) ([]Thread, error) {
	return c.archiveInventory(ctx, max, false)
}

// ThreadArchived reconciles a lost archive/unarchive acknowledgement without
// replaying a mutation or assuming that socket closure rolled it back.
func (c *Client) ThreadArchived(ctx context.Context, threadID string) (bool, error) {
	ids, err := c.ArchivedThreadIDs(ctx)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == threadID {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) ArchivedThreadIDs(ctx context.Context) ([]string, error) {
	threads, err := c.archiveInventory(ctx, 10000, true)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		ids = append(ids, thread.ID)
	}
	return ids, nil
}

func (c *Client) archiveInventory(ctx context.Context, max int, archived bool) ([]Thread, error) {
	var result []Thread
	seen := map[string]bool{}
	for cursor := ""; ; {
		params := map[string]any{"limit": 100, "sortKey": "updated_at", "sortDirection": "desc", "archived": archived,
			"sourceKinds":    []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"},
			"modelProviders": []string{},
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page, err := c.listThreads(ctx, "thread/list", params)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Threads...)
		if len(result) > max {
			return nil, errors.New("session archive: native inventory is too large to verify")
		}
		if page.NextCursor == "" {
			return result, nil
		}
		if seen[page.NextCursor] {
			return nil, errors.New("session archive: native inventory has a repeated cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}
