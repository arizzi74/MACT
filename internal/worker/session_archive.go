package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/sessionarchive"
)

const archiveListPageSize = 25

type sessionArchiveOperations struct {
	compatible func(string) error
	export     func(context.Context, sessionarchive.ExportOptions) (sessionarchive.Manifest, error)
	read       func(context.Context, string) (sessionarchive.Manifest, error)
	restore    func(context.Context, sessionarchive.RestoreOptions) (sessionarchive.Manifest, error)
	verify     func(context.Context, string, sessionarchive.VerifyOptions) (sessionarchive.Manifest, error)
}

func (a *Agent) sessionArchiveEngine() sessionArchiveOperations {
	if a.archiveOperations != nil {
		return *a.archiveOperations
	}
	return sessionArchiveOperations{compatible: sessionarchive.CheckCompatibility, export: sessionarchive.Export, read: sessionarchive.Read, restore: sessionarchive.Restore, verify: sessionarchive.Verify}
}

func sessionArchiveDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "CODEX_Archived")
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("archive directory is not a regular directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return dir, nil
}

func archiveFilename(session protocol.Session, commandID string) string {
	var name strings.Builder
	for _, r := range session.Name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			name.WriteRune(r)
		} else if name.Len() > 0 && !strings.HasSuffix(name.String(), "-") {
			name.WriteByte('-')
		}
		if name.Len() >= 48 {
			break
		}
	}
	slug := strings.Trim(name.String(), "-_")
	if slug == "" {
		slug = "session"
	}
	return fmt.Sprintf("%s-%s-%s.tar.xz", slug, time.Now().UTC().Format("20060102T150405Z"), strings.ReplaceAll(commandID, "-", ""))
}

// dispatchSessionArchive runs under command admission. Accepted jobs use the
// worker lifetime, so gateway/browser disconnects cannot abandon a backup.
func (a *Agent) dispatchSessionArchive(c protocol.Command) (protocol.CommandAck, error) {
	dir, err := sessionArchiveDirectory()
	if err != nil {
		return a.reject(c, protocol.InvalidWorkspace, "The worker archive directory is unavailable.")
	}
	if c.Operation == protocol.ListSessionArchives {
		if err := a.store.SetCommandState(c.ID, CommandExecuting, nil); err != nil {
			return protocol.CommandAck{}, err
		}
		a.group.Add(1)
		go func() {
			defer a.group.Done()
			result, err := listSessionArchives(a.ctx, dir, c.Arguments.SessionArchive.Offset)
			if err != nil {
				a.report(a.executionError(c, &protocol.Error{Code: protocol.InternalError, Message: "Session archives could not be listed."}))
				return
			}
			result.CommandID, result.State = c.ID, "completed"
			_, err = a.record(c, CommandCompleted, &result, "command_completed")
			a.report(err)
		}()
		return protocol.CommandAck{CommandID: c.ID, Status: "accepted"}, nil
	}
	job := sessionArchiveJob{Directory: dir, Phase: "preparing"}
	sessions, err := a.store.ListSessions(c.RuntimeID)
	if err != nil {
		return protocol.CommandAck{}, err
	}
	if c.Operation == protocol.ArchiveSession {
		for _, session := range sessions {
			if session.ID == c.SessionID && session.ThreadID == c.ThreadID {
				job.Session = session
				break
			}
		}
		if job.Session.ID == "" || job.Session.Archived || job.Session.Deleted {
			return a.reject(c, protocol.UnknownSession, "The selected session is no longer available.")
		}
		if !updateSessionIdle(job.Session) {
			return a.reject(c, protocol.SessionBusy, "Wait for the selected session to finish before archiving it.")
		}
		cwd, err := auth.CanonicalWorkspace(job.Session.CWD, a.cfg.AllowedWorkspaceRoots)
		if err != nil || c.Arguments.CWD != job.Session.CWD {
			return a.reject(c, protocol.InvalidWorkspace, "The selected project directory changed. Select the session again.")
		}
		job.ProjectPath, job.ArchiveID = cwd, archiveFilename(job.Session, c.ID)
		if info, err := os.Lstat(cwd); err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				job.ProjectDevice, job.ProjectInode = uint64(stat.Dev), uint64(stat.Ino)
			}
		} else {
			return a.reject(c, protocol.InvalidWorkspace, "The selected project directory is unavailable.")
		}
		if info, err := os.Stat(filepath.Dir(cwd)); err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				job.ParentDevice, job.ParentInode = uint64(stat.Dev), uint64(stat.Ino)
			}
		} else {
			return a.reject(c, protocol.InvalidWorkspace, "The selected project parent is unavailable.")
		}
		job.RemovalPath = filepath.Join(filepath.Dir(cwd), ".codex-archived-"+c.ID)
	} else {
		job.ArchiveID = c.Arguments.SessionArchive.ArchiveID
		if origin, err := a.store.archiveOrigin(job.ArchiveID); err != nil {
			return protocol.CommandAck{}, err
		} else if origin != nil {
			job.CodexHome, job.SQLiteHome = origin.CodexHome, origin.SQLiteHome
			job.ProjectPath = origin.ProjectPath
			if origin.Session.RuntimeID == c.RuntimeID {
				job.Session = origin.Session
			}
		}
	}
	for _, admission := range a.archiveAdmissions {
		if pathsOverlap(admission.Project, job.ProjectPath) {
			return a.reject(c, protocol.SessionBusy, "This project already has an archive or restore job running.")
		}
	}
	if err := a.store.saveArchiveJob(c, job); err != nil {
		return protocol.CommandAck{}, err
	}
	a.startArchiveJob(c, job)
	return protocol.CommandAck{CommandID: c.ID, Status: "accepted"}, nil
}

