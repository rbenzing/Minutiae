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
	c.SetPhaseHook(func(name string, _ time.Duration) {
		if name == "fts.index" { // the last phase that reads with the context on a clean case
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
