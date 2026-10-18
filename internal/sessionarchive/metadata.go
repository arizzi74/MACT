package sessionarchive

import (
	"archive/tar"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/ulikunitz/xz"
)

// ReadMetadata reads the bounded, first manifest for inventory display. It
// validates framing and manifest paths but does not verify payload checksums.
// Read and Restore remain mandatory before using any archived payload.
func ReadMetadata(ctx context.Context, filename string) (Manifest, error) {
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
	h, err := tr.Next()
	if err != nil {
		return m, err
	}
	if h.Name != "manifest.json" || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxManifestBytes {
		return m, fmt.Errorf("archive manifest must be its first entry")
	}
	b, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if err := validateManifestMetadata(m, filename); err != nil {
		return m, err
	}
	m.ArchivePath = filename
	return m, nil
}

func validateManifestMetadata(m Manifest, filename string) error {
	cleanPath := func(p string) bool {
		return utf8.ValidString(p) && len(p) <= 4096 && filepath.IsAbs(p) && filepath.Clean(p) == p && strings.IndexFunc(p, unicode.IsControl) < 0
	}
	if m.FormatVersion != FormatVersion || !validArchiveID(m.ArchiveID) || !cleanPath(m.OriginalPath) || !cleanPath(m.OriginalCodexHome) || m.CreatedAt.IsZero() || !utf8.ValidString(m.DisplayName) || len(m.DisplayName) > 1024 || strings.IndexFunc(m.DisplayName, unicode.IsControl) >= 0 || !utf8.ValidString(m.CodexVersion) || len(m.CodexVersion) > 128 || strings.IndexFunc(m.CodexVersion, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid archive manifest")
	}
	if strings.HasSuffix(filepath.Base(filename), ".tar.xz") && filepath.Base(filename) != m.ArchiveID {
		return fmt.Errorf("archive filename differs from manifest ID")
	}
	ids := map[string]bool{}
	for _, id := range m.ThreadIDs {
		if _, err := uuid.Parse(id); err != nil || ids[id] {
			return fmt.Errorf("invalid native manifest thread tree")
		}
		ids[id] = true
	}
	if !ids[m.ThreadID] {
		return fmt.Errorf("archive root absent from native thread tree")
	}
	seen := map[string]bool{}
	for _, e := range m.Files {
		if !safeRelative(e.Path) || seen[e.Path] {
			return fmt.Errorf("invalid manifest entry path")
		}
		seen[e.Path] = true
		if e.Mode&^0777 != 0 || e.Size < 0 {
			return fmt.Errorf("invalid manifest entry metadata")
		}
		if strings.HasPrefix(e.Path, "native") {
			if !nativeMemberAllowed(e.Path, e.Kind) {
				return fmt.Errorf("unsupported native manifest member")
			}
		} else if e.Path != "project" && !strings.HasPrefix(e.Path, "project/") {
			return fmt.Errorf("unexpected manifest member")
		}
		switch e.Kind {
		case "file":
			hash, err := hex.DecodeString(e.SHA256)
			if err != nil || len(hash) != 32 {
				return fmt.Errorf("invalid entry checksum")
			}
		case "directory":
			if e.Size != 0 || e.Link != "" {
				return fmt.Errorf("invalid directory entry")
			}
		case "symlink":
			if !strings.HasPrefix(e.Path, "project/") || strings.ContainsRune(e.Link, '\x00') {
				return fmt.Errorf("invalid symlink entry")
			}
		default:
			return fmt.Errorf("invalid manifest entry kind")
		}
		if e.SourcePath != "" && (!filepath.IsAbs(e.SourcePath) || !strings.HasPrefix(e.Path, "native/attachments/") || e.Kind != "file" || strings.ContainsRune(e.SourcePath, '\x00')) {
			return fmt.Errorf("invalid native attachment reference")
		}
	}
	return nil
}

func validArchiveID(id string) bool {
	return safeName(id) && utf8.ValidString(id) && len(id) <= 255 && len(id) > len(".tar.xz") && !strings.HasPrefix(id, ".") && strings.HasSuffix(id, ".tar.xz") && strings.IndexFunc(id, unicode.IsControl) < 0
}
