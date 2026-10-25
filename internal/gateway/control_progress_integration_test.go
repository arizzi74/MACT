package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestProcessedSteerRepostsCardsAfterQueuedReplyAndKeepsReplacementIDs(t *testing.T) {
	ctx := t.Context()
	store := testRegistry(t)
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "progress-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("progress-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime, session := uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	emit := func(kind string, payload any) protocol.Event {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		event := protocol.Event{ID: uuid.NewString(), Seq: sequence, WorkerID: worker.ID.String(), RuntimeID: runtime.String(), RuntimeGeneration: 1, SessionID: session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}
		if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	emit("session_discovered", protocol.Session{ID: session.String(), WorkerID: worker.ID.String(), RuntimeID: runtime.String(), ThreadID: "progress-thread", Name: "Progress session", CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()})
	if selected, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "select", Target: session.String()}); err != nil || selected.ErrorCode != "" {
		t.Fatalf("select: %+v %v", selected, err)
	}
	api := &answerProgressAPI{live: map[int64]SendMessage{}}
	options := SenderOptions{BotID: "bot", OwnerID: 10}
	sender := NewSender(store, api, nil, options)
	flush := func() {
		t.Helper()
		if err := sender.flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	flush()
	emit("turn_started", protocol.Result{TurnID: "progress-turn"})
	emit("agent_progress_message", protocol.Result{TurnID: "progress-turn", Text: "Still investigating"})
	flush()
	hourglassID := int64(len(api.messages))
	emit("tool_progress_message", protocol.Result{TurnID: "progress-turn", Text: "cat settings.json"})
	flush()
	toolID := int64(len(api.messages))
	accepted, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 2, UserID: 10, ChatID: 20, Text: "Include the new requirement"})
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("steer: %+v %v", accepted, err)
	}
	if err := store.AcknowledgeCommand(ctx, worker.ID, connection, protocol.CommandAck{CommandID: accepted.CommandID, Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ClaimTelegramDeletions(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("queued/acknowledged steer moved cards before processing: %+v %v", rows, err)
	}
	completed := emit("command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "progress-turn", State: "completed"})
	flush() // The previously unsent queue acknowledgement must precede the cards.
	queuedID := int64(len(api.messages))
	if queuedID != toolID+1 || !strings.Contains(api.live[queuedID].Text, "Queued for") {
		t.Fatalf("queue acknowledgement did not precede restored progress: %+v", api.messages)
	}
	flush()
	if int64(len(api.messages)) != queuedID {
		t.Fatal("replacement cards escaped before old-card cleanup")
	}
	for range 2 {
		if err := sender.flushDeletions(ctx, store, api); err != nil {
			t.Fatal(err)
		}
	}
	if api.live[hourglassID].Text != "" || api.live[toolID].Text != "" || len(api.deleted) != 2 {
		t.Fatal("old progress cards were not removed")
	}
	sender = NewSender(store, api, nil, options)
	flush()
	flush()
	newHourglass, newTool := queuedID+1, queuedID+2
	if int64(len(api.messages)) != newTool || !strings.Contains(api.live[newHourglass].Text, "Still investigating") || !strings.Contains(api.live[newTool].Text, "cat settings.json") {
		t.Fatalf("cards were not reposted hourglass before tool: %+v", api.messages)
	}
	if !api.live[newHourglass].DisableNotification || !api.live[newTool].DisableNotification || len(api.live[newTool].Entities) != 1 || api.live[newTool].Entities[0].Type != "pre" {
		t.Fatal("restored cards lost silence or tool formatting")
	}
	for _, update := range []struct {
		kind, text string
		id         int64
	}{
		{"agent_progress_message", "New commentary after steer", newHourglass},
		{"tool_progress_message", "printf updated", newTool},
	} {
		emit(update.kind, protocol.Result{TurnID: "progress-turn", Text: update.text})
		flush()
		if len(api.editIDs) == 0 || api.editIDs[len(api.editIDs)-1] != update.id {
			t.Fatalf("%s did not update its replacement card", update.kind)
		}
	}
	if err := store.IngestEvent(ctx, worker.ID, connection, completed); err != nil {
		t.Fatal(err)
	}
	emit("command_completed", protocol.Result{CommandID: accepted.CommandID, TurnID: "progress-turn", State: "completed"})
	flush()
	if int64(len(api.messages)) != newTool || len(api.deleted) != 2 {
		t.Fatal("repeated completion moved progress more than once")
	}
}
