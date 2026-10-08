package cli

import (
	"context"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// The run lowers the garbage collector's memory limit to twice the budget, never raises an
// operator's lower GOMEMLIMIT, and puts the old value back.
func TestParseRunNeverRaisesAnOperatorMemoryLimit(t *testing.T) {
	const operator = int64(512) << 20
	prev := debug.SetMemoryLimit(operator)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	var during int64
	p := good("ml")
	p.parse = func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
		during = debug.SetMemoryLimit(-1)
		return p.WellBehaved.Parse(ctx, in, out)
	}
	pf := newPfx(t, "ml")
	code, _, errOut := runP(t, registry(t, "", p), "parse", "run", "--case", pf.dir, "--mem-budget", "1GiB")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if during != operator {
		t.Errorf("memory limit during the run = %d, want the operator's %d (2 x budget is 2 GiB, higher)", during, operator)
	}
	if after := debug.SetMemoryLimit(-1); after != operator {
		t.Errorf("memory limit after the run = %d, want it restored to %d", after, operator)
	}
}

func TestParseRunLowersAnUnsetMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(1<<63 - 1) // no limit
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	var during int64
	p := good("mu")
	p.parse = func(ctx context.Context, in *parse.Input, out parse.Emitter) error {
		during = debug.SetMemoryLimit(-1)
		return p.WellBehaved.Parse(ctx, in, out)
	}
	pf := newPfx(t, "mu")
	if code, _, errOut := runP(t, registry(t, "", p), "parse", "run", "--case", pf.dir, "--mem-budget", "1GiB"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if want := int64(2) << 30; during != want {
		t.Errorf("memory limit during the run = %d, want 2 x budget = %d", during, want)
	}
	if after := debug.SetMemoryLimit(-1); after != 1<<63-1 {
		t.Errorf("memory limit after the run = %d, want it unset again", after)
	}
}

func TestParseRunHelpListsExitCodeTwo(t *testing.T) {
	code, out, errOut := runP(t, registry(t, "", good("h")), "parse", "run", "--help")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "2 ") || !strings.Contains(out, "usage") {
		t.Errorf("the help does not list exit code 2:\n%s", out)
	}
}
