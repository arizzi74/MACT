package sessionarchive

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ulikunitz/xz"
)

const maxManifestBytes = 16 << 20

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && filepath.Base(name) == name
}

func safeRelative(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00") && !path.IsAbs(name) && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

func nativeMemberAllowed(name, kind string) bool {
	if name == "native" {
		return kind == "directory"
	}
	if name == "native/home" || name == "native/db" || name == "native/attachments" {
		return kind == "directory"
	}
	if strings.HasPrefix(name, "native/db/") {
		base := strings.TrimPrefix(name, "native/db/")
		for _, db := range nativeDBs {
			if base == db {
				return kind == "file"
			}
		}
		return false
	}
	if strings.HasPrefix(name, "native/attachments/") {
		parts := strings.Split(strings.TrimPrefix(name, "native/attachments/"), "/")
		if _, err := uuid.Parse(parts[0]); err != nil {
			return false
		}
		return (len(parts) == 1 && kind == "directory") || (len(parts) == 2 && safeName(parts[1]) && kind == "file")
	}
	if name == "native/home/session_index.jsonl" {
		return kind == "file"
	}
	for _, prefix := range []string{"native/home/sessions", "native/home/archived_sessions"} {
		if name == prefix {
			return kind == "directory"
		}
		if strings.HasPrefix(name, prefix+"/") {
			rel := strings.TrimPrefix(name, prefix+"/")
			parts := strings.Split(rel, "/")
			if strings.HasSuffix(prefix, "/archived_sessions") {
				if len(parts) != 1 {
					return false
				}
			} else {
				if kind == "directory" {
					if len(parts) > 3 {
						return false
					}
					for _, part := range parts {
						if part == "" || strings.Trim(part, "0123456789") != "" {
							return false
						}
					}
					return true
				}
				if len(parts) != 4 {
					return false
				}
				for _, part := range parts[:3] {
					if part == "" || strings.Trim(part, "0123456789") != "" {
						return false
					}
				}
			}
			if kind != "file" {
				return false
			}
			_, err := enumerateRollout(filepath.Base(name), ".")
			return err == nil
		}
	}
	return false
}

func within(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func checkedRoot(value string) (string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("expected directory %q", value)
	}
	return resolved, nil
}

// Export publishes an independently checksummed tar.xz only after every source
// file has been read consistently. It leaves the live project and native data intact.
func Export(ctx context.Context, o ExportOptions) (Manifest, error) {
	var m Manifest
	if _, err := uuid.Parse(o.ThreadID); err != nil {
		return m, fmt.Errorf("invalid thread ID: %w", err)
	}
	if o.ArchiveID == "" {
		o.ArchiveID = uuid.NewString()
	}
	if !strings.HasSuffix(o.ArchiveID, ".tar.xz") {
		o.ArchiveID += ".tar.xz"
	}
	if !validArchiveID(o.ArchiveID) {
		return m, fmt.Errorf("invalid archive ID")
	}
	home, err := checkedRoot(o.CodexHome)
	if err != nil {
		return m, err
	}
	o.CodexHome = home
	project, err := checkedRoot(o.ProjectPath)
	if err != nil {
		return m, err
	}
	o.ProjectPath = project
	if o.SQLiteHome == "" {
		o.SQLiteHome = home
	}
	if within(project, home) || within(home, project) {
		return m, fmt.Errorf("project, Codex home and archive directory must be separate")
	}
	if err := os.MkdirAll(o.ArchiveDir, 0700); err != nil {
		return m, err
	}
	o.ArchiveDir, err = checkedRoot(o.ArchiveDir)
	if err != nil {
		return m, err
	}
	if within(project, o.ArchiveDir) {
		return m, fmt.Errorf("archive directory must be outside the project")
	}
	final := filepath.Join(o.ArchiveDir, o.ArchiveID)
	if _, err := os.Lstat(final); err == nil {
		old, err := Read(ctx, final)
		if err != nil {
			return m, err
		}
		if old.ArchiveID != o.ArchiveID || old.ThreadID != o.ThreadID || old.OriginalPath != project {
			return m, ErrConflict
		}
		return old, nil
	} else if !os.IsNotExist(err) {
		return m, err
	}
	stage, err := os.MkdirTemp(o.ArchiveDir, ".archive-stage-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(stage)
	m = Manifest{FormatVersion: FormatVersion, ArchiveID: o.ArchiveID, ThreadID: o.ThreadID, DisplayName: o.DisplayName, OriginalPath: project, OriginalCodexHome: home, CreatedAt: time.Now().UTC(), CodexVersion: o.CodexVersion, ArchivePath: final}
	metadataCheck := m
	metadataCheck.ThreadIDs = []string{o.ThreadID}
	if err := validateManifestMetadata(metadataCheck, final); err != nil {
		return Manifest{}, err
	}
	progress(o.Progress, "Saving native Codex history and metadata")
	if err := exportNative(ctx, o, stage, &m); err != nil {
		return Manifest{}, err
	}
	if err := validateNativeSnapshot(ctx, stage, m); err != nil {
		return Manifest{}, err
	}
	progress(o.Progress, "Saving project files")
	if err := copyTree(ctx, project, filepath.Join(stage, "project")); err != nil {
		return Manifest{}, err
	}
	if err := inventoryStage(ctx, stage, &m); err != nil {
		return Manifest{}, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if len(b) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: archive manifest exceeds %d MiB; source files were preserved", ErrUnsupported, maxManifestBytes>>20)
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), b, 0600); err != nil {
		return Manifest{}, err
	}
	progress(o.Progress, "Compressing and checking archive")
	tmp, err := os.CreateTemp(o.ArchiveDir, ".archive-publish-")
	if err != nil {
		return Manifest{}, err
	}
	tempName := tmp.Name()
	defer os.Remove(tempName)
	if err := pack(ctx, tmp, stage); err != nil {
		tmp.Close()
		return Manifest{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Manifest{}, err
	}
	if err := tmp.Close(); err != nil {
		return Manifest{}, err
	}
	if _, err := Read(ctx, tempName); err != nil {
		return Manifest{}, fmt.Errorf("verify compressed archive: %w", err)
	}
	// Link publishes without overwriting an archive won by another operation.
	if err := os.Link(tempName, final); err != nil {
		return Manifest{}, fmt.Errorf("publish archive: %w", err)
	}
	if dir, err := os.Open(o.ArchiveDir); err == nil {
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return m, err
		}
	}
	return m, nil
}

func copyTree(ctx context.Context, src, dst string) error {
	type directoryMode struct {
		path string
		mode os.FileMode
	}
	var modes []directoryMode
	err := filepath.WalkDir(src, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, current)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			modes = append(modes, directoryMode{target, info.Mode().Perm()})
			return os.MkdirAll(target, 0700)
		case info.Mode().IsRegular():
			return copyFile(ctx, current, target, info)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(current)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("unsupported special project file: %s", rel)
		}
	})
	if err != nil {
		return err
	}
	for i := len(modes) - 1; i >= 0; i-- {
		if err := os.Chmod(modes[i].path, modes[i].mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(ctx context.Context, src, dst string, before os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	in, err := os.OpenFile(src, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf("source changed during archive: %s", src)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, before.Mode().Perm()&0777)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			out.Close()
			os.Remove(dst)
		}
	}()
	_, err = io.Copy(out, &contextReader{ctx: ctx, r: in})
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	after, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("source changed during archive: %s", src)
	}
	complete = true
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func inventoryStage(ctx context.Context, stage string, m *Manifest) error {
	sources := map[string]string{}
	for _, e := range m.Files {
		sources[e.Path] = e.SourcePath
	}
	m.Files = nil
	err := filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == stage {
			return nil
		}
		rel, _ := filepath.Rel(stage, p)
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Path: rel, Mode: uint32(info.Mode().Perm()), SourcePath: sources[rel]}
		switch {
		case d.IsDir():
			e.Kind = "directory"
		case info.Mode().IsRegular():
			e.Kind = "file"
			e.Size = info.Size()
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, &contextReader{ctx: ctx, r: f})
			f.Close()
			if err != nil {
				return err
			}
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
		case info.Mode()&os.ModeSymlink != 0:
			e.Kind = "symlink"
			e.Link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry: %s", rel)
		}
		m.Files = append(m.Files, e)
		return nil
	})
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return err
}

