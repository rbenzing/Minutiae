//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

var errUnsupported = errors.New("fuzzcampaign: process groups are not supported on Windows")

// setGroup is a no-op on Windows: host mode is refused there, and the runner is only exercised
// by its own tests.
func setGroup(*exec.Cmd) {}

// stopGroup would stop a process group; it is unsupported on Windows.
func stopGroup(int, time.Duration) error { return errUnsupported }

// stopProcess falls back to killing the process itself.
func stopProcess(p *os.Process, grace time.Duration) {
	if stopGroup(p.Pid, grace) != nil {
		_ = p.Kill()
	}
}