// Full XZ/checksum validation is deliberately outside updateMu. A first-time
// restore learns its project scope before installing the project admission
// barrier, then rechecks native/gateway idleness behind that barrier.
func (a *Agent) prepareRestoreJob(ctx context.Context, c protocol.Command, job *sessionArchiveJob) error {
	manifest, err := a.sessionArchiveEngine().read(ctx, filepath.Join(job.Directory, job.ArchiveID))
	if err != nil {
		return &protocol.Error{Code: protocol.ArchiveInvalid, Message: "The selected archive could not be validated."}
	}
	if manifest.ArchiveID != job.ArchiveID || (job.Session.ThreadID != "" && job.Session.ThreadID != manifest.ThreadID) {
		return &protocol.Error{Code: protocol.ArchiveInvalid, Message: "The selected archive identity changed."}
	}
	_, runtime, available := a.manager.Client(c.RuntimeID)
	if !available || runtime.State != "running" {
		return &protocol.Error{Code: protocol.CodexUnavailable, Message: "The archive runtime is not available. Retry after it has started."}
	}
	// Recovery can resume after importing files but before native unarchive.
	// Refuse a different runtime before changing the saved identity or issuing
	// any lifecycle RPC, even when the import phase was already completed.
	if err := sessionarchive.CheckVersionCompatibility(manifest.CodexVersion, runtime.CodexVersion); err != nil {
		return err
	}
	if job.ProjectPath != "" && job.ProjectPath != manifest.OriginalPath {
		return &protocol.Error{Code: protocol.ArchiveInvalid, Message: "The selected archive project path changed."}
	}
	if !archiveRestorePathAllowed(manifest.OriginalPath, a.cfg.AllowedWorkspaceRoots) {
		return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The archive's original project path is outside this worker's allowed roots."}
	}
	sessions, err := a.store.ListSessions(c.RuntimeID)
	if err != nil {
		return err
	}
	found := false
	for _, session := range sessions {
		if session.ThreadID == manifest.ThreadID {
			job.Session = session
			found = true
			break
		}
	}
	if job.Session.Deleted {
		return &protocol.Error{Code: protocol.ArchiveConflict, Message: "This original session was permanently deleted and cannot reuse that gateway identity."}
	}
	job.ProjectPath = manifest.OriginalPath
	job.ThreadIDs = append([]string{}, manifest.ThreadIDs...)
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	for id, admission := range a.archiveAdmissions {
		if id != c.ID && pathsOverlap(admission.Project, job.ProjectPath) {
			return archiveBusy("This project already has an archive or restore job running.")
		}
	}
	job.PendingIdentity = !found
	if !found {
		id := job.Session.ID
		if id == "" {
			id = uuid.NewString()
		}
		job.Session = protocol.Session{ID: id, WorkerID: a.cfg.WorkerID, RuntimeID: c.RuntimeID, ThreadID: manifest.ThreadID, Name: manifest.DisplayName, CWD: manifest.OriginalPath, State: "not_loaded", Archived: true}
	}
	if admission := a.archiveAdmissions[c.ID]; admission != nil {
		admission.Project, admission.ThreadID = job.ProjectPath, job.Session.ThreadID
	}
	return a.store.saveArchiveJob(c, *job)
}

