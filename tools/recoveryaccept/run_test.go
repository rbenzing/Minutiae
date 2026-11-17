package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner answers every child with exit 0 and no output, except as configured. It records the calls.
type fakeRunner struct {
	calls    []string
	failAt   int    // the 1-based call that exits with failCode (0: none)
	cancel   func() // called at call cancelAt
	cancelAt int
}

func (f *fakeRunner) Run(ctx context.Context, _ string, name string, args ...string) (int, []byte, []byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	n := len(f.calls)
	if f.cancel != nil && n == f.cancelAt {
		f.cancel()
		return -1, nil, nil, ctx.Err()
	}
	if f.failAt == n {
		return 3, nil, []byte("boom"), nil
	}
	return 0, nil, nil, nil
}

// trivialSteps is a table of n steps that need nothing from the case.
func trivialSteps(n int) func() []step {
	return func() []step {
		var t []step
		for i := range n {
			t = append(t, step{
				name: "step-" + string(rune('a'+i)),
				args: func(*state) ([]string, error) { return []string{"noop"}, nil },
			})
		}
		return t
	}
}

func testOptions(t *testing.T, dir string) options {
	t.Helper()
	return options{root: moduleRoot(t), dir: dir, steps: trivialSteps(4)}
}

func TestCleanupRemovesOnlyItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keepme", "f"), "x")
	var out bytes.Buffer
	if code := accept(context.Background(), testOptions(t, dir), &fakeRunner{}, &out); code != 0 {
		t.Fatalf("accept: %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "ACCEPTANCE PASSED") {
		t.Errorf("no ACCEPTANCE PASSED:\n%s", out.String())
	}
	if got := strings.Join(listing(t, dir), "|"); got != ".|keepme|keepme"+string(filepath.Separator)+"f" {
		t.Errorf("the directory holds %q after the run, want only keepme/f", got)
	}
	// with -keep the directory stays and its path is printed
	o := testOptions(t, dir)
	o.keep = true
	out.Reset()
	if code := accept(context.Background(), o, &fakeRunner{}, &out); code != 0 {
		t.Fatalf("accept -keep: %d\n%s", code, out.String())
	}
	var kept []string
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "accept-") {
			kept = append(kept, filepath.Join(dir, e.Name()))
		}
	}
	if len(kept) != 1 || !strings.Contains(out.String(), kept[0]) {
		t.Errorf("kept %v, output:\n%s", kept, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "keepme", "f")); err != nil {
		t.Errorf("the sibling is gone: %v", err)
	}
}

// A parent directory that did not exist is created for the run and removed with it.
func TestMissingParentIsCreatedAndRemoved(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b")
	var out bytes.Buffer
	if code := accept(context.Background(), testOptions(t, dir), &fakeRunner{}, &out); code != 0 {
		t.Fatalf("accept: %d\n%s", code, out.String())
	}
	if got := listing(t, base); len(got) != 1 {
		t.Errorf("%v remains of the parent directories", got)
	}
}

func TestFailureAndCancelLeaveNothingBehind(t *testing.T) {
	// (a) a child exits 3: exit 1, the step is named, nothing is left
	t.Run("a failing step", func(t *testing.T) {
		dir := t.TempDir()
		var out bytes.Buffer
		// call 1 is the build, calls 2.. are the steps: call 3 is the second step
		if code := accept(context.Background(), testOptions(t, dir), &fakeRunner{failAt: 3}, &out); code != 1 {
			t.Fatalf("accept: %d, want 1\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), "ACCEPTANCE FAILED: step-b") || !strings.Contains(out.String(), "boom") {
			t.Errorf("the failure does not name the step and its reason:\n%s", out.String())
		}
		if got := listing(t, dir); len(got) != 1 {
			t.Errorf("%v left behind", got)
		}
	})
	// (b) the context is cancelled during a step
	t.Run("a cancelled run", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var out bytes.Buffer
		r := &fakeRunner{cancel: cancel, cancelAt: 4}
		if code := accept(ctx, testOptions(t, dir), r, &out); code != 1 {
			t.Fatalf("accept: %d, want 1\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), "ACCEPTANCE FAILED: step-c") {
			t.Errorf("the output does not name the interrupted step:\n%s", out.String())
		}
		if got := listing(t, dir); len(got) != 1 {
			t.Errorf("%v left behind", got)
		}
		if len(r.calls) != 4 {
			t.Errorf("%d children started, want 4 (nothing after the cancel)", len(r.calls))
		}
	})
	// (c) the build fails
	t.Run("a failing build", func(t *testing.T) {
		dir := t.TempDir()
		var out bytes.Buffer
		if code := accept(context.Background(), testOptions(t, dir), &fakeRunner{failAt: 1}, &out); code != 1 || !strings.Contains(out.String(), "ACCEPTANCE FAILED: build") {
			t.Fatalf("accept: %d\n%s", code, out.String())
		}
		if got := listing(t, dir); len(got) != 1 {
			t.Errorf("%v left behind", got)
		}
	})
	// (d) -bin skips the build
	t.Run("bin skips the build", func(t *testing.T) {
		dir := t.TempDir()
		o := testOptions(t, dir)
		o.bin = filepath.Join(dir, "elsewhere")
		r := &fakeRunner{}
		var out bytes.Buffer
		if code := accept(context.Background(), o, r, &out); code != 0 {
			t.Fatalf("accept: %d\n%s", code, out.String())
		}
		for _, c := range r.calls {
			if strings.HasPrefix(c, "go ") {
				t.Errorf("a build ran with -bin: %s", c)
			}
		}
		if len(r.calls) != 4 {
			t.Errorf("%d children, want 4", len(r.calls))
		}
	})
}

// A step whose exit code differs from its pin fails, whichever way.
func TestWrongExitCodeFails(t *testing.T) {
	dir := t.TempDir()
	o := testOptions(t, dir)
	o.steps = func() []step {
		return []step{{name: "expects-four", wantExit: 4, args: func(*state) ([]string, error) { return []string{"x"}, nil }}}
	}
	var out bytes.Buffer
	if code := accept(context.Background(), o, &fakeRunner{}, &out); code != 1 || !strings.Contains(out.String(), "expects-four: exit 0, want 4") {
		t.Errorf("accept: %d\n%s", code, out.String())
	}
}
