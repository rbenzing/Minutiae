package evidence_test

import (
	"slices"
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
	for _, want := range append([]string{"audit", "artifacts", "records.schema", "records.integrity_check", "fts.selftest", "records.stream", "records.stream/fts.expected", "records.batches", "records.lifecycle", "fts.rebuild", "fts.compare"}, ftsIndexPhases()...) {
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

// ftsIndexSteps are the steps of the index pass of P11 for each table, in run order (5D-14i).
var ftsIndexSteps = []string{"decode", "totals", "config", "unreferenced"}

// ftsIndexPhases lists the phases of the index pass in run order: the steps of each table, then fts.index.
func ftsIndexPhases() []string {
	var out []string
	for _, table := range []string{"records_fts", "records_fts_sub"} {
		for _, step := range ftsIndexSteps {
			out = append(out, "fts.index/"+table+"/"+step)
		}
	}
	return append(out, "fts.index")
}

// 5D-14b, 5D-14i: every step of P11 is timed on its own, inside its parent. A clean case runs the steps of the
// index pass; the vocab comparison runs only for a table
// that has a failing bucket (TestVerifyFTSTamperedPostingsRunTheVocabCompare).
func TestVerifyFTSCompareSubPhasesAreTimed(t *testing.T) {
	c := ftsCase(t, 300)
	var seen []string
	c.SetPhaseHook(func(name string, _ time.Duration) { seen = append(seen, name) })
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("verify: %v %v", err, rep.Problems)
	}
	index, compare := slices.Index(seen, "fts.index"), slices.Index(seen, "fts.compare")
	if index < 0 || compare < 0 {
		t.Fatalf("fts.index or fts.compare missing from %v", seen)
	}
	for _, table := range []string{"records_fts", "records_fts_sub"} {
		for _, step := range ftsIndexSteps {
			name := "fts.index/" + table + "/" + step
			if n := countOf(seen, name); n != 1 {
				t.Errorf("sub-phase %q reported %d times in %v", name, n, seen)
			}
			if i := slices.Index(seen, name); i > index {
				t.Errorf("sub-phase %q ends after fts.index", name)
			}
		}
		if n := countOf(seen, "fts.compare/"+table+"/reachability"); n != 0 {
			t.Errorf("the reachability pass of %s still runs", table)
		}
		if n := countOf(seen, "fts.compare/"+table+"/vocab"); n != 0 {
			t.Errorf("the vocab comparison ran for %s on a clean case", table)
		}
	}
	if stream, exp := slices.Index(seen, "records.stream"), slices.Index(seen, "records.stream/fts.expected"); exp < 0 || exp > stream {
		t.Errorf("records.stream/fts.expected is missing or ends after records.stream in %v", seen)
	}
	if st, stream := slices.Index(seen, "fts.selftest"), slices.Index(seen, "records.stream"); st < 0 || st > stream {
		t.Errorf("fts.selftest is missing or runs after records.stream in %v", seen)
	}
}
