package releasemanager

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const claudeOrphanStageAge = 24 * time.Hour

// cleanupClaudeRuntimeStages requires the worker update lock. SIGKILL can leave
// a stage behind before the recovery journal exists. This cleans only our
// private staging layout, never native version files or arbitrary subtrees.
// The explicit root also permits recovery without loading current HOME policy.
func cleanupClaudeRuntimeStages(versionsDir string, now time.Time) error {
	claudeDir := filepath.Dir(versionsDir)
	shareDir := filepath.Dir(claudeDir)
	localDir := filepath.Dir(shareDir)
	home := filepath.Dir(localDir)
	if !filepath.IsAbs(versionsDir) || filepath.Clean(versionsDir) != versionsDir || filepath.Base(versionsDir) != "versions" || filepath.Base(claudeDir) != "claude" || filepath.Base(shareDir) != "share" || filepath.Base(localDir) != ".local" {
		return errors.New("cannot safely identify the Claude runtime staging root")
	}
	for _, path := range []string{home, localDir, shareDir, claudeDir, versionsDir} {
		if !claudeOwnedPath(path, true) {
			return errors.New("cannot safely inspect the Claude runtime staging root")
		}
	}
	before, err := os.Lstat(versionsDir)
	if err != nil {
		return errors.New("cannot inspect the Claude runtime staging root")
	}
	root, err := os.OpenRoot(versionsDir)
	if err != nil {
		return errors.New("cannot open the Claude runtime staging root")
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("Claude runtime staging root changed during inspection")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("cannot read the Claude runtime staging root")
	}
	defer directory.Close()
	cutoff := now.Add(-claudeOrphanStageAge)
	for {
		entries, err := directory.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return errors.New("cannot read the Claude runtime staging root")
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, ".mact-stage-") || name == ".mact-stage-" {
				continue
			}
			if err := cleanupClaudeRuntimeStage(root, directory, name, cutoff); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func claudePrivateStageEntry(info os.FileInfo, directory bool, cutoff time.Time) bool {
	if info == nil || !info.ModTime().Before(cutoff) || info.Mode()&os.ModeSymlink != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return false
	}
	if directory {
		return info.Mode() == os.ModeDir|0700
	}
	return info.Mode() == 0600 || info.Mode() == 0700
}

func cleanupClaudeRuntimeStage(root *os.Root, directory *os.File, name string, cutoff time.Time) error {
	info, err := root.Lstat(name)
	if err != nil || !claudePrivateStageEntry(info, true, cutoff) {
		return nil
	}
	stage, err := root.OpenRoot(name)
	if err != nil {
		return nil
	}
	defer stage.Close()
	opened, err := stage.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil
	}
	file, err := stage.Open(".")
	if err != nil {
		return nil
	}
	entries, readErr := file.ReadDir(2)
	file.Close()
	if (readErr != nil && !errors.Is(readErr, io.EOF)) || len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != "claude") {
		return nil
	}
	var binary os.FileInfo
	if len(entries) == 1 {
		binary, err = stage.Lstat("claude")
		if err != nil || !claudePrivateStageEntry(binary, false, cutoff) {
			return nil
		}
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, current) || !claudePrivateStageEntry(current, true, cutoff) || !info.ModTime().Equal(current.ModTime()) {
		return nil
	}
	if binary != nil {
		currentBinary, err := stage.Lstat("claude")
		if err != nil || !os.SameFile(binary, currentBinary) || !claudePrivateStageEntry(currentBinary, false, cutoff) || binary.Size() != currentBinary.Size() || !binary.ModTime().Equal(currentBinary.ModTime()) {
			return nil
		}
		if err := stage.Remove("claude"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot remove an orphaned private Claude runtime stage")
		}
	}
	// Directory-only removal preserves a replacement file or symlink and refuses
	// to recurse if an unexpected entry appeared during cleanup.
	if err := unix.Unlinkat(int(directory.Fd()), name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) && !errors.Is(err, unix.ENOTDIR) {
		return errors.New("cannot remove an orphaned private Claude runtime staging directory")
	}
	return nil
}
