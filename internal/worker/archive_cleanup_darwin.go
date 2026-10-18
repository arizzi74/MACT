//go:build darwin

package worker

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameArchiveProject(parent *os.File, source, target string) error {
	return unix.RenameatxNp(int(parent.Fd()), source, int(parent.Fd()), target, unix.RENAME_EXCL)
}
