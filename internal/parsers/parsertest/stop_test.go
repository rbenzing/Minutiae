package parsertest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// A parser that ignores the stop error and keeps warning must still produce exactly one violation:
// once the cap is hit the emitter is stopped and refuses every later call.
func TestWarnCapStopsAnIgnoringParser(t *testing.T) {
	p := newFn("ignorer", func(ctx context.Context, _ *parse.Input, out parse.Emitter) error {
		for range parse.MaxWarnings + 50 {
			_ = out.Warn(ctx, "text:line=1", "w") // the error is ignored on purpose
		}
		return nil
	})
	h, art, _ := bundle(t, t, "ignorer", "x\n")
	h.warnSink = func(context.Context, string, string) error { return nil }
	res := h.RunLenient(p, h.Input(p, art, nil, false))
	if len(res.Violations) != 1 || !errors.Is(res.Violations[0], parse.ErrWarningCap) {
		t.Errorf("violations %v, want exactly one ErrWarningCap", res.Violations)
	}
	if len(res.Warnings) != parse.MaxWarnings {
		t.Errorf("kept %d warnings, want %d", len(res.Warnings), parse.MaxWarnings)
	}
}

// ProbeBytes is one budget per Probe call: the primary reader and a Lookuper open share it, as under the host.
func TestProbeBudgetIsSharedWithLookupOpens(t *testing.T) {
	p := newFn("probe", func(context.Context, *parse.Input, parse.Emitter) error { return nil })
	h, art, _ := bundle(t, t, "probe", strings.Repeat("x", 2<<20))
	in := h.Input(p, art, nil, true)
	lim := int(parse.DefaultLimits().ProbeBytes)
	if n, err := in.Primary.R.ReadAt(make([]byte, lim-100), 0); n != lim-100 || err != nil {
		t.Fatalf("primary read: %d %v", n, err)
	}
	r, err := in.Lookup.Open(art)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadAt(make([]byte, 200), 0); n != 100 || !errors.Is(err, parse.ErrProbeLimit) {
		t.Errorf("a lookup open during Probe: n=%d err=%v, want 100 and ErrProbeLimit", n, err)
	}
}
