package claudeadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const deletionJournalDirectory = ".deletions"
const maxDeletionJournalBytes = 1 << 20
const maxNativeProjectDirectories = 10000

// This intent/receipt deliberately contains no conversation, tool input,
// account settings, or credential. Done receipts make worker retries safe
// after native cleanup succeeded but its acknowledgement was lost.
type deletionRecord struct {
	Version           int      `json:"version"`
	ThreadID          string   `json:"threadId"`
	NativeConfigRoots []string `json:"nativeConfigRoots,omitempty"`
	ProtectedCWDs     []string `json:"protectedCwds,omitempty"`
	Done              bool     `json:"done"`
	invalid           bool
}

func deletionError(message string) error {
	return errors.New("Claude session deletion: " + message + " Retry deleting this session after resolving the problem; project files are kept.")
}

// Called with server.mu held. Native reaping and filesystem cleanup run after
// the caller releases that lock, so the reader can finish and close p.done.
func (s *server) beginDeletion(id string) (*deletionRecord, *process, error) {
	if !canonicalID(id) {
		return nil, nil, errors.New("Invalid Claude session ID")
	}
	if s.deleting[id] {
		return nil, nil, deletionError("a deletion is already in progress.")
	}
	if record := s.deletions[id]; record != nil {
		if record.invalid {
			return nil, nil, deletionError("the private deletion journal is unavailable or invalid.")
		}
		if record.Done {
			if len(record.NativeConfigRoots) > 0 || len(record.ProtectedCWDs) > 0 {
				minimal := completedDeletionRecord(record)
				if err := s.writeDeletionRecord(minimal); err != nil {
					return record, nil, err
				}
				s.deletions[id] = minimal
				record = minimal
			}
			return record, nil, nil
		}
		copy := *record
		copy.ProtectedCWDs = s.protectedDeletionCWDs(record.ProtectedCWDs)
		record = &copy
		s.deletions[id] = record
		if err := s.writeDeletionRecord(record); err != nil {
			return record, nil, err
		}
		s.deleting[id] = true
		p := s.runners[id]
		if p != nil {
			delete(s.runners, id)
			p.stop()
		}
		return record, p, nil
	}
	t := s.threads[id]
	if t == nil {
		return nil, nil, errors.New("Claude session was not found")
	}
	if t.active() != nil || len(t.Queue) != 0 {
		return nil, nil, deletionError("the session has an active turn or queued input.")
	}
	for _, pending := range s.pending {
		if pending.Process.threadID == id {
			return nil, nil, deletionError("the session has a pending approval or question.")
		}
	}
	roots := append([]string(nil), t.NativeConfigRoots...)
	if len(roots) == 0 {
		settings, err := s.settings()
		if err != nil {
			return nil, nil, err
		}
		env, err := childEnvironment(s.cfg.Env, settings.Env)
		if err != nil {
			return nil, nil, err
		}
		root, err := nativeConfigDirectory(env, t.CWD)
		if err != nil {
			return nil, nil, err
		}
		roots = []string{root}
	}
	protected := s.protectedDeletionCWDs(nil)
	record := &deletionRecord{Version: 1, ThreadID: id, NativeConfigRoots: roots, ProtectedCWDs: protected}
	if err := s.writeDeletionRecord(record); err != nil {
		s.deletions[id] = record
		return record, nil, err
	}
	s.deletions[id] = record
	s.deleting[id] = true
	p := s.runners[id]
	if p != nil {
		delete(s.runners, id)
		p.stop()
	}
	return record, p, nil
}

func (s *server) protectedDeletionCWDs(prior []string) []string {
	result := append([]string(nil), prior...)
	seen := make(map[string]bool, len(result))
	for _, cwd := range result {
		seen[cwd] = true
	}
	for _, thread := range s.threads {
		if !seen[thread.CWD] {
			result = append(result, thread.CWD)
			seen[thread.CWD] = true
		}
	}
	return result
}

func (s *server) deletionProtectsWorkspace(cwd string) bool {
	for _, record := range s.deletions {
		if record.Done {
			continue
		}
		for _, root := range record.NativeConfigRoots {
			relative, err := filepath.Rel(filepath.Join(root, "projects"), cwd)
			if err != nil || filepath.IsAbs(relative) {
				continue
			}
			components := strings.Split(relative, string(filepath.Separator))
			if len(components) >= 2 && components[0] != ".." && components[1] == record.ThreadID {
				return true
			}
		}
	}
	return false
}

