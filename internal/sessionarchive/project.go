package sessionarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// CheckProject proves that every current project entry matches the verified
// archive, including names, contents, permissions, and inert symlink targets.
func CheckProject(ctx context.Context, directory string, m Manifest) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return ProjectMatches(ctx, root, m)
}

// ProjectMatches supports a directory descriptor pinned by the caller before a
// source is renamed for cleanup. Neither walking nor hashing follows symlinks.
func ProjectMatches(ctx context.Context, root *os.Root, m Manifest, allowMissing ...bool) error {
	want := map[string]Entry{}
	for _, e := range m.Files {
		if e.Path == "project" || strings.HasPrefix(e.Path, "project/") {
			rel := strings.TrimPrefix(e.Path, "project/")
			if e.Path == "project" {
				rel = "."
			}
			want[rel] = e
		}
	}
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		e, ok := want[name]
		if !ok {
			return fmt.Errorf("%w: project has entries absent from archive", ErrConflict)
		}
		delete(want, name)
		before, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if before.Mode().Perm() != os.FileMode(e.Mode) {
			return fmt.Errorf("%w: project permissions changed", ErrConflict)
		}
		switch e.Kind {
		case "directory":
			if !before.IsDir() {
				return fmt.Errorf("%w: project entry type changed", ErrConflict)
			}
		case "symlink":
			if before.Mode()&os.ModeSymlink == 0 {
				return ErrConflict
			}
			link, err := root.Readlink(name)
			if err != nil {
				return err
			}
			if link != e.Link {
				return fmt.Errorf("%w: project symlink changed", ErrConflict)
			}
		case "file":
			if !before.Mode().IsRegular() || before.Size() != e.Size {
				return fmt.Errorf("%w: project file changed", ErrConflict)
			}
			f, err := root.OpenFile(name, os.O_RDONLY|noFollowFlag(), 0)
			if err != nil {
				return err
			}
			opened, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
				f.Close()
				return fmt.Errorf("%w: project changed during validation", ErrConflict)
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, &contextReader{ctx: ctx, r: f})
			after, statErr := f.Stat()
			f.Close()
			if copyErr != nil {
				return copyErr
			}
			if statErr != nil {
				return statErr
			}
			current, err := root.Lstat(name)
			if err != nil {
				return err
			}
			if !current.Mode().IsRegular() || !os.SameFile(opened, current) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || hex.EncodeToString(hash.Sum(nil)) != e.SHA256 {
				return fmt.Errorf("%w: project file changed during validation", ErrConflict)
			}
		default:
			return fmt.Errorf("invalid project manifest entry")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(want) != 0 && !(len(allowMissing) > 0 && allowMissing[0]) {
		return fmt.Errorf("%w: project is missing archived entries", ErrConflict)
	}
	return nil
}
