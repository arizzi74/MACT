//go:build !linux && !darwin

package sessionarchive

import "fmt"

func publishDirectory(source, target string) error {
	return fmt.Errorf("%w: atomic directory restoration requires Linux or macOS", ErrUnsupported)
}

func noFollowFlag() int { return 0 }
