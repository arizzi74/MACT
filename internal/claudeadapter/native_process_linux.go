package claudeadapter

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func nativeProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func nativeProcessStart(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", false
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", false
	}
	return fields[19], true
}

// Claude's Linux registry procStart is /proc/<pid>/stat field 22. A matching
// PID with a different start tick belongs to a newer, unrelated process.
func nativeSessionProcessAlive(pid int, start string) bool {
	if !nativeProcessAlive(pid) {
		return false
	}
	if start != "" {
		if _, err := strconv.ParseUint(start, 10, 64); err == nil {
			if current, ok := nativeProcessStart(pid); ok && current != start {
				return false
			}
		}
	}
	return true
}
