//go:build !linux && !darwin

package worker

import (
	"errors"
	"os"
)

func renameArchiveProject(_ *os.File, _, _ string) error {
	return errors.New("this platform cannot atomically stage an archived project for removal")
}
