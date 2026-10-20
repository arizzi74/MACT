package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func (r *webUIRelay) upload(rpc webUIRPC) error {
	upload, err := protocol.DecodeWebUIUploadParams(rpc.Params)
	var result protocol.WebUIUploadResult
	if err == nil {
		ctx, cancel := context.WithTimeout(r.ctx, 45*time.Second)
		defer cancel()
		if save := r.pool.c.uploadWebUI; save != nil {
			result, err = save(ctx, r.runtime, r.session, upload)
		} else {
			err = validationError("Update this worker to enable Web UI file uploads.")
		}
	}
	r.mu.Lock()
	delete(r.pending, webUIID(rpc.ID))
	r.mu.Unlock()
	reply := webUIRPC{ID: rpc.ID}
	if err != nil {
		message := "The file save was not confirmed. Reconnect and retry the same upload to check its saved path."
		var invalid *CodexCommandValidationError
		var unavailable *protocol.Error
		switch {
		case errors.As(err, &invalid):
			message = safeErrorMessage(r.pool.c.store.redactor.Load(), invalid.Message, "The file upload is unavailable.")
		case errors.As(err, &unavailable):
			message = safeErrorMessage(r.pool.c.store.redactor.Load(), unavailable.Message, "The file upload is unavailable.")
		case errors.Is(err, ErrUpdatePrepared):
			message = "Worker update is in progress. Reconnect after it restarts."
		}
		reply.Error, _ = json.Marshal(map[string]any{"code": -32000, "message": message})
	} else {
		reply.Result, _ = json.Marshal(result)
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	// This result is produced only by the typed local saver. Preserve its exact
	// path for the next prompt; sendRedactedEnvelope still redacts visible text.
	return r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: data})
}

func (a *Agent) saveWebUIUpload(ctx context.Context, runtime protocol.Runtime, session protocol.Session, upload protocol.WebUIUpload) (protocol.WebUIUploadResult, error) {
	if err := upload.Validate(); err != nil {
		return protocol.WebUIUploadResult{}, validationError("%s", err.Error())
	}
	reply, err := a.dispatchWebUIActor(webUIActorCommand{ctx: ctx, runtime: runtime, session: session, upload: &upload})
	return reply.upload, err
}

func (s *sessionActor) webUIUpload(request webUIActorCommand) (protocol.WebUIUploadResult, error) {
	if err := request.ctx.Err(); err != nil {
		return protocol.WebUIUploadResult{}, err
	}
	if request.runtime.ID != s.runtime.ID || request.runtime.Generation != s.runtime.Generation || request.runtime.WorkerID != s.agent.cfg.WorkerID ||
		request.session.ID != s.session.ID || request.session.ThreadID != s.session.ThreadID || request.session.WorkerID != s.agent.cfg.WorkerID ||
		request.session.RuntimeID != s.runtime.ID || s.session.Archived || s.session.Deleted || request.session.CWD != s.session.CWD {
		return protocol.WebUIUploadResult{}, validationError("The session, workspace or runtime changed. Reconnect before uploading the file.")
	}
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != request.runtime.Generation || runtime.State != "running" {
		return protocol.WebUIUploadResult{}, validationError("The runtime changed or is unavailable. Reconnect before uploading the file.")
	}
	cwd, err := auth.CanonicalWorkspace(request.session.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil || cwd != request.session.CWD {
		return protocol.WebUIUploadResult{}, validationError("The session workspace is no longer available or allowed.")
	}
	thread, err := client.ReadThread(request.ctx, s.session.ThreadID, false)
	if err != nil {
		return protocol.WebUIUploadResult{}, err
	}
	actualCWD, err := auth.CanonicalWorkspace(thread.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil || actualCWD != cwd || thread.ID != s.session.ThreadID || !thread.UserSession() {
		return protocol.WebUIUploadResult{}, validationError("The session workspace or identity changed. Reconnect before uploading the file.")
	}
	if err := request.ctx.Err(); err != nil {
		return protocol.WebUIUploadResult{}, err
	}
	path, err := storeWebUIUpload(s.agent.store, s.runtime, s.session, *request.upload, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return protocol.WebUIUploadResult{}, validationError("%s", err.Error())
	}
	return protocol.WebUIUploadResult{UploadID: request.upload.UploadID, Path: path, Text: "File saved in " + path}, nil
}
