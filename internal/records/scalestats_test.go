package records_test

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// pct is the nearest-rank percentile (p in 0..1) of samples in milliseconds.
func pct(ms []float64, p float64) float64 {
	if len(ms) == 0 {
		return math.NaN()
	}
	s := slices.Clone(ms)
	slices.Sort(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// timeIt runs f three times unmeasured (warm cache), then up to n times measured, and returns the
// durations in milliseconds. It stops early once budget has passed (always after at least one sample),
// so one pathological cell cannot hold a run for hours; the caller records len(result). A failing f
// ends the measurement.
func timeIt(n int, budget time.Duration, f func(i int) error) ([]float64, error) {
	for i := 0; i < 3; i++ {
		if err := f(i); err != nil {
			return nil, err
		}
	}
	out := make([]float64, 0, n)
	start := time.Now()
	for i := 0; i < n && (i == 0 || time.Since(start) < budget); i++ {
		t0 := time.Now()
		if err := f(i + 3); err != nil {
			return nil, err
		}
		out = append(out, float64(time.Since(t0).Microseconds())/1000)
	}
	return out, nil
}

func TestPercentileNearestRank(t *testing.T) {
	ms := make([]float64, 100)
	for i := range ms {
		ms[i] = float64(100 - i) // unsorted on purpose
	}
	for _, c := range []struct{ p, want float64 }{{0.95, 95}, {0.5, 50}, {1, 100}, {0.0001, 1}} {
		if got := pct(ms, c.p); got != c.want {
			t.Errorf("pct(%v) = %v, want %v", c.p, got, c.want)
		}
	}
	if !math.IsNaN(pct(nil, 0.95)) {
		t.Error("an empty sample has no percentile")
	}
}

// The budget is checked after each measured call, so a zero budget gives exactly one sample and no
// budget limit gives n; the call counts are deterministic (no clock is asserted).
func TestTimeItCallCounts(t *testing.T) {
	calls := 0
	got, err := timeIt(5, time.Hour, func(int) error { calls++; return nil })
	if err != nil || len(got) != 5 || calls != 3+5 {
		t.Fatalf("n=5: %d samples, %d calls, %v", len(got), calls, err)
	}
	calls = 0
	got, err = timeIt(5, 0, func(int) error { calls++; return nil })
	if err != nil || len(got) != 1 || calls != 3+1 {
		t.Fatalf("zero budget: %d samples, %d calls, %v", len(got), calls, err)
	}
	boom := errTimeIt
	if _, err := timeIt(5, time.Hour, func(int) error { return boom }); err != boom {
		t.Fatalf("a failing call must end the measurement, got %v", err)
	}
}

var errTimeIt = &timeItError{}

type timeItError struct{}

func (*timeItError) Error() string { return "timeIt test error" }

// topLevelPhaseSum adds the wall time of the top-level phases. A name "<parent>/<table>/<step>" is nested inside
// its parent and already counted there; a phase inside the overlap span of a concurrent verify
// (evidence.VerifyPhaseOverlapped) may run beside another, so only the span, which is wall time, is counted for them
// (5D-15c, D183).
func topLevelPhaseSum(phases map[string]float64) float64 {
	sum := 0.0
	for p, s := range phases {
		if !strings.Contains(p, "/") && !evidence.VerifyPhaseOverlapped(p) {
			sum += s
		}
	}
	return sum
}

func TestTopLevelPhaseSumSkipsSubPhases(t *testing.T) {
	if got := topLevelPhaseSum(map[string]float64{"a": 1, "b": 2, "b/x/y": 5}); got != 3 {
		t.Fatalf("topLevelPhaseSum = %v, want 3", got)
	}
}

func TestTopLevelPhaseSumCountsTheOverlapSpanNotThePhasesInIt(t *testing.T) {
	phases := map[string]float64{
		"audit": 1, "artifacts": 2,
		evidence.VerifyOverlapPhase: 10, "records.integrity_check": 9, "records.stream": 4, "fts.index": 5, "fts.index/t/decode": 3,
	}
	if got := topLevelPhaseSum(phases); got != 13 {
		t.Fatalf("topLevelPhaseSum = %v, want 13 (audit, artifacts and the span)", got)
	}
}