// Missing restore destinations are admitted by checking their existing parent;
// symlink resolution may never move a restore outside the worker allowlist.
func archiveRestorePathAllowed(path string, roots []string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return false
	}
	for candidate := path; candidate != "/"; candidate = filepath.Dir(candidate) {
		if _, err := os.Lstat(candidate); err == nil {
			if _, err := auth.CanonicalWorkspace(candidate, roots); err == nil {
				return true
			}
			return false
		} else if !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return false
}

func (a *Agent) startArchiveJob(c protocol.Command, job sessionArchiveJob) {
	if a.archiveAdmissions == nil {
		a.archiveAdmissions = map[string]*archiveAdmission{}
	}
	a.archiveAdmissions[c.ID] = &archiveAdmission{Project: job.ProjectPath, ThreadID: job.Session.ThreadID, Threads: map[string]map[string]bool{}}
	a.group.Add(1)
	go func() {
		defer a.group.Done()
		defer func() { a.updateMu.Lock(); delete(a.archiveAdmissions, c.ID); a.updateMu.Unlock() }()
		result, err := a.runArchiveJob(c, &job)
		if errors.Is(err, context.Canceled) && a.ctx.Err() != nil {
			return
		} // journal remains resumable
		if err != nil {
			var commandError *protocol.Error
			if !errors.As(err, &commandError) {
				code, message := protocol.ArchiveInvalid, "The session archive operation could not be completed. Its archive and source files were retained."
				if errors.Is(err, sessionarchive.ErrConflict) {
					code, message = protocol.ArchiveConflict, "The restore destination or native session already contains different data. Resolve the conflict before retrying; nothing was overwritten."
				}
				if errors.Is(err, sessionarchive.ErrUnsupported) {
					code, message = protocol.CodexMethodUnsupported, "This native Codex version or session layout cannot be archived safely."
				}
				err = &protocol.Error{Code: code, Message: message}
			}
			a.report(a.executionError(c, err))
			return
		}
		result.CommandID, result.State = c.ID, "completed"
		_, err = a.record(c, CommandCompleted, &result, "command_completed")
		a.report(err)
	}()
}

