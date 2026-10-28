package worker

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const webUIPromptDetachDrain = 15 * time.Second

type webUIPromptAttempt struct {
	requestID string
	startedAt time.Time
	sequence  uint64
}

func (r *webUIRelay) startPromptAttempt(id json.RawMessage) error {
	attempt := webUIPromptAttempt{requestID: uuid.NewString(), startedAt: time.Now().UTC()}
	data, err := json.Marshal(protocol.SessionPromptEvent{RequestID: attempt.requestID, StartedAt: attempt.startedAt})
	if err != nil {
		return err
	}
	event, err := r.pool.c.store.AppendEvent(protocol.Event{RuntimeID: r.runtime.ID, RuntimeGeneration: r.runtime.Generation,
		SessionID: r.session.ID, Kind: "session_prompt_started", Data: data})
	if err != nil {
		return err
	}
	attempt.sequence = event.Seq
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.promptRequests == nil {
		r.promptRequests = make(map[string]webUIPromptAttempt)
		r.promptResults = make(chan struct{}, 1)
	}
	r.promptRequests[webUIID(id)] = attempt
	return nil
}

// Called under relay.mu before the correlated RPC response is forwarded. The
// durable event has the same redaction boundary as terminal native turn errors.
func (r *webUIRelay) finishPromptAttemptLocked(id string, failure json.RawMessage) error {
	attempt, ok := r.promptRequests[id]
	if !ok {
		return nil
	}
	delete(r.promptRequests, id)
	defer func() {
		select {
		case r.promptResults <- struct{}{}:
		default:
		}
	}()
	var native struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(failure, &native) != nil || native.Code == 0 {
		return nil // A success or ambiguous transport loss is never a failure.
	}
	_, session, err := r.pool.targetContext(r.pool.ctx, protocol.WebUIFrame{RuntimeID: r.runtime.ID,
		RuntimeGeneration: r.runtime.Generation, SessionID: r.session.ID})
	if err != nil || session.ThreadID != r.session.ThreadID || session.ActiveTurnID != "" {
		return nil
	}
	message := publicTurnErrorMessage(r.pool.c.store.redactor.Load(), native.Message)
	if message == "" {
		message = "The prompt could not start."
	}
	data, err := json.Marshal(protocol.SessionPromptEvent{RequestID: attempt.requestID, StartedAt: attempt.startedAt,
		AfterEventSeq: attempt.sequence, Error: &protocol.Error{Code: protocol.CodexProtocolError, Message: message}})
	if err != nil {
		return err
	}
	_, err = r.pool.c.store.AppendEvent(protocol.Event{RuntimeID: r.runtime.ID, RuntimeGeneration: r.runtime.Generation,
		SessionID: r.session.ID, Kind: "session_prompt_failed", Data: data})
	return err
}

func (r *webUIRelay) hasPendingPrompt() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.promptRequests) > 0
}

func (r *webUIRelay) drainPromptResults(readDone chan error) {
	if !r.hasPendingPrompt() {
		return
	}
	timer := time.NewTimer(webUIPromptDetachDrain)
	defer timer.Stop()
	for r.hasPendingPrompt() {
		select {
		case <-r.promptResults:
		case err := <-readDone:
			readDone <- err // The run's sole reader join still consumes it.
			return
		case <-r.pool.ctx.Done():
			return
		case <-timer.C:
			return
		}
	}
}
