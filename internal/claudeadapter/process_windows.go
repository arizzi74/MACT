//go:build windows

package claudeadapter

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
func stopProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