func (a *Agent) recoverArchiveJobs() error {
	records, err := a.store.archiveJobs()
	if err != nil {
		return err
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	for _, record := range records {
		job := *record.ArchiveJob
		job.Phase = "recovering"
		if err := a.store.saveArchiveJob(record.Command, job); err != nil {
			return err
		}
		a.startArchiveJob(record.Command, job)
	}
	return nil
}

func (a *Agent) runArchiveJob(c protocol.Command, job *sessionArchiveJob) (protocol.Result, error) {
	ctx := a.ctx
	if c.Operation == protocol.RestoreSession {
		if err := a.prepareRestoreJob(ctx, c, job); err != nil {
			return protocol.Result{}, err
		}
	}
	client, runtime, ok := a.manager.Client(c.RuntimeID)
	if !ok || runtime.State != "running" {
		return protocol.Result{}, &protocol.Error{Code: protocol.CodexUnavailable, Message: "The archive runtime is not available. Retry after it has started."}
	}
	if err := a.sessionArchiveEngine().compatible(runtime.CodexVersion); err != nil {
		return protocol.Result{}, err
	}
	info, ready := client.InitializeInfo()
	if !ready || info.CodexHome == "" {
		return protocol.Result{}, &protocol.Error{Code: protocol.CodexProtocolError, Message: "Codex did not report its local data directory; native archival cannot be verified."}
	}
	if job.CodexHome != "" && cleanArchivePath(job.CodexHome) != cleanArchivePath(info.CodexHome) {
		return protocol.Result{}, &protocol.Error{Code: protocol.ArchiveConflict, Message: "The native Codex data directory changed since this archive was created."}
	}
	job.CodexHome = info.CodexHome
	sqliteHome := job.SQLiteHome
	if sqliteHome == "" {
		configCWD := runtime.DefaultCWD
		for {
			if _, statErr := os.Stat(configCWD); statErr == nil {
				break
			}
			parent := filepath.Dir(configCWD)
			if parent == configCWD {
				return protocol.Result{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The runtime configuration directory is unavailable."}
			}
			configCWD = parent
		}
		config, err := client.ReadEffectiveConfig(ctx, configCWD)
		if err != nil {
			return protocol.Result{}, err
		}
		sqliteHome = config.SQLiteHome
		if sqliteHome == "" {
			sqliteHome = info.CodexHome
		}
		job.SQLiteHome = sqliteHome
	}
	if c.Operation == protocol.RestoreSession {
		if err := a.verifyArchiveRestoreDestination(*job, info.CodexHome, sqliteHome); err != nil {
			return protocol.Result{}, err
		}
	}
	profileBinary := ""
	for _, profile := range a.cfg.Runtimes {
		if profile.ID == runtime.ProfileID {
			profileBinary = profile.CodexBinary
			break
		}
	}
	if profileBinary == "" {
		profileBinary = "codex"
	}
	if c.Operation == protocol.ArchiveSession && job.Exported && !job.NativeArchived {
		archived, err := client.ThreadArchived(ctx, job.Session.ThreadID)
		if err != nil {
			return protocol.Result{}, err
		}
		if archived {
			job.NativeArchived = true
			if err := a.store.saveArchiveJob(c, *job); err != nil {
				return protocol.Result{}, err
			}
		}
	}
	phase := func(name string) error { job.Phase = name; return a.store.saveArchiveJob(c, *job) }
	var progressErr error
	progress := func(name string) {
		lower := strings.ToLower(name)
		switch {
		case strings.Contains(lower, "compress"):
			name = "compressing"
		case strings.Contains(lower, "verif"), strings.Contains(lower, "checksum"):
			name = "verifying"
		case strings.Contains(lower, "restor"):
			name = "restoring"
		case strings.Contains(lower, "export"), strings.Contains(lower, "snapshot"):
			name = "exporting"
		}
		switch name {
		case "preparing", "exporting", "compressing", "verifying", "archiving", "cleaning", "extracting", "restoring", "completed", "recovering":
		default:
			return
		}
		if progressErr == nil {
			progressErr = phase(name)
		}
	}
	guardCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	release, err := a.projectArchiveGuard(guardCtx, c, *job, c.Operation == protocol.RestoreSession)
	cancel()
	if err != nil {
		return protocol.Result{}, err
	}
	defer release()
	if c.Operation == protocol.RestoreSession && job.PendingIdentity {
		// Only create/pin a new gateway identity after native idle proof, and
		// before any imported native records can become visible to discovery.
		job.Session, err = a.store.ConfirmArchiveVisibility(runtime, job.Session, job.ArchiveID, true)
		if err != nil {
			return protocol.Result{}, err
		}
		job.PendingIdentity = false
		if err := phase("restoring"); err != nil {
			return protocol.Result{}, err
		}
	}
	filename := filepath.Join(job.Directory, job.ArchiveID)
	if c.Operation == protocol.ArchiveSession {
		if !job.NativeArchived {
			thread, err := client.ReadThreadState(ctx, job.Session.ThreadID)
			if err != nil {
				return protocol.Result{}, err
			}
			cwd, err := auth.CanonicalWorkspace(thread.CWD, a.cfg.AllowedWorkspaceRoots)
			if err != nil || cwd != job.ProjectPath || !thread.UserSession() {
				return protocol.Result{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The original session project changed before archive export."}
			}
		}
		if c.Arguments.SessionArchive.DeleteProject && !job.ProjectRemoved {
			if err := a.verifyProjectRemoval(*job, info.CodexHome, sqliteHome); err != nil {
				return protocol.Result{}, err
			}
		}
		var manifest sessionarchive.Manifest
		if !job.Exported {
			if err := phase("exporting"); err != nil {
				return protocol.Result{}, err
			}
			manifest, err = a.sessionArchiveEngine().export(ctx, sessionarchive.ExportOptions{CodexHome: info.CodexHome, SQLiteHome: sqliteHome, ThreadID: job.Session.ThreadID, ProjectPath: job.ProjectPath, ArchiveDir: job.Directory, ArchiveID: job.ArchiveID, DisplayName: job.Session.Name, CodexVersion: runtime.CodexVersion, Progress: progress})
			if err != nil {
				return protocol.Result{}, err
			}
			if progressErr != nil {
				return protocol.Result{}, progressErr
			}
			job.Exported = true
			job.ThreadIDs = append([]string{}, manifest.ThreadIDs...)
			if err := phase("verifying"); err != nil {
				return protocol.Result{}, err
			}
		} else {
			manifest, err = a.sessionArchiveEngine().read(ctx, filename)
			if err != nil {
				return protocol.Result{}, err
			}
		}
		checkManifest := func(m sessionarchive.Manifest) error {
			if m.ThreadID != job.Session.ThreadID || m.OriginalPath != job.ProjectPath {
				return &protocol.Error{Code: protocol.ArchiveInvalid, Message: "The archive manifest does not match the selected session."}
			}
			// A restart can resume a previously exported job under a newer
			// native runtime. Check both the initial and verified manifests
			// before archiving the conversation or removing its project.
			return sessionarchive.CheckVersionCompatibility(m.CodexVersion, runtime.CodexVersion)
		}
		if err := checkManifest(manifest); err != nil {
			return protocol.Result{}, err
		}
		manifest, err = a.sessionArchiveEngine().verify(ctx, filename, sessionarchive.VerifyOptions{CodexBinary: profileBinary, Progress: progress})
		if err != nil || !manifest.VerifiedSelfContained {
			if err == nil {
				err = sessionarchive.ErrUnsupported
			}
			return protocol.Result{}, err
		}
		if err := checkManifest(manifest); err != nil {
			return protocol.Result{}, err
		}
		if !job.NativeArchived {
			if err := phase("archiving"); err != nil {
				return protocol.Result{}, err
			}
			if err := client.ArchiveThread(ctx, job.Session.ThreadID); err != nil {
				return protocol.Result{}, err
			}
			job.NativeArchived = true
			if err := phase("archiving"); err != nil {
				return protocol.Result{}, err
			}
		}
		saved, err := a.store.ConfirmArchiveVisibility(runtime, job.Session, job.ArchiveID, true)
		if err != nil {
			return protocol.Result{}, err
		}
		if err := a.onSessionContext(ctx, runtime, saved); err != nil {
			return protocol.Result{}, err
		}
		cleanupWarning := ""
		retainedPath := ""
		if c.Arguments.SessionArchive.DeleteProject && !job.ProjectRemoved {
			if err := phase("cleaning"); err != nil {
				return protocol.Result{}, err
			}
			cleanupErr := a.verifyProjectRemoval(*job, info.CodexHome, sqliteHome)
			if cleanupErr == nil {
				cleanupErr = removeArchivedProject(ctx, *job, manifest, func() error { job.CleanupVerified = true; return phase("cleaning") })
			}
			if cleanupErr != nil {
				if ctx.Err() != nil {
					return protocol.Result{}, ctx.Err()
				}
				cleanupWarning = "project_cleanup_failed"
				retainedPath = retainedArchiveProjectPath(*job)
			} else {
				job.ProjectRemoved = true
			}
			if err := phase("cleaning"); err != nil {
				return protocol.Result{}, err
			}
		}
		entry, err := archiveEntry(filename, manifest)
		if err != nil {
			return protocol.Result{}, err
		}
		return protocol.Result{Session: &saved, Text: "Session and project archived.", SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: job.Directory, Archive: &entry, ProjectDeleted: job.ProjectRemoved, WarningCode: cleanupWarning, RetainedProjectPath: retainedPath}}, nil
	}
	if err := phase("extracting"); err != nil {
		return protocol.Result{}, err
	}
	var restoredManifest sessionarchive.Manifest
	if !job.Imported {
		restoredManifest, err = a.sessionArchiveEngine().restore(ctx, sessionarchive.RestoreOptions{ArchivePath: filename, CodexHome: info.CodexHome, SQLiteHome: sqliteHome, Destination: job.ProjectPath, CodexVersion: runtime.CodexVersion, Progress: progress})
		if err != nil {
			return protocol.Result{}, err
		}
		if progressErr != nil {
			return protocol.Result{}, progressErr
		}
		job.Imported = true
		if err := phase("restoring"); err != nil {
			return protocol.Result{}, err
		}
	} else {
		restoredManifest, err = a.sessionArchiveEngine().read(ctx, filename)
		if err != nil {
			return protocol.Result{}, err
		}
	}
	if restoredManifest.ArchiveID != job.ArchiveID || restoredManifest.ThreadID != job.Session.ThreadID || restoredManifest.OriginalPath != job.ProjectPath {
		return protocol.Result{}, &protocol.Error{Code: protocol.ArchiveInvalid, Message: "The selected archive identity changed."}
	}
	if err := sessionarchive.CheckVersionCompatibility(restoredManifest.CodexVersion, runtime.CodexVersion); err != nil {
		return protocol.Result{}, err
	}
	archivedIDs, err := client.ArchivedThreadIDs(ctx)
	if err != nil {
		return protocol.Result{}, err
	}
	archivedSet := map[string]bool{}
	for _, id := range archivedIDs {
		archivedSet[id] = true
	}
	threadTree := archiveThreadTree(*job)
	for id := range threadTree {
		if id != job.Session.ThreadID && archivedSet[id] {
			if err := client.UnarchiveThread(ctx, id); err != nil {
				return protocol.Result{}, err
			}
		}
	}
	if archivedSet[job.Session.ThreadID] {
		if err := client.UnarchiveThread(ctx, job.Session.ThreadID); err != nil {
			return protocol.Result{}, err
		}
	}
	thread, err := client.ResumeThread(ctx, job.Session.ThreadID, codexadapter.ThreadOptions{})
	if err != nil {
		return protocol.Result{}, err
	}
	if thread.ID != job.Session.ThreadID || cleanArchivePath(thread.CWD) != cleanArchivePath(job.ProjectPath) || !thread.UserSession() {
		return protocol.Result{}, &protocol.Error{Code: protocol.ArchiveInvalid, Message: "Codex did not resume the original restored session in its original project."}
	}
	restored := sessionFromThread(runtime, thread, true)
	restored.ID, restored.Name, restored.Stats = job.Session.ID, job.Session.Name, job.Session.Stats
	saved, err := a.store.ConfirmArchiveVisibility(runtime, restored, job.ArchiveID, false)
	if err != nil {
		return protocol.Result{}, err
	}
	if err := a.onSessionContext(ctx, runtime, saved); err != nil {
		return protocol.Result{}, err
	}
	manifest, err := a.sessionArchiveEngine().read(ctx, filename)
	if err != nil {
		return protocol.Result{}, err
	}
	entry, err := archiveEntry(filename, manifest)
	if err != nil {
		return protocol.Result{}, err
	}
	return protocol.Result{Session: &saved, Text: "Original Codex session restored and resumed. The archive was kept.", SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: job.Directory, Archive: &entry}}, nil
}

// Rename the verified source to an adjacent private tombstone before removal.
// Refuse a replacement symlink or a changed canonical path at the final step.
type archiveCleanupHooks struct {
	rename      func(*os.File, string, string) error
	checkpoint  func() error
	afterRemove func(string) error
}

func retainedArchiveProjectPath(job sessionArchiveJob) string {
	parent, err := os.Stat(filepath.Dir(job.ProjectPath))
	if err != nil {
		return ""
	}
	stat, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != job.ParentDevice || uint64(stat.Ino) != job.ParentInode {
		return ""
	}
	for _, path := range []string{job.RemovalPath, job.ProjectPath} {
		if path != "" {
			if _, err := os.Lstat(path); err == nil {
				return path
			}
		}
	}
	return ""
}

func removeArchivedProject(ctx context.Context, job sessionArchiveJob, manifest sessionarchive.Manifest, checkpoints ...func() error) error {
	hooks := archiveCleanupHooks{rename: renameArchiveProject}
	if len(checkpoints) > 0 {
		hooks.checkpoint = checkpoints[0]
	}
	return removeArchivedProjectWithHooks(ctx, job, manifest, hooks)
}

func removeArchivedProjectWithRename(ctx context.Context, job sessionArchiveJob, manifest sessionarchive.Manifest, rename func(*os.File, string, string) error) error {
	return removeArchivedProjectWithHooks(ctx, job, manifest, archiveCleanupHooks{rename: rename})
}

func removeArchivedProjectWithHooks(ctx context.Context, job sessionArchiveJob, manifest sessionarchive.Manifest, hooks archiveCleanupHooks) error {
	path, tombstone := job.ProjectPath, job.RemovalPath
	if tombstone == "" || filepath.Dir(tombstone) != filepath.Dir(path) || !strings.HasPrefix(filepath.Base(tombstone), ".codex-archived-") || job.ProjectInode == 0 || job.ParentInode == 0 {
		return errors.New("archive project cleanup identity is missing")
	}
	matching := func(info os.FileInfo) bool {
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && uint64(stat.Dev) == job.ProjectDevice && uint64(stat.Ino) == job.ProjectInode
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	parentInfo, err := parent.Stat(".")
	if err != nil {
		return err
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || uint64(parentStat.Dev) != job.ParentDevice || uint64(parentStat.Ino) != job.ParentInode {
		return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The project parent changed before cleanup; its files were kept."}
	}
	leaf, tombLeaf := filepath.Base(path), filepath.Base(tombstone)
	removeTombstone := func() error {
		// Pin the renamed directory itself. Removing its children through this
		// handle cannot follow a replacement of the parent path or tombstone.
		root, err := parent.OpenRoot(tombLeaf)
		if err != nil {
			return err
		}
		defer root.Close()
		info, err := root.Stat(".")
		if err != nil {
			return err
		}
		if !matching(info) {
			return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The project changed during cleanup; its files were kept."}
		}
		if err := sessionarchive.ProjectMatches(ctx, root, manifest, job.CleanupVerified); err != nil {
			// The archived project changed after export. Put the intact staged
			// directory back when its original name is still available.
			if handle, openErr := parent.Open("."); !job.CleanupVerified && openErr == nil {
				_ = renameArchiveProject(handle, tombLeaf, leaf)
				handle.Close()
			} else if openErr == nil {
				handle.Close()
			}
			return err
		}
		if !job.CleanupVerified && hooks.checkpoint != nil {
			if err := hooks.checkpoint(); err != nil {
				return err
			}
		}
		job.CleanupVerified = true
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, err := dir.ReadDir(-1)
		dir.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := root.RemoveAll(entry.Name()); err != nil {
				return err
			}
			if hooks.afterRemove != nil {
				if err := hooks.afterRemove(entry.Name()); err != nil {
					return err
				}
			}
		}
		info, err = parent.Lstat(tombLeaf)
		if err != nil {
			return err
		}
		if !matching(info) {
			return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The cleanup staging path changed; its files were kept."}
		}
		return parent.Remove(tombLeaf)
	}
	if info, err := parent.Lstat(tombLeaf); err == nil {
		if !matching(info) {
			return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The cleanup staging path changed; its files were kept."}
		}
		return removeTombstone()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := parent.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !matching(info) {
		return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The project path changed before cleanup; its files were kept."}
	}
	original, err := parent.OpenRoot(leaf)
	if err != nil {
		return err
	}
	if err := sessionarchive.ProjectMatches(ctx, original, manifest); err != nil {
		original.Close()
		return err
	}
	original.Close()
	parentHandle, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer parentHandle.Close()
	if err := hooks.rename(parentHandle, leaf, tombLeaf); err != nil {
		return err
	}
	// A source replacement between the first check and the atomic rename is
	// preserved intact. Never erase a directory whose identity was not backed up.
	info, err = parent.Lstat(tombLeaf)
	if err != nil {
		return err
	}
	if !matching(info) {
		return &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The project changed during cleanup; its files were kept."}
	}
	return removeTombstone()
}

func archiveEntry(path string, manifest sessionarchive.Manifest) (protocol.SessionArchiveEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return protocol.SessionArchiveEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return protocol.SessionArchiveEntry{}, errors.New("archive is not a regular file")
	}
	return protocol.SessionArchiveEntry{ID: filepath.Base(path), Name: filepath.Base(path), SessionName: manifest.DisplayName, OriginalPath: manifest.OriginalPath, CreatedAt: manifest.CreatedAt, Bytes: info.Size(), CodexVersion: manifest.CodexVersion}, nil
}

func listSessionArchives(ctx context.Context, directory string, offset int) (protocol.Result, error) {
	files, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Result{SessionArchive: &protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: directory, Archives: []protocol.SessionArchiveEntry{}}}, nil
	}
	if err != nil {
		return protocol.Result{}, err
	}
	var names []string
	for _, file := range files {
		if protocol.ValidSessionArchiveID(file.Name()) && file.Type().IsRegular() {
			names = append(names, file.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if offset > len(names) {
		offset = len(names)
	}
	end := offset + archiveListPageSize
	if end > len(names) {
		end = len(names)
	}
	result := protocol.SessionArchiveResult{Phase: "completed", ArchiveDirectory: directory, Archives: []protocol.SessionArchiveEntry{}}
	for _, name := range names[offset:end] {
		path := filepath.Join(directory, name)
		manifest, err := sessionarchive.ReadMetadata(ctx, path)
		if err != nil {
			continue
		} // an incomplete or foreign file is never offered for restore
		entry, err := archiveEntry(path, manifest)
		if err != nil {
			continue
		}
		result.Archives = append(result.Archives, entry)
	}
	if end < len(names) {
		result.NextOffset = &end
	}
	return protocol.Result{SessionArchive: &result}, nil
}
