package examine

import (
	"math/rand"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// FB-m: planning memory is bounded. At the plan cap (the most runs one plan holds) the overlap passes,
// the largest consumer of the plan, peak at or under planMemoryBudget.
func TestOverlapPeakHeapAtPlanCapStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates the plan cap worth of runs")
	}
	const planMemoryBudget = 512 << 20
	n := maxPlanRuns
	items := make([]ownedRuns, 0, n/4)
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test data
	for i := 0; i < n/4; i++ {
		rs := make([]evidence.Run, 0, 4)
		for k := 0; k < 4; k++ {
			rs = append(rs, evidence.Run{Offset: int64(rng.Intn(1 << 40)), Length: int64(1 + rng.Intn(1<<20))})
		}
		items = append(items, ownedRuns{Owner: "o" + strconv.Itoa(i), Runs: rs})
	}
	runtime.GC()
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	_ = overlapsOf(items)
	_ = overlapCounts(items)
	close(stop)
	<-done
	t.Logf("%d runs: peak heap %d MiB", n, peak.Load()>>20)
	if p := peak.Load(); p > planMemoryBudget {
		t.Errorf("peak heap %d MiB at %d runs exceeds the %d MiB budget; lower maxPlanRuns", p>>20, n, planMemoryBudget>>20)
	}
}
