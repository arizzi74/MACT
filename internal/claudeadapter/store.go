package claudeadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type savedThread struct {
	ID                     string         `json:"id"`
	Name                   string         `json:"name,omitempty"`
	CWD                    string         `json:"cwd"`
	Model                  string         `json:"model,omitempty"`
	CreatedAt              int64          `json:"createdAt"`
	UpdatedAt              int64          `json:"updatedAt"`
	NativeStarted          bool           `json:"nativeStarted"`
	NativeConfigRoots      []string       `json:"nativeConfigRoots,omitempty"`
	Turns                  []*savedTurn   `json:"turns"`
	Queue                  []*queuedInput `json:"queue,omitempty"`
	ClaudeStatus           *ClaudeStatus  `json:"claudeStatus,omitempty"`
	RequestedModel         string         `json:"requestedModel,omitempty"`
	StatusConfigModel      string         `json:"statusConfigModel,omitempty"`
	StatusConfigPermission string         `json:"statusConfigPermission,omitempty"`
}

type queuedInput struct {
	ID      string           `json:"id"`
	Content []map[string]any `json:"content"`
}

type savedTurn struct {
	ID             string           `json:"id"`
	Status         string           `json:"status"`
	StartedAt      int64            `json:"startedAt"`
	CompletedAt    *int64           `json:"completedAt"`
	Items          []map[string]any `json:"items"`
	NativeResultID string           `json:"nativeResultId,omitempty"`
}

func (t *savedThread) active() *savedTurn {
	if len(t.Turns) == 0 {
		return nil
	}
	turn := t.Turns[len(t.Turns)-1]
	if turn.Status == "inProgress" {
		return turn
	}
	return nil
}

func (t *savedThread) metadata(includeTurns bool) map[string]any {
	status := "idle"
	if t.active() != nil {
		status = "active"
	}
	preview := ""
	for _, turn := range t.Turns {
		for _, item := range turn.Items {
			if item["type"] == "userMessage" {
				if content, ok := item["content"].([]any); ok {
					for _, block := range content {
						if part, ok := block.(map[string]any); ok && part["type"] == "text" {
							preview, _ = part["text"].(string)
							break
						}
					}
				}
				if content, ok := item["content"].([]map[string]any); ok {
					for _, part := range content {
						if part["type"] == "text" {
							preview, _ = part["text"].(string)
							break
						}
					}
				}
				break
			}
		}
		if preview != "" {
			break
		}
	}
	if len(preview) > 512 {
		preview = string([]rune(preview)[:min(128, len([]rune(preview)))])
	}
	value := map[string]any{"id": t.ID, "sessionId": t.ID, "name": t.Name, "cwd": t.CWD, "model": t.Model, "modelProvider": "anthropic", "source": "appServer", "threadSource": "user", "status": map[string]string{"type": status}, "createdAt": t.CreatedAt, "updatedAt": t.UpdatedAt, "preview": preview, "ephemeral": false}
	if includeTurns {
		turns := make([]any, 0, len(t.Turns))
		for _, turn := range t.Turns {
			turns = append(turns, turn.view(true))
		}
		value["turns"] = turns
	}
	return value
}

func (t *savedTurn) view(full bool) map[string]any {
	items := []map[string]any{}
	view := "notLoaded"
	if full {
		view = "full"
		items = t.Items
	}
	return map[string]any{"id": t.ID, "status": t.Status, "startedAt": t.StartedAt, "completedAt": t.CompletedAt, "itemsView": view, "items": items}
}

func canonicalID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private Claude directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Claude directory must be a private directory (0700)")
	}
	return nil
}

func validateSettingsFile(path string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return errors.New("Claude settings path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("Claude settings file is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Claude settings file must be a private regular file (0600)")
	}
	return nil
}

func loadThreads(directory string) (map[string]*savedThread, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	threads := make(map[string]*savedThread)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !canonicalID(id) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("Claude history file must be a private regular file")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxMessageBytes+1))
		_ = f.Close()
		if err != nil || len(data) > maxMessageBytes {
			return nil, errors.New("Claude history file exceeds size limit")
		}
		var thread savedThread
		if json.Unmarshal(data, &thread) != nil || thread.ID != id || !filepath.IsAbs(thread.CWD) {
			return nil, errors.New("invalid Claude history metadata")
		}
		for _, turn := range thread.Turns {
			if turn == nil || !canonicalID(turn.ID) {
				return nil, errors.New("invalid Claude history turn")
			}
			if turn.Status == "inProgress" {
				now := time.Now().Unix()
				turn.Status = "interrupted"
				turn.CompletedAt = &now
			}
		}
		if len(thread.Queue) > 0 {
			if len(thread.Turns) > 0 {
				last := thread.Turns[len(thread.Turns)-1]
				last.Items = append(last.Items, map[string]any{"id": uuid.NewString(), "type": "agentMessage", "text": "Queued messages were cancelled when the worker restarted; resend them.", "phase": "final_answer"})
			}
			thread.Queue = nil
		}
		threads[id] = &thread
		sanitizeSavedStatus(&thread)
	}
	return threads, nil
}

func (s *server) save(thread *savedThread) error {
	if s.deletions[thread.ID] != nil {
		return errors.New("Claude session is being deleted and cannot be saved")
	}
	if !canonicalID(thread.ID) {
		return errors.New("invalid Claude session ID")
	}
	data, err := json.Marshal(thread)
	if err != nil {
		return err
	}
	if len(data) > maxMessageBytes {
		return errors.New("Claude display history exceeds size limit")
	}
	f, err := os.CreateTemp(s.cfg.StateDirectory, ".session-*")
	if err != nil {
		return errors.New("cannot write Claude session metadata")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(name, filepath.Join(s.cfg.StateDirectory, thread.ID+".json"))
	}
	if err != nil {
		return errors.New("cannot persist Claude session metadata")
	}
	dir, err := os.Open(s.cfg.StateDirectory)
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		return errors.New("cannot sync Claude session metadata")
	}
	return nil
}

// Extra arguments are administrator supplied but cannot replace the owned
// session, framing, or permission callback channels.
func validateArgs(args []string) error {
	for _, arg := range args {
		flag := strings.SplitN(arg, "=", 2)[0]
		switch flag {
		case "--", "--resume", "-r", "--continue", "-c", "--session-id", "--fork-session", "--output-format", "--input-format", "--permission-prompt-tool", "--no-session-persistence", "--print", "-p", "--permission-mode":
			return errors.New("Claude arguments cannot override adapter protocol or session flags")
		}
	}
	return nil
}
