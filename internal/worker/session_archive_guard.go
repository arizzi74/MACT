package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

type archiveAdmission struct {
	Project  string
	ThreadID string
	Threads  map[string]map[string]bool
}

type nativeArchiveFence struct {
	project    string
	defaultCWD string
	known      map[string]bool
	threads    map[string]bool
}

func archiveThreadTree(job sessionArchiveJob) map[string]bool {
	ids := map[string]bool{}
	for _, id := range job.ThreadIDs {
		if id != "" {
			ids[id] = true
		}
	}
	if job.Session.ThreadID != "" {
		ids[job.Session.ThreadID] = true
	}
	return ids
}

func cleanArchivePath(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	var tail []string
	for candidate := filepath.Clean(path); ; candidate = filepath.Dir(candidate) {
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved)
		}
		tail = append(tail, filepath.Base(candidate))
		if filepath.Dir(candidate) == candidate {
			return filepath.Clean(path)
		}
	}
}

func pathsOverlap(left, right string) bool {
	left, right = cleanArchivePath(left), cleanArchivePath(right)
	if left == "" || right == "" {
		return false
	}
	return left == right || strings.HasPrefix(left, right+string(filepath.Separator)) || strings.HasPrefix(right, left+string(filepath.Separator)) || left == "/" || right == "/"
}