func (s *server) runDeletion(peer *peer, id json.RawMessage, record *deletionRecord, p *process) {
	defer s.wg.Done()
	err := s.completeDeletion(record, p)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deleting, record.ThreadID)
	if err != nil {
		s.deletionErrors[record.ThreadID] = err.Error()
		rpcError(peer, id, -32010, err.Error())
		return
	}
	s.deletions[record.ThreadID] = completedDeletionRecord(record)
	delete(s.deletionErrors, record.ThreadID)
	delete(s.threads, record.ThreadID)
	rpcResult(peer, id, map[string]any{})
	s.broadcast("thread/deleted", map[string]any{"threadId": record.ThreadID})
}

func (s *server) completeDeletion(record *deletionRecord, p *process) error {
	if p != nil {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return deletionError("the owned Claude process has not exited yet.")
		}
	}
	if record.Done {
		return nil
	}
	if err := s.writeDeletionRecord(record); err != nil {
		return err
	}
	// Preflight every root before removing any target. No project directory,
	// memory, shared index, global history, credentials, or session registry
	// is a deletion target.
	var projects []*nativeDeletionProject
	defer func() {
		for _, project := range projects {
			project.root.Close()
		}
	}()
	for _, directory := range record.NativeConfigRoots {
		found, err := planNativeDeletion(directory, record.ThreadID, record.ProtectedCWDs)
		if err != nil {
			return err
		}
		projects = append(projects, found...)
	}
	for _, project := range projects {
		for _, target := range project.targets {
			current, err := project.root.Lstat(target.name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !os.SameFile(current, target.info) {
				return deletionError("native session artifacts changed during cleanup.")
			}
			if target.info.IsDir() {
				err = project.root.RemoveAll(target.name)
			} else {
				err = project.root.Remove(target.name)
			}
			if err != nil {
				return deletionError("native session artifacts could not be removed; check their permissions.")
			}
		}
		if err := syncRoot(project.root); err != nil {
			return deletionError("native session artifact removal could not be synced.")
		}
	}
	if err := s.removeAdapterHistory(record.ThreadID); err != nil {
		return err
	}
	return s.writeDeletionRecord(completedDeletionRecord(record))
}

func completedDeletionRecord(record *deletionRecord) *deletionRecord {
	return &deletionRecord{Version: 1, ThreadID: record.ThreadID, Done: true}
}

func (s *server) removeAdapterHistory(id string) error {
	state, err := openRealDirectory(s.cfg.StateDirectory)
	if err != nil {
		return deletionError("adapter session storage is unavailable.")
	}
	defer state.Close()
	name := id + ".json"
	if info, err := state.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return deletionError("the adapter session file is not a regular file.")
		}
		if err := state.Remove(name); err != nil {
			return deletionError("the adapter session file could not be removed; check its permissions.")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return deletionError("the adapter session file is unavailable.")
	}
	if err := syncRoot(state); err != nil {
		return deletionError("adapter session removal could not be synced.")
	}
	return nil
}

func syncRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func openRealDirectory(path string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("directory changed while opening")
	}
	return root, nil
}

func openRealChild(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("not a real directory")
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		child.Close()
		return nil, errors.New("directory changed while opening")
	}
	return child, nil
}

type nativeDeletionTarget struct {
	name string
	info os.FileInfo
}
type nativeDeletionProject struct {
	root    *os.Root
	targets []nativeDeletionTarget
}

