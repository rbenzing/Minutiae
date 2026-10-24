package evidence

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// inFlightTokenizer wraps the replicas and records how many of them run at the same moment.
func inFlightTokenizer(peak *atomic.Int64, onText func(string)) func(string) func(string, func([]byte, int)) {
	var now atomic.Int64
	return func(table string) func(string, func([]byte, int)) {
		base := tokenizeUnicode61
		if table == FTSSubTable {
			base = tokenizeTrigram
		}
		return func(text string, emit func([]byte, int)) {
			n := now.Add(1)
			defer now.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			if onText != nil {
				onText(text)
			}
			time.Sleep(20 * time.Microsecond) // lets the workers overlap whatever the number of CPUs
			base(text, emit)
		}
	}
}

// The per-row work of a chunk runs on the configured number of workers: one runs one at a time, more run
// together and never more than asked for.
func TestVerifyUsesTheConfiguredWorkers(t *testing.T) {
	c := indexedTextCase(t, 400)
	for _, w := range []int{1, 2, 8} {
		var peak atomic.Int64
		restore := SetFTSReplicaOverride(inFlightTokenizer(&peak, nil))
		c.SetVerifyWorkers(w)
		_, err := c.Verify()
		restore()
		if err != nil {
			t.Fatalf("workers %d: %v", w, err)
		}
		got := int(peak.Load())
		switch {
		case w == 1 && got != 1:
			t.Fatalf("one worker ran %d tokenizers at once", got)
		case w > 1 && (got < 2 || got > w):
			t.Fatalf("%d workers ran %d tokenizers at once, want between 2 and %d", w, got, w)
		}
	}
	c.SetVerifyWorkers(0)
	if got, want := c.verifyWorkers(), min(runtime.GOMAXPROCS(0), 8); got != want {
		t.Fatalf("default workers = %d, want %d", got, want)
	}
}

// A worker that panics while it computes the per-row work fails the verify: a problem, not a crash, not a clean
// report, and not an index that counts as checked.
func TestVerifyWorkerPanicFailsClosed(t *testing.T) {
	c, rec := caseWithArtifact(t)
	insertTextRecords(t, c, rec.ID, 300, func(i int) (string, string) {
		if i == 150 {
			return "hostile panicmark row", ""
		}
		return ftsText(i)
	})
	if _, err := c.ReindexText(context.Background(), ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	var peak atomic.Int64
	restore := SetFTSReplicaOverride(inFlightTokenizer(&peak, func(text string) {
		if strings.Contains(text, "panicmark") {
			panic("hostile row")
		}
	}))
	defer restore()
	for _, w := range []int{1, 4} {
		c.SetVerifyWorkers(w)
		rep, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK() || rep.CancelledOnly() {
			t.Fatalf("workers %d: a failed worker must fail verify: %q", w, rep.Problems)
		}
		if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "could not be computed") }) {
			t.Fatalf("workers %d: no problem names the failed worker: %q", w, rep.Problems)
		}
		if rep.FTSDocsChecked != 0 {
			t.Fatalf("workers %d: %d documents counted as checked after a failed worker", w, rep.FTSDocsChecked)
		}
	}
}

// A cancel while the workers run is a clean cut (D111-D120): the integrity problem found earlier stays in the
// report and the cancel adds no noise.
func TestVerifyParallelCancelKeepsFindings(t *testing.T) {
	for _, w := range []int{1, 8} {
		c := indexedTextCase(t, 12000) // three record chunks of 5000, 5000, 2000
		recs, err := c.Manifest()
		if err != nil || len(recs) == 0 {
			t.Fatalf("manifest: %v %d", err, len(recs))
		}
		path := filepath.Join(c.Dir, filepath.FromSlash(recs[0].Path))
		if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil { //nolint:gosec // a test edits its own temp case
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		chunks := 0
		c.SetVerifyWorkers(w)
		rep, err := c.verifyRun(ctx, func(table string, _ int) {
			if table == "records" {
				if chunks++; chunks == 2 {
					cancel() // the second chunk is read; the third must not be
				}
			}
		}, VerifyOptions{})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if chunks != 2 {
			t.Fatalf("workers %d: %d chunks were read, want 2", w, chunks)
		}
		if rep.OK() || rep.CancelledOnly() || !slices.Contains(rep.Problems, verifyCancelled) {
			t.Fatalf("workers %d: %q", w, rep.Problems)
		}
		if !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "hash mismatch") }) {
			t.Fatalf("workers %d: the integrity problem found before the cancel is missing: %q", w, rep.Problems)
		}
		for _, p := range rep.Problems {
			if strings.Contains(p, "context canceled") || strings.Contains(p, "could not") || strings.Contains(p, "unreadable") {
				t.Fatalf("workers %d: cancel noise in the report: %q", w, p)
			}
		}
		if rep.FTSDocsChecked != 0 {
			t.Fatalf("workers %d: FTSDocsChecked = %d after a cut", w, rep.FTSDocsChecked)
		}
	}
}
