//go:build !windows && !linux

package claudeadapter

import "syscall"

func nativeProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func nativeSessionProcessAlive(pid int, start string) bool { return nativeProcessAlive(pid) }