func pack(ctx context.Context, out io.Writer, stage string) error {
	x, err := xz.NewWriter(out)
	if err != nil {
		return err
	}
	t := tar.NewWriter(x)
	err = filepath.WalkDir(stage, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == stage {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(p)
			if err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(stage, p)
		h.Name = filepath.ToSlash(rel)
		h.Uid = 0
		h.Gid = 0
		h.Uname = ""
		h.Gname = ""
		if err := t.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(t, &contextReader{ctx: ctx, r: f})
		return err
	})
	if err != nil {
		t.Close()
		x.Close()
		return err
	}
	if err := t.Close(); err != nil {
		x.Close()
		return err
	}
	return x.Close()
}

// Read streams and validates the complete archive, including every checksum.
func Read(ctx context.Context, filename string) (Manifest, error) { return unpack(ctx, filename, "") }

func unpack(ctx context.Context, filename, dst string) (Manifest, error) {
	var m Manifest
	f, err := os.OpenFile(filename, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() {
		return m, fmt.Errorf("archive is not a regular file")
	}
	if err := inspectXZ(ctx, f, info.Size()); err != nil {
		return m, err
	}
	x, err := (xz.ReaderConfig{DictCap: 8 << 20, SingleStream: true}).NewReader(&contextReader{ctx: ctx, r: f})
	if err != nil {
		return m, err
	}
	tr := tar.NewReader(x)
	seen := map[string]Entry{}
	symlinks := map[string]string{}
	haveManifest := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}
		if !safeRelative(h.Name) {
			return m, fmt.Errorf("unsafe archive path %q", h.Name)
		}
		if _, ok := seen[h.Name]; ok {
			return m, fmt.Errorf("duplicate archive path %q", h.Name)
		}
		for parent := path.Dir(h.Name); parent != "."; parent = path.Dir(parent) {
			if _, ok := symlinks[parent]; ok {
				return m, fmt.Errorf("archive path has symlink parent %q", h.Name)
			}
		}
		e := Entry{Path: h.Name, Mode: uint32(h.Mode) & 0777, Size: h.Size}
		if h.Mode&07000 != 0 {
			return m, fmt.Errorf("unsafe archive mode")
		}
		if h.Name == "manifest.json" {
			if haveManifest || h.Typeflag != tar.TypeReg || h.Size > maxManifestBytes {
				return m, fmt.Errorf("invalid archive manifest")
			}
			b, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
			if err != nil {
				return m, err
			}
			if err = json.Unmarshal(b, &m); err != nil {
				return m, err
			}
			haveManifest = true
			seen[h.Name] = e
			continue
		}
		if h.Name != "project" && h.Name != "native" && !strings.HasPrefix(h.Name, "project/") && !strings.HasPrefix(h.Name, "native/") {
			return m, fmt.Errorf("unexpected archive path %q", h.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			e.Kind = "directory"
			e.Size = 0
			if dst != "" {
				if err := os.MkdirAll(target, 0700); err != nil {
					return m, err
				}
			}
		case tar.TypeReg:
			e.Kind = "file"
			hash := sha256.New()
			var out *os.File
			w := io.Writer(hash)
			if dst != "" {
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					return m, err
				}
				out, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(e.Mode)&0777)
				if err != nil {
					return m, err
				}
				w = io.MultiWriter(out, hash)
			}
			_, copyErr := io.Copy(w, tr)
			if out != nil {
				err = out.Close()
			}
			if copyErr != nil {
				return m, copyErr
			}
			if err != nil {
				return m, err
			}
			e.SHA256 = hex.EncodeToString(hash.Sum(nil))
		case tar.TypeSymlink:
			e.Kind = "symlink"
			e.Size = 0
			e.Link = h.Linkname
			if !strings.HasPrefix(h.Name, "project/") || strings.ContainsRune(h.Linkname, '\x00') {
				return m, fmt.Errorf("unsafe symlink %q", h.Name)
			}
			symlinks[h.Name] = h.Linkname
		default:
			return m, fmt.Errorf("unsupported archive entry type for %q", h.Name)
		}
		seen[h.Name] = e
		if strings.HasPrefix(h.Name, "native") && !nativeMemberAllowed(h.Name, e.Kind) {
			return m, fmt.Errorf("unsupported native archive member %q", h.Name)
		}
	}
	// Reading beyond tar EOF also validates the xz footer and disallows concealed data.
	remaining, err := io.Copy(io.Discard, x)
	if err != nil {
		return m, err
	}
	if remaining != 0 {
		return m, fmt.Errorf("trailing archive data")
	}
	if !haveManifest || m.FormatVersion != FormatVersion || !safeName(m.ArchiveID) || !strings.HasSuffix(m.ArchiveID, ".tar.xz") || !filepath.IsAbs(m.OriginalPath) || !filepath.IsAbs(m.OriginalCodexHome) {
		return m, fmt.Errorf("invalid archive manifest")
	}
	if err := validateManifestMetadata(m, filename); err != nil {
		return m, err
	}
	if strings.HasSuffix(filepath.Base(filename), ".tar.xz") && filepath.Base(filename) != m.ArchiveID {
		return m, fmt.Errorf("archive filename does not match its manifest ID")
	}
	if _, err := uuid.Parse(m.ThreadID); err != nil {
		return m, fmt.Errorf("invalid manifest thread ID")
	}
	if len(m.ThreadIDs) == 0 {
		return m, fmt.Errorf("missing native thread IDs")
	}
	ids := map[string]bool{}
	for _, id := range m.ThreadIDs {
		if _, err := uuid.Parse(id); err != nil || ids[id] {
			return m, fmt.Errorf("invalid native manifest thread tree")
		}
		ids[id] = true
	}
	if !ids[m.ThreadID] {
		return m, fmt.Errorf("archive root absent from native thread tree")
	}
	if len(seen) != len(m.Files)+1 {
		return m, fmt.Errorf("archive manifest entry count mismatch")
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, ok := symlinks[parent]; ok {
				return m, fmt.Errorf("archive member beneath a symlink: %s", name)
			}
		}
	}
	manifestPaths := map[string]bool{}
	for _, want := range m.Files {
		if want.SourcePath != "" && (!filepath.IsAbs(want.SourcePath) || !strings.HasPrefix(want.Path, "native/attachments/") || want.Kind != "file" || strings.ContainsRune(want.SourcePath, '\x00')) {
			return m, fmt.Errorf("invalid native attachment reference")
		}
		if manifestPaths[want.Path] {
			return m, fmt.Errorf("duplicate manifest entry")
		}
		manifestPaths[want.Path] = true
		got, ok := seen[want.Path]
		if !ok || got.Kind != want.Kind || got.Mode != want.Mode || got.Size != want.Size || got.SHA256 != want.SHA256 || got.Link != want.Link {
			return m, fmt.Errorf("archive checksum or entry mismatch: %s", want.Path)
		}
	}
	if dst != "" {
		for name, link := range symlinks {
			target := filepath.Join(dst, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return m, err
			}
			if err := os.Symlink(link, target); err != nil {
				return m, err
			}
		}
		for i := len(m.Files) - 1; i >= 0; i-- {
			entry := m.Files[i]
			if entry.Kind == "directory" {
				if err := os.Chmod(filepath.Join(dst, filepath.FromSlash(entry.Path)), os.FileMode(entry.Mode)); err != nil {
					return m, err
				}
			}
		}
	}
	m.ArchivePath = filename
	return m, nil
}
