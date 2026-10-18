package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Server) webuiSessionEdit(w http.ResponseWriter, r *http.Request) {
	s.webuiSessionAction(w, r, protocol.RenameSession)
}
func (s *Server) webuiSessionArchive(w http.ResponseWriter, r *http.Request) {
	s.webuiSessionAction(w, r, protocol.ArchiveSession)
}
func (s *Server) webuiSessionArchives(w http.ResponseWriter, r *http.Request) {
	s.webuiSessionAction(w, r, protocol.ListSessionArchives)
}
func (s *Server) webuiSessionRestore(w http.ResponseWriter, r *http.Request) {
	s.webuiSessionAction(w, r, protocol.RestoreSession)
}

func (s *Server) webuiSessionAction(w http.ResponseWriter, r *http.Request, operation protocol.Operation) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, r.Method == http.MethodPost); !ok {
		return
	}
	sessionTarget := operation == protocol.RenameSession || operation == protocol.ArchiveSession
	var result registry.WebUISessionAction
	var err error
	if r.Method == http.MethodGet {
		request := r.URL.Query().Get("request_id")
		command := r.URL.Query().Get("command_id")
		if request == "" {
			request = command
		} else if command != "" && command != request {
			bad(w)
			return
		}
		requestID, e := uuid.Parse(request)
		if e != nil || requestID == uuid.Nil {
			bad(w)
			return
		}
		var workerID, sessionID uuid.UUID
		if sessionTarget {
			sessionID, e = uuid.Parse(r.URL.Query().Get("session_id"))
			if e != nil || sessionID == uuid.Nil {
				bad(w)
				return
			}
		} else {
			workerID, e = uuid.Parse(r.URL.Query().Get("worker_id"))
			if e != nil || workerID == uuid.Nil {
				bad(w)
				return
			}
		}
		result, err = s.store.WebUISessionAction(r.Context(), workerID, sessionID, requestID, operation)
	} else if sessionTarget {
		var in struct {
			SessionID     string `json:"session_id"`
			RequestID     string `json:"request_id"`
			Confirmed     bool   `json:"confirmed"`
			Name          string `json:"name"`
			CWD           string `json:"cwd"`
			DeleteProject bool   `json:"delete_project"`
		}
		if !decode(w, r, &in) {
			return
		}
		sessionID, se := uuid.Parse(in.SessionID)
		requestID, re := uuid.Parse(in.RequestID)
		if se != nil || re != nil || sessionID == uuid.Nil || requestID == uuid.Nil || !in.Confirmed {
			bad(w)
			return
		}
		if operation == protocol.RenameSession {
			if in.DeleteProject {
				bad(w)
				return
			}
			result, err = s.store.QueueWebUISessionEdit(r.Context(), sessionID, requestID, in.Name)
		} else {
			if in.Name != "" {
				bad(w)
				return
			}
			result, err = s.store.QueueWebUISessionArchive(r.Context(), sessionID, requestID, in.CWD, in.DeleteProject)
		}
	} else {
		var in struct {
			webUICreationTargetInput
			ArchiveID string `json:"archive_id"`
			Confirmed bool   `json:"confirmed"`
			Offset    int    `json:"offset"`
		}
		if !decode(w, r, &in) {
			return
		}
		target, ok := in.webUICreationTargetInput.target()
		if !ok {
			bad(w)
			return
		}
		if operation == protocol.RestoreSession && !in.Confirmed {
			bad(w)
			return
		}
		result, err = s.store.QueueWebUIArchiveOperation(r.Context(), target, operation, protocol.SessionArchiveRequest{ArchiveID: in.ArchiveID, Offset: in.Offset})
	}
	if errors.Is(err, registry.ErrWebUISessionActionNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		var actionErr *protocol.Error
		if errors.As(err, &actionErr) {
			writeJSON(w, http.StatusConflict, map[string]string{"error_code": actionErr.Code, "message": actionErr.Message})
		} else {
			fail(w, err)
		}
		return
	}
	raw, err := json.Marshal(result)
	if err == nil {
		raw, err = s.redactWebUI(raw)
	}
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost && result.Pending {
		status = http.StatusAccepted
	}
	writeJSON(w, status, json.RawMessage(raw))
}
