package records_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestIngestAuditCostIsPerBatch pins the shape of the ingest write path without a clock: the audit log
// (one fsync per entry) grows with the number of batches, never with the number of records.
func TestIngestAuditCostIsPerBatch(t *testing.T) {
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 4)
	gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 3, Artifacts: ids})
	w := startWriter(t, c, records.Parser{Name: "cost", Version: "1"}, records.WriterOptions{BatchRows: 500}, ids...)
	add(t, w, gen.Block(0, 2500))
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, e := range auditOf(t, c, "") {
		if strings.HasPrefix(e.Action, "records.") {
			got[e.Action]++
		}
	}
	want := map[string]int{"records.ingest.start": 1, "records.batch": 5, "records.ingest.end": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records.* audit entries for 2500 records in batches of 500: %v, want %v", got, want)
	}
}

// benchDir is where BenchmarkIngest puts its case: the 200,000-record runs of Task 5 write several
// hundred megabytes, which must land under the session scratchpad (ruling D15), so the run passes
// -benchdir. Without the flag the case is in b.TempDir(), which is only for the small default sizes.
var benchDir = flag.String("benchdir", "", "directory for the benchmark case (default b.TempDir())")

func benchCase(b *testing.B, name string) *evidence.Case {
	b.Helper()
	if *benchDir == "" {
		return recordstest.NewCase(b)
	}
	parent := filepath.Join(*benchDir, name)
	if err := os.RemoveAll(parent); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		b.Fatal(err)
	}
	c, err := evidence.Create(parent, evidence.CreateOptions{ID: "BENCH", Examiner: "Bench"})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close(); _ = os.RemoveAll(parent) })
	return c
}

func BenchmarkIngest(b *testing.B) {
	for _, rows := range []int{1000, 5000, 20000} {
		b.Run(fmt.Sprintf("batch%d", rows), func(b *testing.B) {
			c := benchCase(b, fmt.Sprintf("ingest%d", rows))
			ids := recordstest.AddScaleArtifacts(b, c, 6)
			gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 1, Artifacts: ids})
			// BatchBytes is raised so that the row count, not the 32 MiB default, ends a batch of 20,000
			// rows of about 1.3 KiB: the label says what is measured.
			w, err := records.NewWriter(c, records.Parser{Name: "bench", Version: "1"}, records.WriterOptions{BatchRows: rows, BatchBytes: 512 << 20})
			if err != nil {
				b.Fatal(err)
			}
			if err := w.Start(ctx, records.StartOptions{Artifacts: ids}); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for done := 0; done < b.N; {
				n := min(rows, b.N-done)
				b.StopTimer() // the generator is the parser's CPU: excluded, as in the spec's target
				blk := gen.Block(int64(done), n)
				b.StartTimer()
				for _, r := range blk {
					if err := w.Add(ctx, r); err != nil {
						b.Fatal(err)
					}
				}
				done += n
			}
			if _, err := w.End(ctx); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "records/s")
		})
	}
}
