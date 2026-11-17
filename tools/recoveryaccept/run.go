package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// runner runs one child process and reports its exit code and output. A cancelled ctx kills the child (and what it
// started) and returns an error.
type runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) (code int, stdout, stderr []byte, err error)
}

// execRunner is the real runner. Every child gets CGO_ENABLED=0 (the default release build, and what the build step
// here must produce) and runs in its own process group where the OS has one.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir, name string, args ...string) (int, []byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the tool's own build and the binary it built
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	setGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, so.Bytes(), se.Bytes(), ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), so.Bytes(), se.Bytes(), nil
	}
	if err != nil {
		return -1, so.Bytes(), se.Bytes(), err
	}
	return 0, so.Bytes(), se.Bytes(), nil
}

// binaryName is the file name of the built binary.
func binaryName(goos string) string {
	if goos == "windows" {
		return "minutiae.exe"
	}
	return "minutiae"
}

// state is what the steps share.
type state struct {
	root, scratch, bin, caseDir string
	refs                        map[string]string // fixture name -> its image artifact id; "sqlite" -> the carved database
	before                      []manifestRec     // the manifest before the running step
	oracle                      map[string]json.RawMessage
	stderr                      []byte // stderr of the step that just ran
	out                         io.Writer
}

// added is what the running step wrote to the case.
func (s *state) added() ([]manifestRec, error) {
	now, err := readManifest(s.caseDir)
	if err != nil {
		return nil, err
	}
	return newSince(s.before, now), nil
}

// step is one command of the run: its arguments, the exit code it must give and the check of its output.
type step struct {
	name     string
	args     func(*state) ([]string, error)
	wantExit int
	check    func(*state, []byte) error
}

// removeAll removes dir, retrying briefly (a just-exited child can still hold a file on Windows).
func removeAll(dir string) error {
	var err error
	for range 20 {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// makeParent creates dir and any missing ancestors, returning the ones it created (outermost first) so the cleanup
// removes exactly those and only when they are empty.
func makeParent(dir string) ([]string, error) {
	var missing []string
	for cur := dir; ; {
		if _, err := os.Stat(cur); err == nil {
			break
		}
		missing = append([]string{cur}, missing...)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	var made []string
	for _, d := range missing {
		if err := os.Mkdir(d, 0o700); err != nil {
			return made, err
		}
		made = append(made, d)
	}
	return made, nil
}

// accept runs the acceptance and returns the exit code. It creates <dir>/accept-<unixnano>-<pid> with os.Mkdir (a
// name that exists is a failure: it never reuses or removes a directory it did not create) and removes that
// directory, and any parent it had to create, on every return unless o.keep.
func accept(ctx context.Context, o options, r runner, out io.Writer) (exit int) {
	fail := func(name string, err error) int {
		fmt.Fprintf(out, "ACCEPTANCE FAILED: %s: %v\n", name, err)
		return exitFail
	}
	made, err := makeParent(o.dir)
	removeMade := func() {
		for i := len(made) - 1; i >= 0; i-- {
			_ = os.Remove(made[i]) // an empty directory that this run created
		}
	}
	if err != nil {
		removeMade()
		return fail("scratch", err)
	}
	scratch := filepath.Join(o.dir, fmt.Sprintf("accept-%d-%d", time.Now().UnixNano(), os.Getpid()))
	if err := os.Mkdir(scratch, 0o700); err != nil {
		removeMade()
		return fail("scratch", err)
	}
	defer func() {
		if o.keep {
			return
		}
		if err := removeAll(scratch); err != nil {
			fmt.Fprintf(out, "ACCEPTANCE FAILED: cleanup: %v\n", err)
			exit = exitFail
			return
		}
		removeMade()
	}()
	if o.keep {
		fmt.Fprintf(out, "scratch directory kept: %s\n", scratch)
	}

	st := &state{
		root: o.root, scratch: scratch, bin: o.bin, caseDir: filepath.Join(scratch, "ACCEPT"),
		refs: map[string]string{}, oracle: map[string]json.RawMessage{}, out: out,
	}
	if st.bin == "" {
		st.bin = filepath.Join(scratch, binaryName(runtime.GOOS))
		if o.verbose {
			fmt.Fprintf(out, "$ go build -o %s ./cmd/minutiae\n", st.bin)
		}
		code, _, stderr, err := r.Run(ctx, o.root, "go", "build", "-o", st.bin, "./cmd/minutiae")
		switch {
		case err != nil:
			return fail("build", err)
		case code != 0:
			return fail("build", fmt.Errorf("go build exited %d: %s", code, tail(stderr)))
		}
		fmt.Fprintln(out, "ok   build")
	}

	table := steps()
	if o.steps != nil {
		table = o.steps()
	}
	for i, s := range table {
		if err := ctx.Err(); err != nil {
			return fail(s.name, err)
		}
		args, err := s.args(st)
		if err != nil {
			return fail(s.name, err)
		}
		if i > 0 {
			if st.before, err = readManifest(st.caseDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fail(s.name, err)
			}
		}
		if o.verbose {
			fmt.Fprintf(out, "$ minutiae %s\n", strings.Join(args, " "))
		}
		code, stdout, stderr, err := r.Run(ctx, scratch, st.bin, args...)
		if err != nil {
			return fail(s.name, err)
		}
		st.stderr = stderr
		if code != s.wantExit {
			return fail(s.name, fmt.Errorf("exit %d, want %d: %s", code, s.wantExit, tail(append(stderr, stdout...))))
		}
		if s.check != nil {
			if err := s.check(st, stdout); err != nil {
				return fail(s.name, err)
			}
		}
		fmt.Fprintf(out, "ok   %s (exit %d)\n", s.name, code)
	}
	fmt.Fprintln(out, "ACCEPTANCE PASSED")
	return exitOK
}

// tail is the last 400 bytes of b on one line, for a failure message.
func tail(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) > 400 {
		b = b[len(b)-400:]
	}
	return strings.Join(strings.Fields(string(b)), " ")
}