// Removing parent would remove a CWD equal to or beneath it. A session whose
// CWD is an ancestor does not own every project underneath that directory.
func archivePathContains(parent, child string) bool {
	parent, child = cleanArchivePath(parent), cleanArchivePath(child)
	if parent == "" || child == "" {
		return false
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func archiveBusy(message string) error {
	return &protocol.Error{Code: protocol.SessionBusy, Message: message, Retryable: true}
}

// Called with updateMu held. Only work which can touch the selected project is
// fenced; independent sessions continue running and serving commands.
func (a *Agent) archiveBlocksCommand(c protocol.Command) bool {
	if c.Operation == protocol.ListSessionArchives || c.Operation == protocol.BrowseWorkspace {
		return false
	}
	if len(a.archiveAdmissions) == 0 {
		return false
	}
	cwd := c.Arguments.CWD
	if c.SessionID != "" {
		sessions, err := a.store.ListSessions(c.RuntimeID)
		if err != nil {
			return true
		}
		for _, session := range sessions {
			if session.ID == c.SessionID {
				cwd = session.CWD
				break
			}
		}
	} else if c.Operation == protocol.NewSession && cwd == "" {
		_, runtime, _ := a.manager.Client(c.RuntimeID)
		cwd = runtime.DefaultCWD
	}
	for _, admission := range a.archiveAdmissions {
		if (admission.ThreadID != "" && c.ThreadID == admission.ThreadID) || admission.Threads[c.RuntimeID][c.ThreadID] || pathsOverlap(cwd, admission.Project) {
			return true
		}
	}
	return false
}

// Every native message is checked while holding the proxy lock, immediately
// before it can be forwarded. Unknown thread mutations are refused until their
// project can be established by a fresh inventory.
func (p *attachmentProxy) archiveBlocksLocked(payload []byte) bool {
	if len(p.archiveFences) == 0 {
		return false
	}
	object, valid := nativeObject(payload)
	if !valid {
		return true
	}
	method := nativeString(object["method"])
	if method == "" || method == "initialize" || method == "initialized" || nativeReadOnlyRequest(method) {
		return false
	}
	params, valid := nativeObject(object["params"])
	if !valid {
		return true
	}
	thread := nativeString(params["threadId"])
	for _, fence := range p.archiveFences {
		for _, key := range []string{"cwd", "workingDirectory", "path", "sourcePath", "destinationPath"} {
			value := nativeString(params[key])
			if value != "" && (!filepath.IsAbs(value) || pathsOverlap(value, fence.project)) {
				return true
			}
		}
		if thread != "" {
			if fence.threads[thread] || !fence.known[thread] {
				return true
			}
			// Only thread/turn RPCs are scoped by their thread ID. A stray
			// threadId on a filesystem/process/global RPC cannot authorize it.
			if strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "turn/") {
				continue
			}
		}
		cwd := nativeString(params["cwd"])
		if cwd == "" && (method == "thread/start" || method == "thread/fork") {
			cwd = fence.defaultCWD
		}
		if cwd != "" {
			if !filepath.IsAbs(cwd) || pathsOverlap(cwd, fence.project) {
				return true
			}
			continue
		}
		if strings.HasPrefix(method, "fs/") {
			path := nativeString(params["path"])
			if path != "" && filepath.IsAbs(path) && !pathsOverlap(path, fence.project) {
				continue
			}
		}
		// Unscoped process and filesystem mutations may affect this project.
		if method != "account/rateLimitResetCredit/consume" {
			return true
		}
	}
	return false
}

// Learning an unrelated user thread does not expand the protected thread tree.
// Only an absolute, independently returned CWD can admit a newly created ID.
func (p *attachmentProxy) observeArchiveUserThreadLocked(id, cwd string) {
	if id == "" || !filepath.IsAbs(cwd) {
		return
	}
	for _, fence := range p.archiveFences {
		if pathsOverlap(cwd, fence.project) {
			delete(fence.known, id)
		} else if !fence.threads[id] {
			fence.known[id] = true
		}
	}
}

func (p *attachmentProxy) observeArchiveBackendThreadLocked(payload []byte) {
	if len(p.archiveFences) == 0 {
		return
	}
	message, ok := nativeObject(payload)
	if !ok {
		return
	}
	container := message["result"]
	if container == nil {
		container = message["params"]
	}
	object, ok := nativeObject(container)
	if !ok {
		return
	}
	thread, ok := nativeObject(object["thread"])
	if !ok {
		return
	}
	source := nativeString(thread["source"])
	switch source {
	case "cli", "vscode", "exec", "appServer":
	default:
		return
	}
	p.observeArchiveUserThreadLocked(nativeString(thread["id"]), nativeString(thread["cwd"]))
}

func (m *RuntimeManager) observeArchiveSession(runtime protocol.Runtime, session protocol.Session) {
	m.mu.RLock()
	current, proxy := m.runtimes[runtime.ID], m.attachments[runtime.ID]
	if current == nil || current.runtime.Generation != runtime.Generation || proxy == nil {
		m.mu.RUnlock()
		return
	}
	proxy.mu.Lock()
	proxy.observeArchiveUserThreadLocked(session.ThreadID, session.CWD)
	proxy.mu.Unlock()
	m.mu.RUnlock()
}

func (s *attachmentSession) rejectDuringArchive(payload []byte) bool {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(payload, &request) != nil || request.Method == "" || len(request.ID) == 0 || string(request.ID) == "null" {
		return false
	}
	response, err := json.Marshal(map[string]any{"id": request.ID, "error": map[string]any{"code": -32002, "message": "This project is being archived or restored. Retry when the operation finishes."}})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	return s.front.Write(ctx, websocket.MessageText, response) == nil
}

func (p *attachmentProxy) checkArchiveIdleLocked(fence *nativeArchiveFence) error {
	if p.closed || p.uncertain {
		return archiveBusy("Native project activity cannot be verified.")
	}
	for session := range p.sessions {
		activity := &session.activity
		if activity.uncertain || len(activity.processes) > 0 {
			return archiveBusy("Native project processes cannot be proven idle.")
		}
		for _, request := range activity.requests {
			if (request.thread != "" && fence.threads[request.thread]) || (request.thread == "" && !nativeReadOnlyRequest(request.method)) {
				return archiveBusy("A native project command is still executing.")
			}
		}
		for _, approval := range activity.approvals {
			if fence.threads[approval.parent.thread] {
				return archiveBusy("The project has pending native approval or input.")
			}
		}
		for turn := range activity.turns {
			if fence.threads[turn.thread] {
				return archiveBusy("The project has an active native turn.")
			}
		}
		for thread := range activity.compact {
			if fence.threads[thread] {
				return archiveBusy("The project is compacting a native session.")
			}
		}
		for thread := range activity.threads {
			if fence.threads[thread] {
				return archiveBusy("The native project is active.")
			}
		}
		for hook := range activity.hooks {
			if fence.threads[hook.thread] {
				return archiveBusy("The project has an active native hook.")
			}
		}
	}
	return nil
}

// projectArchiveGuard retains only project-specific native admission. Runtime
// supervision and unrelated actors remain available throughout compression.
func (a *Agent) projectArchiveGuard(ctx context.Context, command protocol.Command, job sessionArchiveJob, restoring bool) (func(), error) {
	a.manager.mu.Lock()
	type target struct {
		runtime protocol.Runtime
		client  *codexadapter.Client
		proxy   *attachmentProxy
	}
	var targets []target
	if a.manager.archiveScopes == nil {
		a.manager.archiveScopes = map[string]map[string]*nativeArchiveFence{}
	}
	a.manager.archiveScopes[command.ID] = map[string]*nativeArchiveFence{}
	for _, managed := range a.manager.runtimes {
		if managed.client != nil {
			targets = append(targets, target{managed.runtime, managed.client, a.manager.attachments[managed.runtime.ID]})
			a.manager.archiveScopes[command.ID][managed.runtime.ID] = &nativeArchiveFence{project: job.ProjectPath, defaultCWD: managed.runtime.DefaultCWD, known: map[string]bool{}, threads: archiveThreadTree(job)}
		}
	}
	a.manager.mu.Unlock()
	cleanup := func() {
		a.manager.mu.Lock()
		delete(a.manager.archiveScopes, command.ID)
		current := make([]*attachmentProxy, 0, len(a.manager.attachments))
		for _, proxy := range a.manager.attachments {
			current = append(current, proxy)
		}
		a.manager.mu.Unlock()
		for _, proxy := range current {
			proxy.mu.Lock()
			delete(proxy.archiveFences, command.ID)
			proxy.mu.Unlock()
		}
		for _, target := range targets {
			if target.proxy != nil {
				target.proxy.mu.Lock()
				delete(target.proxy.archiveFences, command.ID)
				target.proxy.mu.Unlock()
			}
		}
	}
	fail := func(err error) (func(), error) { cleanup(); return nil, err }
	for _, target := range targets {
		if target.runtime.LocalSocket != "" && target.proxy == nil {
			return fail(archiveBusy("Native project admission is not managed."))
		}
		if target.proxy != nil {
			target.proxy.mu.Lock()
			if target.proxy.archiveFences == nil {
				target.proxy.archiveFences = map[string]*nativeArchiveFence{}
			}
			target.proxy.archiveFences[command.ID] = &nativeArchiveFence{project: job.ProjectPath, defaultCWD: target.runtime.DefaultCWD, known: map[string]bool{}, threads: archiveThreadTree(job)}
			target.proxy.mu.Unlock()
		}
	}
	for _, target := range targets {
		inventory, err := target.client.ArchiveInventory(ctx, 10000)
		if err != nil {
			return fail(archiveBusy("The native project inventory could not be verified."))
		}
		loaded, complete, err := loadedThreadIDs(ctx, target.client)
		if err != nil || !complete {
			return fail(archiveBusy("Loaded native sessions could not be verified."))
		}
		metadata := map[string]codexadapter.Thread{}
		for _, thread := range inventory {
			metadata[thread.ID] = thread
		}
		for id := range loaded {
			thread, err := target.client.ReadThreadState(ctx, id)
			if err != nil {
				return fail(err)
			}
			metadata[id] = thread
		}
		fence := &nativeArchiveFence{project: job.ProjectPath, defaultCWD: target.runtime.DefaultCWD, known: map[string]bool{}, threads: archiveThreadTree(job)}
		for id, thread := range metadata {
			fence.known[id] = true
			if pathsOverlap(thread.CWD, job.ProjectPath) {
				fence.threads[id] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for id, thread := range metadata {
				if fence.threads[thread.ParentThreadID] && !fence.threads[id] {
					fence.threads[id], changed = true, true
				}
			}
		}
		a.manager.mu.Lock()
		a.manager.archiveScopes[command.ID][target.runtime.ID] = fence
		a.manager.mu.Unlock()
		if target.proxy != nil {
			target.proxy.mu.Lock()
			target.proxy.archiveFences[command.ID] = fence
			err = target.proxy.checkArchiveIdleLocked(fence)
			target.proxy.mu.Unlock()
			if err != nil {
				return fail(err)
			}
		}
		a.updateMu.Lock()
		if admission := a.archiveAdmissions[command.ID]; admission != nil {
			admission.Threads[target.runtime.ID] = fence.threads
		}
		a.updateMu.Unlock()
		for id := range fence.threads {
			if _, exists := metadata[id]; !exists {
				// On import the original native session may be absent entirely.
				if restoring || job.NativeArchived || target.runtime.ID != command.RuntimeID {
					continue
				}
			}
			if err := target.client.VerifyThreadQueueIdle(ctx, id); err != nil {
				return fail(archiveBusy("This project has queued native input or its queue cannot be verified."))
			}
			// Codex keeps background terminals in its live thread manager;
			// this RPC rejects persisted threads which are not loaded. The
			// complete inventory above was read behind the project fence.
			// A skipped live-only check still needs fresh cold-thread proof
			// below, after the queue and goal checks.
			if loaded[id] {
				if err := target.client.VerifyThreadBackgroundTerminalsIdle(ctx, id); err != nil {
					return fail(archiveBusy("This project has a native background terminal or it cannot be verified."))
				}
			}
			goal, err := target.client.ReadGoal(ctx, id)
			if err != nil {
				return fail(archiveBusy("The native session goal could not be verified."))
			}
			if goal != nil && goal.Status != "paused" && goal.Status != "complete" && goal.Status != "blocked" {
				return fail(archiveBusy("Pause or complete the native session goal before archiving this project."))
			}
			thread, err := target.client.ReadThreadState(ctx, id)
			if err != nil {
				return fail(err)
			}
			if !loaded[id] && thread.Status != "notLoaded" && thread.Status != "not_loaded" {
				return fail(archiveBusy("A native project session changed during archive idle verification. Retry when it has settled."))
			}
			if thread.ActiveTurnID != "" || (thread.Status != "idle" && thread.Status != "notLoaded" && thread.Status != "not_loaded" && thread.Status != "systemError") {
				return fail(archiveBusy("This project has an active native session."))
			}
		}
	}
	sessions, err := a.store.ListSessions("")
	if err != nil {
		return fail(err)
	}
	for _, session := range sessions {
		if session.Deleted || !pathsOverlap(session.CWD, job.ProjectPath) {
			continue
		}
		if !updateSessionIdle(session) {
			return fail(archiveBusy("This project has an active session or pending response."))
		}
		if actor := a.actorForThread(session.RuntimeID, session.ThreadID); actor != nil {
			reply := make(chan bool, 1)
			select {
			case actor.updateChecks <- reply:
			case <-ctx.Done():
				return fail(ctx.Err())
			}
			select {
			case idle := <-reply:
				if !idle {
					return fail(archiveBusy("This project has queued or pending gateway work."))
				}
			case <-ctx.Done():
				return fail(ctx.Err())
			}
		}
	}
	defaultCWD := map[string]string{}
	for _, target := range targets {
		defaultCWD[target.runtime.ID] = target.runtime.DefaultCWD
	}
	if err := a.store.checkArchivePending(command.ID, job.ProjectPath, defaultCWD); err != nil {
		return fail(err)
	}
	for _, target := range targets {
		if target.proxy != nil {
			target.proxy.mu.Lock()
			err = target.proxy.checkArchiveIdleLocked(target.proxy.archiveFences[command.ID])
			target.proxy.mu.Unlock()
			if err != nil {
				return fail(err)
			}
		}
	}
	return cleanup, nil
}

func (a *Agent) verifyProjectRemoval(job sessionArchiveJob, codexHome, sqliteHome string) error {
	path := cleanArchivePath(job.ProjectPath)
	if path == "" {
		return errors.New("project removal requires an absolute directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	protected := []string{"/", home, codexHome, sqliteHome, filepath.Dir(a.cfg.StateFile), filepath.Dir(a.cfg.TokenFile), job.Directory}
	protected = append(protected, a.cfg.AllowedWorkspaceRoots...)
	for _, profile := range a.cfg.Runtimes {
		protected = append(protected, profile.WorkingDirectory)
	}
	for _, root := range protected {
		root = cleanArchivePath(root)
		if root != "" && (path == root || strings.HasPrefix(root, path+string(filepath.Separator))) {
			return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "This project contains a protected worker, runtime, home, or workspace root and cannot be removed."}
		}
	}
	sessions, err := a.store.ListSessions("")
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if !session.Deleted && session.ThreadID != job.Session.ThreadID && archivePathContains(path, session.CWD) {
			return &protocol.Error{Code: "shared_directory", Message: "Another session shares or overlaps this project directory. Archive it without removing the project."}
		}
	}
	// Gateway discovery intentionally excludes some native sessions. They
	// still prevent removal of a directory which another user thread owns.
	for _, runtime := range a.manager.Snapshot() {
		client, _, ok := a.manager.Client(runtime.ID)
		if !ok {
			continue
		}
		threads, err := client.ArchiveInventory(a.ctx, 10000)
		if err != nil {
			return err
		}
		for _, thread := range threads {
			if thread.ID != job.Session.ThreadID && thread.UserSession() && archivePathContains(path, thread.CWD) {
				return &protocol.Error{Code: "shared_directory", Message: "Another native session shares or overlaps this project directory. Archive it without removing the project."}
			}
		}
	}
	return nil
}

func (a *Agent) verifyArchiveRestoreDestination(job sessionArchiveJob, codexHome, sqliteHome string) error {
	path := cleanArchivePath(job.ProjectPath)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	home = cleanArchivePath(home)
	if path == "" || path == "/" || path == home || (home != "" && strings.HasPrefix(home, path+string(filepath.Separator))) {
		return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "Archives cannot restore over the filesystem or user home root."}
	}
	protected := []string{codexHome, sqliteHome, job.Directory, filepath.Dir(a.cfg.StateFile), filepath.Dir(a.cfg.TokenFile), filepath.Join(home, ".ssh"), filepath.Join(home, ".config"), filepath.Join(home, ".codex")}
	for _, root := range protected {
		if pathsOverlap(path, root) {
			return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The archive project overlaps protected worker or user configuration data."}
		}
	}
	return nil
}
