//go:build !linux

package worker

import "golang.org/x/sys/unix"

const webUIDirectoryTraversalFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_DIRECTORY
