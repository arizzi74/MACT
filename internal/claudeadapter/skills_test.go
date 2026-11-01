package claudeadapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSkills = `[{"name":"project:inspect","description":"Inspect the project","argumentHint":"<target>"},{"name":"code-review","builtin":true},{"name":"compact","builtin":true},{"name":"clear","builtin":true},{"name":"../bad"}]`

func TestClaudeSkillsDiscoverWithoutPromptAndInvokeNatively(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_SKILLS="+testSkills)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	ctx := context.Background()
	entries, err := r.ListClaudeSkills(ctx, thread.ID)
	if err != nil || len(entries) != 2 || entries[1].Name != "project:inspect" {
		t.Fatalf("catalog = %#v %v", entries, err)
	}
	r.server.mu.Lock()
	turns := len(r.server.threads[thread.ID].Turns)
	r.server.mu.Unlock()
	if turns != 0 {
		t.Fatal("listing created a conversation turn")
	}
	for _, name := range []string{"missing", "compact", "clear", "../bad", "project:inspect\nclear"} {
		if _, _, err := r.StartClaudeSkill(ctx, thread.ID, name, "", false); err == nil {
			t.Fatalf("accepted unavailable skill %q", name)
		}
	}
	data, _ := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if strings.Contains(string(data), `"user"`) {
		t.Fatal("discovery or rejected command submitted a prompt")
	}
	turn, queued, err := r.StartClaudeSkill(ctx, thread.ID, "project:inspect", "src\nkeep arguments", false)
	if err != nil || queued || turn.ID == "" {
		t.Fatalf("invoke: %#v %v %v", turn, queued, err)
	}
	completed := awaitEvent(t, r, "turn_completed")
	if completed.TurnID != turn.ID {
		t.Fatal("skill escaped the native turn lifecycle")
	}
	data, _ = os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if !strings.Contains(string(data), `"user":"/project:inspect src\nkeep arguments"`) {
		t.Fatalf("native dispatch did not preserve arguments: %s", data)
	}
}

func TestClaudeSkillsQueueBehindActiveTurn(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_SKILLS="+testSkills)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	ctx := context.Background()
	_, err := r.StartTurn(ctx, thread.ID, "block")
	if err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, r, "turn_started")
	if _, _, err = r.StartClaudeSkill(ctx, thread.ID, "project:inspect", "", false); err == nil {
		t.Fatal("unfenced concurrent skill admitted")
	}
	_, queued, err := r.StartClaudeSkill(ctx, thread.ID, "project:inspect", "later", true)
	if err != nil || !queued {
		t.Fatalf("queue: %v %v", queued, err)
	}
	r.server.mu.Lock()
	tt := r.server.threads[thread.ID]
	proc := r.server.runners[thread.ID]
	if len(tt.Turns) != 1 || len(tt.Queue) != 1 {
		t.Fatal("skill ran before preceding turn finished")
	}
	// Settle the native parent, then let the normal queue/lifecycle own the skill.
	proc.handle([]byte(`{"type":"result","subtype":"success","result":"done"}`))
	r.server.mu.Unlock()
	awaitEvent(t, r, "turn_completed")
	awaitEvent(t, r, "turn_completed")
	r.server.mu.Lock()
	defer r.server.mu.Unlock()
	if len(tt.Turns) != 2 || len(tt.Queue) != 0 || tt.active() != nil {
		t.Fatal("queued skill did not complete")
	}
}

func TestClaudeSkillCatalogBoundsAndMissingMetadata(t *testing.T) {
	if _, known := decodeNativeSkills(json.RawMessage(`{}`)); known {
		t.Fatal("missing catalog treated as empty")
	}
	if entries, known := decodeNativeSkills(json.RawMessage(`{"commands":[]}`)); !known || len(entries) != 0 {
		t.Fatal("empty catalog unavailable")
	}
	entries, known := decodeNativeSkills(json.RawMessage(`{"commands":[{"name":"ok","description":"<img src=x>\nhello"},{"name":"ok"},{"name":"__private"}]}`))
	if !known || len(entries) != 1 || entries[0].Description != "<img src=x> hello" {
		t.Fatalf("catalog not bounded/deduplicated: %#v", entries)
	}
}
