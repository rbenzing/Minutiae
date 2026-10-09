package evidence_test

import (
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func TestVerifyPhasesAreReportedOnceEach(t *testing.T) {
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 4)
	gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 1, Artifacts: ids})
	recordstest.Ingest(t, c, records.Parser{Name: "ph", Version: "1"}, ids, gen.Block(0, 50))
	var seen []string
	c.SetPhaseHook(func(name string, d time.Duration) {
		if d < 0 {
			t.Errorf("phase %s has a negative duration", name)
		}
		seen = append(seen, name)
	})
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %v", err, rep.Problems)
	}
	for _, want := range []string{"audit", "artifacts", "records.schema", "records.integrity_check", "records.stream", "records.batches", "records.lifecycle", "fts.rebuild", "fts.compare"} {
		if n := countOf(seen, want); n != 1 {
			t.Errorf("phase %q reported %d times in %v", want, n, seen)
		}
	}
}

func countOf(ss []string, s string) int {
	n := 0
	for _, x := range ss {
		if x == s {
			n++
		}
	}
	return n
}