func planNativeDeletion(directory, id string, protected []string) (result []*nativeDeletionProject, err error) {
	var openedPlans []*nativeDeletionProject
	if !canonicalID(id) || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(filepath.Separator) {
		return nil, deletionError("the native session storage path is invalid.")
	}
	physical, resolveErr := filepath.EvalSymlinks(directory)
	if errors.Is(resolveErr, os.ErrNotExist) {
		return nil, nil
	}
	if resolveErr != nil || physical != directory {
		return nil, deletionError("the recorded native configuration path changed or contains a symbolic link.")
	}
	config, err := openRealDirectory(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, deletionError("the native configuration directory is unavailable or a symbolic link.")
	}
	defer config.Close()
	if err := verifyNoRunningNativeSession(config, id); err != nil {
		return nil, err
	}
	projects, err := openRealChild(config, "projects")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, deletionError("the native projects directory is unavailable or a symbolic link.")
	}
	defer projects.Close()
	entries, err := readRootDirectory(projects, maxNativeProjectDirectories)
	if err != nil {
		return nil, deletionError("native project directories could not be inspected.")
	}
	defer func() {
		if err != nil {
			for _, project := range openedPlans {
				project.root.Close()
			}
			result = nil
		}
	}()
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, deletionError("a native project directory is a symbolic link; inspect it before deleting.")
		}
		if !entry.IsDir() {
			continue
		}
		project, openErr := openRealChild(projects, entry.Name())
		if openErr != nil {
			return nil, deletionError("a native project directory is unavailable or changed.")
		}
		names, readErr := readRootDirectory(project, 100000)
		if readErr != nil {
			project.Close()
			return nil, deletionError("native session artifacts could not be inspected.")
		}
		plan := &nativeDeletionProject{root: project}
		for _, file := range names {
			name := file.Name()
			if !sessionArtifactName(id, name) {
				continue
			}
			info, statErr := project.Lstat(name)
			if statErr != nil {
				project.Close()
				return nil, deletionError("a native session artifact changed during inspection.")
			}
			if name == id {
				if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					project.Close()
					return nil, deletionError("the native session sidecar is not a real directory.")
				}
			} else if !info.Mode().IsRegular() {
				project.Close()
				return nil, deletionError("a native session transcript is not a regular file.")
			}
			path := filepath.Join(directory, "projects", entry.Name(), name)
			for _, cwd := range protected {
				if artifactContainsProject(path, cwd) {
					project.Close()
					return nil, deletionError("a native artifact overlaps a project working directory.")
				}
			}
			plan.targets = append(plan.targets, nativeDeletionTarget{name: name, info: info})
		}
		if len(plan.targets) == 0 {
			project.Close()
		} else {
			result = append(result, plan)
			openedPlans = append(openedPlans, plan)
		}
	}
	return result, nil
}

// Only documented transcript variants qualify, never an arbitrary UUID prefix.
// https://code.claude.com/docs/en/claude-directory#application-data
func sessionArtifactName(id, name string) bool {
	if name == id || name == id+".jsonl" {
		return true
	}
	if strings.HasPrefix(name, id+".orphaned-") && strings.HasSuffix(name, ".jsonl") {
		suffix := strings.TrimSuffix(strings.TrimPrefix(name, id+".orphaned-"), ".jsonl")
		stamp, tail, ok := strings.Cut(suffix, "-")
		return ok && timestampComponent(stamp) && safeSuffix(tail)
	}
	if strings.HasPrefix(name, id+".jsonl.superseded-") {
		return timestampComponent(strings.TrimPrefix(name, id+".jsonl.superseded-"))
	}
	return false
}

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)
var artifactSuffix = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func timestampComponent(value string) bool {
	return len(value) > 0 && len(value) <= 32 && digitsOnly.MatchString(value)
}
func safeSuffix(value string) bool {
	return len(value) > 0 && len(value) <= 128 && artifactSuffix.MatchString(value)
}

func readRootDirectory(root *os.Root, limit int) ([]os.DirEntry, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errors.New("directory exceeds inspection limit")
	}
	return entries, nil
}

func artifactContainsProject(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}

func verifyNoRunningNativeSession(config *os.Root, id string) error {
	registry, err := openRealChild(config, "sessions")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return deletionError("the running-session registry is unavailable or a symbolic link.")
	}
	defer registry.Close()
	entries, err := readRootDirectory(registry, 10000)
	if err != nil {
		return deletionError("the running-session registry could not be inspected.")
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := registry.Lstat(entry.Name())
		if err != nil || !info.Mode().IsRegular() {
			return deletionError("the running-session registry contains an unsafe record.")
		}
		f, err := registry.Open(entry.Name())
		if err != nil {
			return deletionError("a running-session record is unavailable.")
		}
		opened, statErr := f.Stat()
		data, readErr := io.ReadAll(io.LimitReader(f, 65537))
		f.Close()
		if statErr != nil || readErr != nil || len(data) > 65536 || !os.SameFile(info, opened) {
			return deletionError("a running-session record changed or is invalid.")
		}
		var record struct {
			SessionID string          `json:"sessionId"`
			PID       int             `json:"pid"`
			ProcStart json.RawMessage `json:"procStart"`
		}
		if json.Unmarshal(data, &record) != nil {
			return deletionError("a running-session record is invalid.")
		}
		var start string
		_ = json.Unmarshal(record.ProcStart, &start)
		if record.SessionID == id && (record.PID <= 0 || nativeSessionProcessAlive(record.PID, start)) {
			return deletionError("this session is still open in another Claude Code process; close it first.")
		}
	}
	return nil
}

