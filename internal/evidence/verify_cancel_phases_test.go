package evidence_test

import (
	"context"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// D117: a cancel at the end of ANY phase, on a clean case, is either a clean cut (the cancelled line
// is the only problem) or, after the last phase that reads with the context, nothing at all. It is
// never a pile of cancel-caused problems that would make the CLI exit 4.
func TestVerifyCancelAtTheEndOfEveryPhase(t *testing.T) {
	c := ftsCase(t, 300)
	phases := []string{
		"audit",
		"artifacts",
		"records.schema",
		"records.integrity_check",
		"fts.selftest",
		"records.stream/fts.expected",
		"records.stream",
		"records.batches",
		"records.lifecycle",
	}
	phases = append(phases, ftsIndexPhases()...) // the index pass of P11, in run order (5D-14i)
	phases = append(phases, "fts.rebuild")
	phases = append(phases, "fts.compare")
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seen := false
			c.SetPhaseHook(func(name string, _ time.Duration) {
				if name == phase {
					seen = true
					cancel()
				}
			})
			defer c.SetPhaseHook(nil)
			rep, err := c.VerifyWith(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !seen {
				t.Fatalf("phase %q never ran", phase)
			}
			if phase == "fts.compare" { // nothing after it reads with the context
				if !rep.OK() {
					t.Fatalf("a cancel after the last cancellable phase failed a clean verify: %q", rep.Problems)
				}
				return
			}
			if !rep.CancelledOnly() {
				t.Fatalf("cancel at the end of %s: problems %q, want only the cancelled line", phase, rep.Problems)
			}
		})
	}
}

// D120: with a composed check (as on the CLI) a cancel after the last cancellable phase is not
// invisible: the composed checks see the ended context, so the verify is a cancelled one.
func TestVerifyCancelAtFTSCompareWithAComposedCheck(t *testing.T) {
	c := ftsCase(t, 300)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.SetPhaseHook(func(name string, _ time.Duration) {
		if name == "fts.compare" {
			cancel()
		}
	})
	defer c.SetPhaseHook(nil)
	trivial := func(context.Context, *evidence.Case, []evidence.ManifestRecord, *evidence.VerifyReport) {}
	rep, err := c.VerifyWith(ctx, trivial)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.CancelledOnly() {
		t.Fatalf("a cancel at fts.compare with a composed check: problems %q, want only the cancelled line", rep.Problems)
	}
}
