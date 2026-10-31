package claudeadapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCompactionPreservesConversationAndSettingsAcrossRestart(t *testing.T) {
	cfg := testConfig(t)
	r := startTest(t, cfg)
	thread := startThread(t, r)
	runTurn(t, r, thread.ID, "Remember the project requirements")
	if !r.Supports("thread/compact/start") {
		t.Fatal("native compaction is not advertised")
	}
	r.server.mu.Lock()
	saved := r.server.threads[thread.ID]
	effort := "high"
	saved.RequestedModel, saved.RequestedEffort, saved.RequestedPermissionMode = "sonnet", &effort, "acceptEdits"
	tokens := int64(32000)
	saved.ensureStatus().Context.UsedTokens = &tokens
	if err := r.server.save(saved); err != nil {
		t.Fatal(err)
	}
	r.server.mu.Unlock()
	// Resume the existing native conversation, including after a worker restart.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = startTest(t, cfg)
	if err := r.CompactThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	completed := awaitEvent(t, r, "turn_completed")
	if completed.ThreadID != thread.ID {
		t.Fatal("compaction replaced the conversation")
	}
	status := readStatus(t, r, thread.ID)
	if status.Context.UsedTokens != nil {
		t.Fatal("pre-compaction context was presented as current usage")
	}
	r.server.mu.Lock()
	saved = r.server.threads[thread.ID]
	last := saved.Turns[len(saved.Turns)-1]
	if len(saved.Turns) != 2 || saved.RequestedModel != "sonnet" || saved.RequestedEffort == nil || *saved.RequestedEffort != effort || saved.RequestedPermissionMode != "acceptEdits" {
		t.Fatal("compaction changed the saved conversation or settings")
	}
	found := false
	for _, item := range last.Items {
		if item["type"] == "contextCompaction" {
			found = item["status"] == "completed"
		}
	}
	r.server.mu.Unlock()
	if !found {
		t.Fatal("native compact_boundary was not recorded")
	}
	actions, err := os.ReadFile(filepath.Join(cfg.WorkingDirectory, "actions.jsonl"))
	if err != nil || !strings.Contains(string(actions), `"resume":true`) || !strings.Contains(string(actions), `"user":"/compact"`) {
		t.Fatalf("compaction did not resume the native conversation: %v", err)
	}
	runTurn(t, r, thread.ID, "Continue after compaction")
}

func TestClaudeCompactionTerminalOutcomes(t *testing.T) {
	for _, test := range []struct{ mode, event, outcome string }{
		{"skip", "turn_completed", "skipped"},
		{"error", "turn_failed", "failed"},
		{"exit", "turn_failed", "failed"},
		{"block", "turn_interrupted", "interrupted"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_COMPACT="+test.mode)
			r := startTest(t, cfg)
			thread := startThread(t, r)
			if err := r.CompactThread(context.Background(), thread.ID); err != nil {
				t.Fatal(err)
			}
			started := awaitEvent(t, r, "turn_started")
			if test.mode == "block" {
				if err := r.CompactThread(context.Background(), thread.ID); err == nil {
					t.Fatal("overlapping compaction was admitted")
				}
				if err := r.Interrupt(context.Background(), thread.ID, started.TurnID); err != nil {
					t.Fatal(err)
				}
			}
			awaitEvent(t, r, test.event)
			r.server.mu.Lock()
			defer r.server.mu.Unlock()
			saved := r.server.threads[thread.ID]
			if saved.active() != nil {
				t.Fatal("compaction left the session busy")
			}
			for _, item := range saved.Turns[len(saved.Turns)-1].Items {
				if item["type"] == "contextCompaction" {
					if item["status"] != test.outcome {
						t.Fatalf("compaction outcome = %v, want %s", item["status"], test.outcome)
					}
					return
				}
			}
			t.Fatal("compaction outcome was not saved")
		})
	}
}

func TestClaudeCompactionWaitsForNativeIdleAndRejectsBusySession(t *testing.T) {
	cfg := testConfig(t)
	cfg.Env = append(cfg.Env, "MACT_CLAUDE_HELPER_COMPACT=hold-idle")
	r := startTest(t, cfg)
	thread := startThread(t, r)
	if err := r.CompactThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	for {
		event := awaitAny(t, r)
		if event.Kind == "turn_completed" {
			t.Fatal("compaction finished before native idle")
		}
		if event.Method == "item/completed" && event.ItemType == "agentMessage" {
			break
		}
	}
	if err := r.CompactThread(context.Background(), thread.ID); err == nil {
		t.Fatal("compaction ignored the native lifecycle guard")
	}
	r.server.mu.Lock()
	proc := r.server.runners[thread.ID]
	proc.handle([]byte(`{"type":"system","subtype":"session_state_changed","state":"idle"}`))
	r.server.mu.Unlock()
	awaitEvent(t, r, "turn_completed")
}

func TestClaudeAutomaticCompactionAndStaleBoundary(t *testing.T) {
	r := startTest(t, testConfig(t))
	thread := startThread(t, r)
	turnID := acceptStatusTurn(t, r, thread.ID)
	p := nativeStatusProcess(r, thread.ID)
	feedStatus(p, `{"type":"system","subtype":"compact_boundary","user_message_uuid":"old-turn"}`)
	feedStatus(p, `{"type":"system","subtype":"status","status":"compacting"}`)
	feedStatus(p, `{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":32000,"post_tokens":1200}}`)
	wantTokens(t, readStatus(t, r, thread.ID).Context.UsedTokens, 1200)
	feedStatus(p, `{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":32000}}`)
	feedStatus(p, `{"type":"assistant","message":{"id":"after-compact","model":"sonnet","usage":{"input_tokens":1234},"content":[{"type":"text","text":"continued"}]}}`)
	feedStatus(p, `{"type":"result","subtype":"success","result":"continued"}`)
	awaitEvent(t, r, "turn_completed")
	wantTokens(t, readStatus(t, r, thread.ID).Context.UsedTokens, 1234)
	full, err := r.ReadThread(context.Background(), thread.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Turns []struct {
			ID    string
			Items []struct{ Type, Status string }
		}
	}
	if err := json.Unmarshal(full.Raw, &data); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, turn := range data.Turns {
		if turn.ID == turnID {
			for _, item := range turn.Items {
				if item.Type == "contextCompaction" && item.Status == "completed" {
					count++
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("automatic compaction count = %d, want 1", count)
	}
}
