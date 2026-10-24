package evidence_test

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func TestVerifyPhaseFeedsTheObserver(t *testing.T) {
	c := evidencetest.NewCase(t)
	p := evidence.NewVerifyProgress()
	var inside evidence.VerifyProgressSnapshot
	check := func(_ context.Context, c *evidence.Case, _ []evidence.ManifestRecord, _ *evidence.VerifyReport) {
		done, add := c.VerifyPhase("x", 10, "bytes")
		add(4)
		inside = p.Snapshot()
		done()
	}
	if _, err := c.VerifyObserved(context.Background(), p, check); err != nil {
		t.Fatal(err)
	}
	if inside.Phase != "checks/x" || !inside.Counted || inside.Done != 4 || inside.Total != 10 || inside.Unit != "bytes" {
		t.Errorf("snapshot inside the check = %+v, want counted checks/x bytes 4 of 10", inside)
	}
	if s := p.Snapshot(); s.Phase != "" || len(s.Also) != 0 {
		t.Errorf("a phase is left running after verify: %+v", s)
	}
}

func TestVerifyPhaseWithoutObserverIsANoOp(t *testing.T) {
	c := evidencetest.NewCase(t)
	ran := false
	check := func(_ context.Context, c *evidence.Case, _ []evidence.ManifestRecord, _ *evidence.VerifyReport) {
		done, add := c.VerifyPhase("x", 10, "bytes")
		add(4)
		done()
		ran = true
	}
	if _, err := c.VerifyWith(context.Background(), check); err != nil || !ran {
		t.Fatalf("ran %v err %v", ran, err)
	}
	// outside any verify it is a no-op too
	done, add := c.VerifyPhase("y", 1, "bytes")
	add(1)
	done()
}
