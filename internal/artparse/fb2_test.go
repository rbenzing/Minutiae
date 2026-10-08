package artparse_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/parse"
)

// A Probe that ignores cancellation and is abandoned leaves a live goroutine: the run stops, like an
// abandoned Parse, and the next job is not started.
func TestAbandonedProbeStopsTheRun(t *testing.T) {
	f := newRx(t, "pa", "pb")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	pr := &sealedProbe{name: "pa", kept: &rxKept{}, release: release}
	h := f.host(both(shortTimeouts, withLimits(func(l *parse.Limits) { l.ProbeTimeout = 100 * time.Millisecond })), pr, wbp("pb"))
	sum := f.run(h, artparse.Selection{})
	if sum.Stopped != artparse.StoppedAbandoned {
		t.Fatalf("Stopped = %q, want abandoned; jobs %+v", sum.Stopped, sum.Jobs)
	}
	if j := jobOf(t, sum, "pb"); j.Outcome != artparse.OutcomeNotRun {
		t.Errorf("the job after an abandoned Probe: %+v", j)
	}
	if j := jobOf(t, sum, "pa"); j.Outcome != artparse.OutcomeUnparsed || !j.Abandoned || !strings.Contains(j.Reason, "probe failed") {
		t.Errorf("the abandoned job: %+v", j)
	}
}

func TestPlanReportsAnAbandonedProbe(t *testing.T) {
	f := newRx(t, "pa", "pb")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	pr := &sealedProbe{name: "pa", kept: &rxKept{}, release: release}
	h := f.host(both(shortTimeouts, withLimits(func(l *parse.Limits) { l.ProbeTimeout = 100 * time.Millisecond })), pr, wbp("pb"))
	plan, err := h.Plan(context.Background(), artparse.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Stopped != artparse.StoppedAbandoned || len(plan.Rows) != 2 {
		t.Fatalf("plan stopped %q with %d rows, want abandoned with 2", plan.Stopped, len(plan.Rows))
	}
}