func (s *server) journalRoot() (*os.Root, error) {
	path := filepath.Join(s.cfg.StateDirectory, deletionJournalDirectory)
	if err := privateDirectory(path); err != nil {
		return nil, deletionError("the private deletion journal directory is unavailable.")
	}
	root, err := openRealDirectory(path)
	if err != nil {
		return nil, deletionError("the private deletion journal directory is unsafe.")
	}
	return root, nil
}

func (s *server) writeDeletionRecord(record *deletionRecord) error {
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxDeletionJournalBytes {
		return deletionError("the deletion intent exceeds its metadata limit.")
	}
	root, err := s.journalRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	name := fmt.Sprintf(".intent-%d-%s", time.Now().UnixNano(), record.ThreadID)
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return deletionError("the deletion intent could not be written.")
	}
	defer root.Remove(name)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = root.Rename(name, record.ThreadID+".json")
	}
	if err == nil {
		err = syncRoot(root)
	}
	if err != nil {
		return deletionError("the deletion intent could not be persisted.")
	}
	return nil
}

func (s *server) recoverDeletions() error {
	root, err := s.journalRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := readRootDirectory(root, 100000)
	if err != nil {
		return deletionError("the private deletion journal could not be inspected.")
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.Name() != id+".json" || !canonicalID(id) {
			continue
		}
		record := &deletionRecord{ThreadID: id, invalid: true}
		s.deletions[id] = record
		info, statErr := root.Lstat(entry.Name())
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			s.deletionErrors[id] = deletionError("the saved deletion intent is not a private regular file.").Error()
			continue
		}
		f, openErr := root.Open(entry.Name())
		if openErr != nil {
			s.deletionErrors[id] = deletionError("the saved deletion intent is unavailable.").Error()
			continue
		}
		opened, statErr := f.Stat()
		data, readErr := io.ReadAll(io.LimitReader(f, maxDeletionJournalBytes+1))
		f.Close()
		if statErr != nil || readErr != nil || len(data) > maxDeletionJournalBytes || !os.SameFile(info, opened) || json.Unmarshal(data, record) != nil || !validDeletionRecord(record, id) {
			record = &deletionRecord{ThreadID: id, invalid: true}
			s.deletions[id] = record
			s.deletionErrors[id] = deletionError("the saved deletion intent is invalid.").Error()
			continue
		}
		record.invalid = false
		if record.Done {
			if cleanupErr := s.removeAdapterHistory(id); cleanupErr != nil {
				s.deletionErrors[id] = cleanupErr.Error()
			}
			minimal := completedDeletionRecord(record)
			if len(record.NativeConfigRoots) > 0 || len(record.ProtectedCWDs) > 0 {
				if cleanupErr := s.writeDeletionRecord(minimal); cleanupErr != nil {
					s.deletionErrors[id] = cleanupErr.Error()
				}
			}
			s.deletions[id] = minimal
			delete(s.threads, id)
			continue
		}
		if cleanupErr := s.completeDeletion(record, nil); cleanupErr != nil {
			s.deletionErrors[id] = cleanupErr.Error()
			continue
		}
		s.deletions[id] = completedDeletionRecord(record)
		delete(s.threads, id)
	}
	return nil
}

func validDeletionRecord(record *deletionRecord, id string) bool {
	if record.Version != 1 || record.ThreadID != id {
		return false
	}
	if record.Done {
		return true
	}
	if len(record.NativeConfigRoots) == 0 || len(record.NativeConfigRoots) > 32 {
		return false
	}
	for _, path := range append(append([]string(nil), record.NativeConfigRoots...), record.ProtectedCWDs...) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			return false
		}
	}
	return true
}
