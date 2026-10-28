package worker

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

var bucketClaudeDeletions = []byte("claude_deletions")

// A deletion intent authorizes only this exact native identity. It contains no
// conversation, workspace path or account data and survives the native/local
// commit boundary. Native deletion itself is idempotent across bridge restarts.
type claudeDeletionIntent struct {
	RuntimeID  string   `json:"runtime_id"`
	ThreadID   string   `json:"thread_id"`
	SessionID  string   `json:"session_id"`
	CommandIDs []string `json:"command_ids,omitempty"`
}

func purgedSessionKey(runtimeID, threadID string) []byte {
	return []byte("purged_session:" + runtimeID + "\x00" + threadID)
}

func purgedSessionIDKey(sessionID string) []byte {
	return []byte("purged_session_id:" + sessionID)
}

func (s *Store) beginClaudeDeletion(runtime protocol.Runtime, session protocol.Session, command protocol.Command) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(bucketClaudeDeletions)
		if err != nil {
			return err
		}
		intent := claudeDeletionIntent{RuntimeID: runtime.ID, ThreadID: session.ThreadID, SessionID: session.ID}
		if value := bucket.Get(sessionKey(runtime.ID, session.ThreadID)); value != nil {
			if err := json.Unmarshal(value, &intent); err != nil {
				return err
			}
			if intent.RuntimeID != runtime.ID || intent.ThreadID != session.ThreadID || intent.SessionID != session.ID {
				return errors.New("Claude deletion: pending target identity changed")
			}
		}
		if command.ID != "" && !containsString(intent.CommandIDs, command.ID) {
			intent.CommandIDs = append(intent.CommandIDs, command.ID)
		}
		data, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put(sessionKey(runtime.ID, session.ThreadID), data)
	})
}

func (s *Store) claudeDeletionIntents() ([]claudeDeletionIntent, error) {
	var intents []claudeDeletionIntent
	err := s.db.View(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketClaudeDeletions)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, data []byte) error {
			var intent claudeDeletionIntent
			if err := json.Unmarshal(data, &intent); err != nil {
				return err
			}
			intents = append(intents, intent)
			return nil
		})
	})
	return intents, err
}

func (s *Store) finishClaudeDeletion(runtimeID, threadID string) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		if bucket := tx.Bucket(bucketClaudeDeletions); bucket != nil {
			return bucket.Delete(sessionKey(runtimeID, threadID))
		}
		return nil
	})
}

func (s *Store) refuseClaudeDeletionAttempt(runtimeID, threadID, commandID string, priorIntent bool) error {
	if !priorIntent {
		return s.finishClaudeDeletion(runtimeID, threadID)
	}
	return s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketClaudeDeletions)
		if bucket == nil {
			return nil
		}
		key := sessionKey(runtimeID, threadID)
		var intent claudeDeletionIntent
		if err := json.Unmarshal(bucket.Get(key), &intent); err != nil {
			return err
		}
		for index, id := range intent.CommandIDs {
			if id == commandID {
				intent.CommandIDs = append(intent.CommandIDs[:index], intent.CommandIDs[index+1:]...)
				break
			}
		}
		data, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return bucket.Put(key, data)
	})
}

// Finalize every admitted durable attempt before clearing the deletion fence.
// A retry may supersede an executing attempt whose native response was lost;
// retaining that earlier command ID prevents a permanent maintenance blocker.
func (a *Agent) completeClaudeDeletionReceipts(runtime protocol.Runtime, saved protocol.Session) error {
	var intent claudeDeletionIntent
	err := a.store.db.View(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketClaudeDeletions)
		if bucket == nil {
			return nil
		}
		value := bucket.Get(sessionKey(runtime.ID, saved.ThreadID))
		if value == nil {
			return nil
		}
		return json.Unmarshal(value, &intent)
	})
	if err != nil {
		return err
	}
	for _, commandID := range intent.CommandIDs {
		record, found, err := a.store.LoadCommand(commandID)
		if err != nil {
			return err
		}
		if !found || record.Command.Operation != protocol.DeleteSession || record.Command.SessionID != saved.ID || record.Command.ThreadID != saved.ThreadID || record.Command.RuntimeID != runtime.ID {
			return errors.New("Claude deletion recovery: admitted command identity missing or changed")
		}
		if record.State == CommandCompleted {
			continue
		}
		result := &protocol.Result{CommandID: commandID, State: "completed", Session: &saved}
		if _, err := a.record(record.Command, CommandCompleted, result, "command_completed"); err != nil {
			return err
		}
	}
	return a.store.finishClaudeDeletion(runtime.ID, saved.ThreadID)
}

func (s *Store) claudeDeletionPending(runtimeID, threadID string) (bool, error) {
	pending := false
	err := s.db.View(func(tx *workerdb.Tx) error {
		if bucket := tx.Bucket(bucketClaudeDeletions); bucket != nil {
			pending = bucket.Get(sessionKey(runtimeID, threadID)) != nil
		}
		return nil
	})
	return pending, err
}

func purgedCommandSession(tx *workerdb.Tx, command protocol.Command) *protocol.Session {
	if command.RuntimeID == "" || command.ThreadID == "" || tx.Bucket(bucketMeta).Get(purgedSessionKey(command.RuntimeID, command.ThreadID)) == nil {
		return nil
	}
	var session protocol.Session
	if json.Unmarshal(tx.Bucket(bucketSessions).Get(sessionKey(command.RuntimeID, command.ThreadID)), &session) != nil || !session.Deleted {
		return nil
	}
	return &session
}

