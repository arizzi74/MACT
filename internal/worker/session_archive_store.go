package worker

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

// The journal is part of the durable command. Every destructive or externally
// visible phase is written before it starts, so a restart can reconcile it.
type sessionArchiveJob struct {
	ArchiveID       string           `json:"archive_id"`
	Directory       string           `json:"directory"`
	ProjectPath     string           `json:"project_path"`
	ThreadIDs       []string         `json:"thread_ids,omitempty"`
	CodexHome       string           `json:"codex_home,omitempty"`
	SQLiteHome      string           `json:"sqlite_home,omitempty"`
	RemovalPath     string           `json:"removal_path,omitempty"`
	ProjectDevice   uint64           `json:"project_device,omitempty"`
	ProjectInode    uint64           `json:"project_inode,omitempty"`
	ParentDevice    uint64           `json:"parent_device,omitempty"`
	ParentInode     uint64           `json:"parent_inode,omitempty"`
	Session         protocol.Session `json:"session"`
	Phase           string           `json:"phase"`
	Exported        bool             `json:"exported"`
	NativeArchived  bool             `json:"native_archived"`
	ProjectRemoved  bool             `json:"project_removed"`
	CleanupVerified bool             `json:"cleanup_verified"`
	Imported        bool             `json:"imported"`
	PendingIdentity bool             `json:"pending_identity,omitempty"`
}

func (s *Store) archiveOrigin(archiveID string) (*sessionArchiveJob, error) {
	var origin *sessionArchiveJob
	err := s.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketCommands).ForEach(func(_, value []byte) error {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			if record.Command.Operation == protocol.ArchiveSession && record.ArchiveJob != nil && record.ArchiveJob.ArchiveID == archiveID {
				copy := *record.ArchiveJob
				origin = &copy
			}
			return nil
		})
	})
	return origin, err
}

func (s *Store) archiveJobs() ([]CommandRecord, error) {
	var records []CommandRecord
	err := s.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketCommands).ForEach(func(_, value []byte) error {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			if record.State == CommandExecuting && record.ArchiveJob != nil {
				records = append(records, record)
			}
			return nil
		})
	})
	return records, err
}

func (s *Store) saveArchiveJob(command protocol.Command, job sessionArchiveJob) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketCommands)
		value := b.Get([]byte(command.ID))
		if value == nil {
			return errors.New("worker store: archive command is missing")
		}
		record, err := decodeCommand(value)
		if err != nil {
			return err
		}
		record.State, record.ArchiveJob = CommandExecuting, &job
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(command.ID), encoded); err != nil {
			return err
		}
		result := protocol.Result{CommandID: command.ID, State: "running", SessionArchive: &protocol.SessionArchiveResult{Phase: job.Phase, ArchiveDirectory: job.Directory}}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = s.appendEvent(tx, protocol.Event{RuntimeID: command.RuntimeID, RuntimeGeneration: command.RuntimeGeneration, SessionID: command.SessionID, Kind: "session_archive_progress", Data: body})
		return err
	})
}

func archiveVisibilityKey(runtimeID, threadID string) []byte {
	return []byte("session_archive:" + runtimeID + ":" + threadID)
}

// ConfirmArchiveVisibility pins full archival against in-flight discovery and
// atomically publishes its state. Restoring releases that same durable pin.
func (s *Store) ConfirmArchiveVisibility(runtime protocol.Runtime, expected protocol.Session, archiveID string, archived bool) (protocol.Session, error) {
	if runtime.WorkerID != s.workerID || expected.WorkerID != s.workerID || expected.RuntimeID != runtime.ID || expected.ID == "" || expected.ThreadID == "" || runtime.Generation == 0 {
		return protocol.Session{}, errors.New("worker store: invalid full archive visibility target")
	}
	var saved protocol.Session
	err := s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketSessions)
		key := sessionKey(runtime.ID, expected.ThreadID)
		if value := bucket.Get(key); value != nil {
			if err := json.Unmarshal(value, &saved); err != nil {
				return err
			}
		} else {
			saved = expected
		}
		if saved.ID != expected.ID || saved.Deleted {
			return errors.New("worker store: archived session identity changed")
		}
		meta := tx.Bucket(bucketMeta)
		if archived {
			if err := meta.Put(archiveVisibilityKey(runtime.ID, expected.ThreadID), []byte(archiveID)); err != nil {
				return err
			}
			saved.Archived, saved.Loaded, saved.ActiveTurnID, saved.State = true, false, "", "not_loaded"
		} else {
			if err := meta.Delete(archiveVisibilityKey(runtime.ID, expected.ThreadID)); err != nil {
				return err
			}
			saved = expected
			saved.Archived, saved.Deleted, saved.ActiveTurnID = false, false, ""
		}
		saved.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		_, err = s.appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: saved.ID, Kind: "session_state_changed", Data: encoded})
		return err
	})
	return saved, err
}

func (s *Store) checkArchivePending(commandID, project string, defaultCWD map[string]string) error {
	return s.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketCommands).ForEach(func(_, value []byte) error {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			if record.Command.ID == commandID || (record.State != CommandReceived && record.State != CommandExecuting) {
				return nil
			}
			cwd := record.Command.Arguments.CWD
			if cwd == "" && record.Command.Operation == protocol.NewSession {
				cwd = defaultCWD[record.Command.RuntimeID]
			}
			if record.Command.SessionID != "" {
				v := tx.Bucket(bucketSessions).Get(sessionKey(record.Command.RuntimeID, record.Command.ThreadID))
				var session protocol.Session
				if v != nil && json.Unmarshal(v, &session) == nil {
					cwd = session.CWD
				}
			}
			if pathsOverlap(cwd, project) {
				return archiveBusy("This project has a queued or executing command.")
			}
			return nil
		})
	})
}
