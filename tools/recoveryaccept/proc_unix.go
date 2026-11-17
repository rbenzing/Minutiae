//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setGroup makes the child the leader of its own process group, so a cancel reaches everything it started.
func setGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killTree kills the process group led by p, by its recorded PID.
func killTree(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
