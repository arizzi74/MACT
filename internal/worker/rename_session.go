package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

// Titles do not become paths. Keep their validation independent of the
// directory-name restrictions used when creating a project.
func normalizeSessionTitle(name string) (string, error) {
	name, err := protocol.NormalizeSessionTitle(name)
	if err != nil {
		return "", validationError("Enter a session name of 1–120 characters without control characters.")
	}
	return name, nil
}

func (s *sessionActor) renameSession(ctx context.Context, client *codexadapter.Client, name string) (protocol.Result, error) {
	name, err := normalizeSessionTitle(name)
	if err != nil {
		return protocol.Result{}, err
	}
	if err := client.RenameThread(ctx, s.session.ThreadID, name); err != nil {
		return protocol.Result{}, err
	}
	saved, err := s.agent.store.ConfirmSessionName(s.runtime, s.session, name)
	if err != nil {
		return protocol.Result{}, err
	}
	s.session = saved
	return protocol.Result{Text: "Thread renamed to “" + name + "”.", Session: &saved}, nil
}

// ConfirmSessionName records the native-confirmed title and its inventory event
// atomically. A failed durable write cannot be reported as a successful rename.
func (s *Store) ConfirmSessionName(runtime protocol.Runtime, expected protocol.Session, name string) (protocol.Session, error) {
	if runtime.WorkerID != s.workerID || expected.WorkerID != s.workerID || expected.RuntimeID != runtime.ID || expected.ID == "" || expected.ThreadID == "" || runtime.Generation == 0 {
		return protocol.Session{}, errors.New("worker store: invalid session rename target")
	}
	var saved protocol.Session
	err := s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketSessions)
		key := sessionKey(runtime.ID, expected.ThreadID)
		value := bucket.Get(key)
		if value == nil {
			return errors.New("worker store: renamed session identity is missing")
		}
		if err := json.Unmarshal(value, &saved); err != nil {
			return err
		}
		if saved.ID != expected.ID || saved.Deleted {
			return errors.New("worker store: renamed session identity changed")
		}
		if saved.Name == name {
			return nil
		}
		saved.Name, saved.UpdatedAt = name, time.Now().UTC()
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		_, err = s.appendEvent(tx, protocol.Event{WorkerID: s.workerID, RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: saved.ID, Kind: "session_state_changed", Data: encoded})
		return err
	})
	return saved, err
}

func (a *Agent) observeSessionName(runtime protocol.Runtime, threadID, name string) {
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil {
		a.report(err)
		return
	}
	for _, session := range sessions {
		if session.ThreadID == threadID && !session.Deleted {
			_, err = a.store.ConfirmSessionName(runtime, session, name)
			a.report(err)
			return
		}
	}
}
