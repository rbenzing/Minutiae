//go:build windows

package main

import (
	"os"
	"os/exec"
	"time"
)

// setGroup is a no-op on Windows: host mode is refused there (HostModeAllowed), so the runner
// is only exercised by its own tests, which do not use process groups.
func setGroup(*exec.Cmd) {}

// stopProcess kills the process itself; there is no process group to stop, and the grace is
// not used.
func stopProcess(p *os.Process, _ time.Duration) { _ = p.Kill() }
