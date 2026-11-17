//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
)

// setGroup is a no-op on Windows: killTree takes the whole tree by PID.
func setGroup(*exec.Cmd) {}

// killTree kills p and the processes it started (taskkill /T), then p itself.
func killTree(p *os.Process) error {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run() //nolint:gosec // the PID of our own child
	return p.Kill()
}