func purgedCommandRecordSession(tx *workerdb.Tx, record CommandRecord) *protocol.Session {
	if session := purgedCommandSession(tx, record.Command); session != nil {
		return session
	}
	// Creation commands retain their original empty session target. Their
	// content-free result still identifies which deleted session they created.
	if record.Result != nil && record.Result.Session != nil {
		if key := tx.Bucket(bucketMeta).Get(purgedSessionIDKey(record.Result.Session.ID)); key != nil {
			var session protocol.Session
			if json.Unmarshal(tx.Bucket(bucketSessions).Get(key), &session) == nil && session.Deleted {
				return &session
			}
		}
	}
	return nil
}

// Preserve command identity and terminal outcome for deduplication, while
// removing all prompt, history, attachment, answer and filesystem payloads.
func scrubDeletedCommand(record *CommandRecord, session protocol.Session) {
	record.Command.Arguments = protocol.Arguments{}
	record.Command.ExpectedTurnID = ""
	record.InboundFile, record.ArchiveJob = nil, nil
	if record.Result != nil {
		result := &protocol.Result{CommandID: record.Command.ID, State: record.Result.State}
		if record.Result.Error != nil {
			result.Error = &protocol.Error{Code: record.Result.Error.Code, Message: "The session was deleted."}
		}
		if record.Command.Operation == protocol.DeleteSession || record.Result.Session != nil {
			result.Session = &session
		}
		record.Result = result
	}
}

func purgeClaudeSessionData(tx *workerdb.Tx, session protocol.Session) error {
	if err := tx.Bucket(bucketMeta).Put(purgedSessionKey(session.RuntimeID, session.ThreadID), []byte("1")); err != nil {
		return err
	}
	if err := tx.Bucket(bucketMeta).Put(purgedSessionIDKey(session.ID), sessionKey(session.RuntimeID, session.ThreadID)); err != nil {
		return err
	}
	if err := tx.Bucket(bucketMeta).Delete(archiveVisibilityKey(session.RuntimeID, session.ThreadID)); err != nil {
		return err
	}
	if bucket := tx.Bucket(bucketAsyncQuestions); bucket != nil {
		prefix := []byte(session.ID + "\x00")
		cursor := bucket.Cursor()
		for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
	}
	if err := tx.Bucket(bucketWebUIUploads).ForEach(func(key, data []byte) error {
		var upload webUIUploadRecord
		if err := json.Unmarshal(data, &upload); err != nil {
			return err
		}
		if upload.Scope.SessionID == session.ID || (upload.Scope.RuntimeID == session.RuntimeID && upload.Scope.ThreadID == session.ThreadID) {
			return tx.Bucket(bucketWebUIUploads).Delete(key)
		}
		return nil
	}); err != nil {
		return err
	}
	// Outbox events are immutable: the gateway may have committed an event
	// whose ACK was lost. They retain their bytes until the ordinary ACK purge.
	return tx.Bucket(bucketCommands).ForEach(func(key, data []byte) error {
		record, err := decodeCommand(data)
		if err != nil {
			return err
		}
		matches := record.Command.SessionID == session.ID || (record.Command.RuntimeID == session.RuntimeID && record.Command.ThreadID == session.ThreadID)
		if record.Result != nil && record.Result.Session != nil {
			matches = matches || record.Result.Session.ID == session.ID
		}
		if !matches {
			return nil
		}
		scrubDeletedCommand(&record, session)
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketCommands).Put(key, encoded)
	})
}

func (a *Agent) deleteStoredSession(runtime protocol.Runtime, session protocol.Session) (protocol.Session, error) {
	if a.cfg.AgentKind != "claude" {
		return a.store.DeleteSession(runtime, session)
	}
	saved, err := a.store.deleteSession(runtime, session, true)
	if err == nil {
		a.webHistory.forgetSession(runtime.ID, session.ThreadID)
	}
	return saved, err
}

func (a *Agent) recoverClaudeDeletions() error {
	if a.cfg.AgentKind != "claude" {
		return nil
	}
	intents, err := a.store.claudeDeletionIntents()
	if err != nil {
		return err
	}
	for _, intent := range intents {
		client, runtime, ok := a.manager.Client(intent.RuntimeID)
		if !ok {
			a.log.Warn("Claude session deletion recovery deferred: runtime unavailable", "session_id", intent.SessionID)
			continue
		}
		sessions, err := a.store.ListSessions(runtime.ID)
		if err != nil {
			return err
		}
		var session protocol.Session
		for _, candidate := range sessions {
			if candidate.ID == intent.SessionID && candidate.ThreadID == intent.ThreadID {
				session = candidate
				break
			}
		}
		if session.ID == "" {
			return errors.New("Claude deletion recovery: session identity missing")
		}
		if err := client.DeleteThread(a.ctx, intent.ThreadID); err != nil {
			// A permission or filesystem failure stays fenced in both ledgers;
			// it must not prevent unrelated sessions from coming online.
			a.log.Warn("Claude session deletion recovery deferred: native cleanup incomplete", "session_id", intent.SessionID)
			continue
		}
		saved, err := a.deleteStoredSession(runtime, session)
		if err != nil {
			return err
		}
		if err := a.completeClaudeDeletionReceipts(runtime, saved); err != nil {
			return err
		}
	}
	return nil
}
