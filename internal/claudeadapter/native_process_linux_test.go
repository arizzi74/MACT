package claudeadapter

import (
	"os"
	"strconv"
	"testing"
)

func TestNativeRegistryRejectsRecycledPIDWithoutKillingIt(t *testing.T) {
	pid := os.Getpid()
	start, ok := nativeProcessStart(pid)
	if !ok {
		t.Skip("Linux process start ticks unavailable")
	}
	ticks, err := strconv.ParseUint(start, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !nativeSessionProcessAlive(pid, start) {
		t.Fatal("live matching native PID was ignored")
	}
	if nativeSessionProcessAlive(pid, strconv.FormatUint(ticks+1, 10)) {
		t.Fatal("stale recycled registry PID was treated as the session writer")
	}
	if !nativeSessionProcessAlive(pid, "unknown-format") {
		t.Fatal("unknown registry start format was not checked conservatively")
	}
	if !nativeProcessAlive(pid) {
		t.Fatal("registry liveness check terminated an unrelated process")
	}
}
