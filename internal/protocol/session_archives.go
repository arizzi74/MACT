package protocol

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Session archives are worker-local. An ID is a filename, never an arbitrary
// path supplied by the browser. Native payloads never cross this protocol.
type SessionArchiveRequest struct {
	ArchiveID     string `json:"archive_id,omitempty"`
	DeleteProject bool   `json:"delete_project,omitempty"`
	Offset        int    `json:"offset,omitempty"`
}

type SessionArchiveEntry struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	SessionName  string    `json:"session_name"`
	OriginalPath string    `json:"original_path"`
	CreatedAt    time.Time `json:"created_at"`
	Bytes        int64     `json:"bytes"`
	CodexVersion string    `json:"codex_version"`
}

type SessionArchiveResult struct {
	Phase               string                `json:"phase,omitempty"`
	ArchiveDirectory    string                `json:"archive_directory,omitempty"`
	Archive             *SessionArchiveEntry  `json:"archive,omitempty"`
	Archives            []SessionArchiveEntry `json:"archives,omitempty"`
	NextOffset          *int                  `json:"next_offset,omitempty"`
	ProjectDeleted      bool                  `json:"project_deleted,omitempty"`
	WarningCode         string                `json:"warning_code,omitempty"`
	RetainedProjectPath string                `json:"retained_project_path,omitempty"`
}

// Session titles do not name directories: punctuation and slashes are valid.
func NormalizeSessionTitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || value == "" || utf8.RuneCountInString(value) > 120 {
		return "", errors.New("enter a session name of 1–120 characters")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", errors.New("session names cannot contain control characters")
		}
	}
	return value, nil
}

func ValidSessionArchiveID(value string) bool {
	if !utf8.ValidString(value) || len(value) > 255 || !strings.HasSuffix(value, ".tar.xz") || len(value) <= len(".tar.xz") || strings.ContainsAny(value, "/\\") || value == "." || value == ".." || strings.HasPrefix(value, ".") || filepath.Base(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validateSessionArchiveCommand(c Command) error {
	r := c.Arguments.SessionArchive
	switch c.Operation {
	case ArchiveSession:
		if r == nil || r.ArchiveID != "" || r.Offset != 0 || strings.TrimSpace(c.Arguments.CWD) == "" {
			return errors.New("invalid session archive request")
		}
	case ListSessionArchives:
		if r == nil || r.ArchiveID != "" || r.DeleteProject || r.Offset < 0 || r.Offset > 100000 {
			return errors.New("invalid archive list request")
		}
	case RestoreSession:
		if r == nil || !ValidSessionArchiveID(r.ArchiveID) || r.DeleteProject || r.Offset != 0 {
			return errors.New("invalid archive restore request")
		}
	default:
		if r != nil {
			return errors.New("archive arguments require an archive operation")
		}
	}
	return nil
}

func (r *SessionArchiveResult) Validate() error {
	if r == nil || len(r.Archives) > 1000 || (r.ArchiveDirectory != "" && !filepath.IsAbs(r.ArchiveDirectory)) {
		return errors.New("invalid archive result")
	}
	if r.WarningCode != "" && r.WarningCode != "project_cleanup_failed" {
		return errors.New("invalid archive warning")
	}
	if r.RetainedProjectPath != "" && (r.WarningCode != "project_cleanup_failed" || len(r.RetainedProjectPath) > 4096 || !filepath.IsAbs(r.RetainedProjectPath) || strings.IndexFunc(r.RetainedProjectPath, unicode.IsControl) >= 0) {
		return errors.New("invalid retained project path")
	}
	switch r.Phase {
	case "", "preparing", "exporting", "compressing", "verifying", "archiving", "cleaning", "extracting", "restoring", "completed", "recovering":
	default:
		return errors.New("invalid archive phase")
	}
	entries := r.Archives
	if r.Archive != nil {
		entries = append(append([]SessionArchiveEntry(nil), entries...), *r.Archive)
	}
	for _, e := range entries {
		if !ValidSessionArchiveID(e.ID) || len(e.Name) > 512 || len(e.SessionName) > 1024 || len(e.OriginalPath) > 4096 || !filepath.IsAbs(e.OriginalPath) || e.CreatedAt.IsZero() || e.Bytes < 0 || len(e.CodexVersion) > 128 {
			return errors.New("invalid archive entry")
		}
	}
	if r.NextOffset != nil && (*r.NextOffset < 0 || *r.NextOffset > 100000) {
		return errors.New("invalid archive pagination")
	}
	return nil
}
