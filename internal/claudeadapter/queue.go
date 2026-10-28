package claudeadapter

import (
	"time"

	"github.com/google/uuid"
)

// All queue operations run under server.mu. User input is written to the
// native CLI only after the preceding top-level result has settled.
func (s *server) beginTurn(t *savedThread, content []map[string]any) (*savedTurn, *process, map[string]any, error) {
	t.ensureStatus().Cost.LastTurnUSD = nil
	turn := &savedTurn{ID: uuid.NewString(), Status: "inProgress", StartedAt: time.Now().Unix(), Items: []map[string]any{}}
	userItem := map[string]any{"id": uuid.NewString(), "type": "userMessage", "content": historyInput(content)}
	turn.Items = append(turn.Items, userItem)
	t.Turns = append(t.Turns, turn)
	t.UpdatedAt = turn.StartedAt
	if err := s.save(t); err != nil {
		t.Turns = t.Turns[:len(t.Turns)-1]
		return nil, nil, nil, err
	}
	proc := s.runners[t.ID]
	if proc == nil {
		var err error
		proc, err = s.startProcess(t)
		if err != nil {
			turn.Status = "failed"
			now := time.Now().Unix()
			turn.CompletedAt = &now
			_ = s.save(t)
			return nil, nil, nil, err
		}
		s.runners[t.ID] = proc
	}
	return turn, proc, userItem, nil
}

func (s *server) admitTurn(t *savedThread, turn *savedTurn, proc *process, userItem map[string]any, content []map[string]any) {
	s.broadcast("turn/started", map[string]any{"threadId": t.ID, "turn": map[string]any{"id": turn.ID, "status": "inProgress"}})
	s.broadcast("item/completed", map[string]any{"threadId": t.ID, "turnId": turn.ID, "item": userItem})
	s.broadcastStatus(t)
	go proc.submit(turn.ID, content)
}

func (s *server) nextQueued(t *savedThread) {
	if s.closed || len(t.Queue) == 0 || t.active() != nil {
		return
	}
	entry := t.Queue[0]
	t.Queue = t.Queue[1:]
	turn, proc, userItem, err := s.beginTurn(t, entry.Content)
	if err != nil {
		t.Queue = nil
		if len(t.Turns) > 0 {
			last := t.Turns[len(t.Turns)-1]
			last.Items = append(last.Items, map[string]any{"id": uuid.NewString(), "type": "agentMessage", "text": "The queued message could not start. Queued messages were cancelled; resend them after checking the worker.", "phase": "final_answer"})
		}
		_ = s.save(t)
		s.broadcast("thread/queue/changed", map[string]any{"threadId": t.ID})
		s.broadcast("error", map[string]any{"threadId": t.ID, "error": map[string]any{"message": "Claude Code could not start the queued message. Resend it after checking the worker."}})
		return
	}
	s.broadcast("thread/queue/changed", map[string]any{"threadId": t.ID})
	s.admitTurn(t, turn, proc, userItem, entry.Content)
}
