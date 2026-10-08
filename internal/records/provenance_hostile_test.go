package records_test

import (
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func liveHeap() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// peakHeap samples the live heap every few milliseconds until stop is called and returns the peak.
func peakHeap() (stop func() uint64) {
	var (
		mu   sync.Mutex
		peak uint64
		done = make(chan struct{})
		fin  = make(chan struct{})
	)
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() {
		metrics.Read(sample)
		if sample[0].Value.Kind() == metrics.KindUint64 {
			mu.Lock()
			peak = max(peak, sample[0].Value.Uint64())
			mu.Unlock()
		}
	}
	go func() {
		defer close(fin)
		for {
			read()
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-fin
		read()
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

// A runs sidecar at the cap (1,048,576 valid lines) is read, hashed and translated within a bounded
// heap and time: the run slice itself is 16 MiB, and nothing retained grows with the line count
// beyond it (the line buffer is fixed).
func TestSidecarReadIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 29 MB runs sidecar")
	}
	const n = evidence.MaxRecoveredRuns
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", make([]byte, n))
	side := func() evidence.ManifestRecord { // the run slice is garbage once the sidecar is written
		runs := make([]evidence.Run, n)
		for i := range runs {
			runs[i] = evidence.Run{Offset: int64(i), Length: 1}
		}
		return recordstest.AddRunsSidecar(t, c, img, "f.runs.jsonl", runs)
	}()
	src := extractSrc(img, evidence.Derivation{RunsArtifact: side.ID})
	f := recordstest.AddDerivedWith(t, c, "f.bin", src, make([]byte, n))
	res := recordstest.Ingest(t, c, testParser, []string{f.ID}, []records.Record{offsetRec(f.ID, &records.Range{Offset: 500000, Length: 100})})
	r := newReader(t, c)
	runtime.GC()
	base := liveHeap()

	stop := peakHeap()
	start := time.Now()
	full, err := r.Get(ctx, res.FirstID)
	elapsed := time.Since(start)
	peak := stop()
	if err != nil {
		t.Fatal(err)
	}
	requireTranslated(t, full.Provenance)
	if im := full.Provenance.Offset.Image; len(im) != 100 || im[0].ImageOffset != 500000 || im[99].ImageOffset != 500099 {
		t.Fatalf("%d image extents, want 100 one-byte extents from 500000", len(full.Provenance.Offset.Image))
	}
	if !full.Provenance.OK() {
		t.Fatalf("problems %+v", full.Provenance.Problems)
	}
	t.Logf("Get over %d sidecar lines took %s, peak live heap %d MiB over a base of %d MiB", n, elapsed, peak>>20, base>>20)
	if elapsed > 60*time.Second {
		t.Errorf("Get took %s", elapsed)
	}
	// The budget is relative: the heap above its GC'd baseline. The 16 MiB run slice is the only
	// retained growth; per-line allocations kept live (about 1M lines) would exceed the margin.
	const margin = 64 << 20
	budget := base + margin
	if peak > budget {
		t.Errorf("peak live heap %d MiB exceeds the %d MiB budget (base %d MiB + %d MiB)", peak>>20, budget>>20, base>>20, margin>>20)
	}
}
