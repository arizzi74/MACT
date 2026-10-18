// Package sessionarchive stores a project together with its native Codex replay
// data. It deliberately never archives authentication or replaces a worker's
// global Codex database. Callers must quiesce the complete thread tree first.
package sessionarchive

import (
	"errors"
	"time"
)

const FormatVersion = 1

var (
	ErrConflict    = errors.New("archive restore destination already exists")
	ErrUnsupported = errors.New("unsupported native Codex archive layout")
)

type ExportOptions struct {
	CodexHome    string
	SQLiteHome   string
	ThreadID     string
	ProjectPath  string
	ArchiveDir   string
	ArchiveID    string
	DisplayName  string
	CodexVersion string
	Progress     func(string)
}

type RestoreOptions struct {
	ArchivePath string
	CodexHome   string
	SQLiteHome  string
	// Destination defaults to the original project path.
	Destination    string
	CodexVersion   string
	Progress       func(string)
	repairNative   bool
	rolloutAliases map[string]string
}

type Manifest struct {
	FormatVersion         int       `json:"format_version"`
	ArchiveID             string    `json:"archive_id"`
	ThreadID              string    `json:"thread_id"`
	ThreadIDs             []string  `json:"thread_ids"`
	DisplayName           string    `json:"display_name"`
	OriginalPath          string    `json:"original_path"`
	OriginalCodexHome     string    `json:"original_codex_home"`
	CreatedAt             time.Time `json:"created_at"`
	CodexVersion          string    `json:"codex_version"`
	ArchivePath           string    `json:"-"`
	VerifiedSelfContained bool      `json:"-"`
	Files                 []Entry   `json:"files"`
}

type VerifyOptions struct {
	CodexBinary string
	Progress    func(string)
}

type Entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
	// SourcePath is used only for native attachment rebinding on import.
	SourcePath string `json:"source_path,omitempty"`
}

func progress(fn func(string), message string) {
	if fn != nil {
		fn(message)
	}
}
