//go:build windows

package claudeadapter

import "os"

// Windows cannot establish liveness through signal 0; a matching registry
// record is conservatively treated as live until Claude removes it.
func nativeProcessAlive(pid int) bool { _, err := os.FindProcess(pid); return err == nil }

func nativeSessionProcessAlive(pid int, start string) bool { return nativeProcessAlive(pid) }
