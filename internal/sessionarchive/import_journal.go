package sessionarchive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

type importIntent struct {
	ArchiveID   string   `json:"archive_id"`
	Digest      string   `json:"sha256"`
	Destination string   `json:"destination"`
	CodexHome   string   `json:"codex_home"`
	SQLiteHome  string   `json:"sqlite_home"`
	ThreadIDs   []string `json:"thread_ids"`
	Repair      bool     `json:"repair,omitempty"`
}

func intentPath(o RestoreOptions, m Manifest) string {
	return filepath.Join(o.CodexHome, ".telegramgw-archive-imports", m.ArchiveID+".json")
}

func expectedIntent(ctx context.Context, o RestoreOptions, m Manifest) (importIntent, error) {
	hash, err := Digest(ctx, o.ArchivePath)
	return importIntent{ArchiveID: m.ArchiveID, Digest: hash, Destination: o.Destination, CodexHome: o.CodexHome, SQLiteHome: o.SQLiteHome, ThreadIDs: m.ThreadIDs, Repair: o.repairNative}, err
}

func ownsInterruptedImport(ctx context.Context, o RestoreOptions, m Manifest) (bool, error) {
	owned, repair, err := interruptedImportMode(ctx, o, m)
	if err != nil {
		return false, err
	}
	if owned && repair != o.repairNative {
		return false, fmt.Errorf("%w: native import journal mode changed", ErrConflict)
	}
	return owned, nil
}

func interruptedImportMode(ctx context.Context, o RestoreOptions, m Manifest) (bool, bool, error) {
	filename := intentPath(o, m)
	if err := checkDestinationParents(filename); err != nil {
		return false, false, err
	}
	f, err := os.OpenFile(filename, os.O_RDONLY|noFollowFlag(), 0)
	if os.IsNotExist(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return false, false, fmt.Errorf("invalid native import journal")
	}
	var got importIntent
	if err := json.NewDecoder(io.LimitReader(f, maxManifestBytes)).Decode(&got); err != nil {
		return false, false, err
	}
	want, err := expectedIntent(ctx, o, m)
	if err != nil {
		return false, false, err
	}
	want.Repair = got.Repair
	if !reflect.DeepEqual(got, want) {
		return false, false, fmt.Errorf("%w: an interrupted import belongs to another archive or destination", ErrConflict)
	}
	return true, got.Repair, nil
}

func beginImportIntent(ctx context.Context, o RestoreOptions, m Manifest) error {
	if owns, err := ownsInterruptedImport(ctx, o, m); err != nil || owns {
		return err
	}
	filename := intentPath(o, m)
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	if err := checkDestinationParents(filename); err != nil {
		return err
	}
	intent, err := expectedIntent(ctx, o, m)
	if err != nil {
		return err
	}
	b, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".intent-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), filename); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(filename))
}

func syncDirectory(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func publishNativeFile(ctx context.Context, source, target string) (bool, error) {
	if err := checkDestinationParents(target); err != nil {
		return false, err
	}
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("%w: native target is not a regular file", ErrConflict)
		}
		a, err := Digest(ctx, source)
		if err != nil {
			return false, err
		}
		b, err := Digest(ctx, target)
		if err != nil {
			return false, err
		}
		if a != b {
			return false, fmt.Errorf("%w: native file differs from interrupted import", ErrConflict)
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".native-import-")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	tmp.Close()
	os.Remove(name)
	defer os.Remove(name)
	info, err := os.Stat(source)
	if err != nil {
		return false, err
	}
	if err := copyFile(ctx, source, name, info); err != nil {
		return false, err
	}
	f, err := os.OpenFile(name, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return false, err
	}
	err = f.Sync()
	f.Close()
	if err != nil {
		return false, err
	}
	if err := os.Link(name, target); err != nil {
		return false, err
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return true, err
	}
	return true, nil
}
