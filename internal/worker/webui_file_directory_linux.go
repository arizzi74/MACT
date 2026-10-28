package worker

import "golang.org/x/sys/unix"

// Traversal handles need no directory-listing permission. O_DIRECTORY with
// O_NOFOLLOW still rejects symlinks, including with O_PATH on Linux.
const webUIDirectoryTraversalFlags = unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_DIRECTORY
