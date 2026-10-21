//go:build unix

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setGroup makes the child the leader of its own process group, so a stop reaches everything
// it started and nothing else.
func setGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// stopProcess stops the process group led by p by its recorded PID: SIGTERM, then SIGKILL
// after grace if the group is still there. It never matches processes by name.
func stopProcess(p *os.Process, grace time.Duration) {
	pgid := -p.Pid
	if grace > 0 {
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			if errors.Is(syscall.Kill(pgid, 0), syscall.ESRCH) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}
