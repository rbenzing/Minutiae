//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestRunnerTimeoutStopsTheRecordedProcessGroup(t *testing.T) {
	var rec recorded
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	env := testEnv(t, map[string]string{"HELPER_SLEEP": "120", "HELPER_SPAWN": pidFile}, &rec)
	env.Limit = func(Item) time.Duration { return time.Second }
	start := time.Now()
	res, err := (GoRunner{}).Run(context.Background(), testItem, env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Timeout {
		t.Fatalf("outcome %q, want timeout (note %q)", res.Outcome, res.Note)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("the timeout took %v", time.Since(start))
	}
	if rec.cmd == nil || rec.cmd.Process == nil {
		t.Fatal("the process was not recorded")
	}
	if !processGone(rec.cmd.Process.Pid) {
		t.Errorf("the recorded process %d is still running", rec.cmd.Process.Pid)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the helper recorded no child: %v", err)
	}
	child, _ := strconv.Atoi(string(raw))
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(child) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !processGone(child) {
		t.Errorf("the child %d of the group is still running", child)
	}
}
