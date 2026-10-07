//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// killGroup makes a cancelled command stop its whole process group, so
// programs that start helpers (ORCA does) do not leave them running.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
