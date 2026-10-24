package evidence_test

import (
	"context"
	"slices"
	"testing"
	"time"
)

// D116: a cancel that lands after every cancellable phase has finished does not turn a clean verify
// into a failed one.
func TestVerifyCancelAfterEveryPhaseFinishedStaysClean(t *testing.T) {
	c := ftsCase(t, 300)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// fts.index is the last serial phase that reads with the context on a clean case, but P10 runs beside the
	// stream and the index pass and may end after it: the cancel lands only once both have ended (hook calls
	// are serialised, so the map needs no lock).
	ended := map[string]bool{}
	c.SetPhaseHook(func(name string, _ time.Duration) {
		ended[name] = true
		if ended["fts.index"] && ended["records.integrity_check"] {
			cancel()
		}
	})
	rep, err := c.VerifyWith(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("a late cancel failed a clean verify: %q", rep.Problems)
	}
	if slices.Contains(rep.Problems, "reproduce: verification cancelled") {
		t.Fatal("marked cancelled")
	}
}
