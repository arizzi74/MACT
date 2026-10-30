package codexadapter

import (
	"context"
	"errors"
)

// ReadClaudePermissionModes returns only modes advertised by the native Claude
// bridge. Absence is not evidence that any particular mode is available.
func (c *Client) ReadClaudePermissionModes(ctx context.Context, cwd string) ([]string, error) {
	var result struct {
		Config struct {
			Modes []string `json:"supportedPermissionModes"`
		} `json:"config"`
	}
	if err := c.request(ctx, "config/read", map[string]any{"cwd": cwd, "includeLayers": false}, &result, false); err != nil {
		return nil, err
	}
	return result.Config.Modes, nil
}

// UpdateClaudeThreadSettings keeps native Claude permission modes distinct
// from Codex approval policies and sandbox presets.
func (c *Client) UpdateClaudeThreadSettings(ctx context.Context, threadID string, model, effort, permissionMode *string) error {
	if threadID == "" {
		return errors.New("thread id is required")
	}
	params := map[string]any{"threadId": threadID}
	for key, value := range map[string]*string{"model": model, "effort": effort, "permissionMode": permissionMode} {
		if value != nil {
			params[key] = *value
		}
	}
	return c.request(ctx, "thread/settings/update", params, nil, false)
}
