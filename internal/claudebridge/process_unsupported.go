//go:build !darwin && !linux

package claudebridge

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func gracefulProcessGroup(cmd *exec.Cmd) { terminateProcessGroup(cmd) }
