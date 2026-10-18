//go:build linux

package sessionarchive

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
)

func publishDirectory(source, target string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%w: project appeared during restore", ErrConflict)
	}
	return err
}

func noFollowFlag() int { return unix.O_NOFOLLOW | unix.O_NONBLOCK }
