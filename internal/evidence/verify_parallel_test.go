package evidence_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

const parallelChunkBytes = 100000 // about 100 of the 1,000-byte rows of the scale generator: 14 chunks

// parallelCase holds 1,400 records in two ingests (two audited batches), read in chunks of about 100 rows (the byte cut is lowered).
func parallelCase(t *testing.T) (*evidence.Case, []string) {
	t.Helper()
	t.Cleanup(evidence.SetVerifyChunkBytes(parallelChunkBytes))
	c := recordstest.NewCase(t)
	ids := recordstest.AddScaleArtifacts(t, c, 4)
	gen := recordstest.NewScale(recordstest.ScaleConfig{Seed: 2, Artifacts: ids})
	p := records.Parser{Name: "par", Version: "1"}
	recordstest.Ingest(t, c, p, ids, gen.Block(0, 700))
	recordstest.Ingest(t, c, records.Parser{Name: "par", Version: "2"}, ids, gen.Block(700, 700))
	return c, ids
}

// verifyWith runs Verify with n workers and returns the report, with the audit counter (one entry per verify)
// taken out, and its JSON: the bytes the CLI would print.
func verifyWith(t *testing.T, c *evidence.Case, n int) (evidence.VerifyReport, []byte) {
	t.Helper()
	c.SetVerifyWorkers(n)
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	rep.AuditEntries = 0
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	return rep, b
}

// requireSameReport verifies c with each worker count and requires the report of one worker.
func requireSameReport(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	serial, want := verifyWith(t, c, 1)
	for _, w := range []int{2, 8} {
		got, b := verifyWith(t, c, w)
		if string(b) != string(want) || !reflect.DeepEqual(got, serial) {
			t.Fatalf("workers %d changed the report:\nserial   %s\nparallel %s", w, want, b)
		}
	}
	return serial
}

func requireProblem(t *testing.T, rep evidence.VerifyReport, part string) {
	t.Helper()
	for _, p := range rep.Problems {
		if strings.Contains(p, part) {
			return
		}
	}
	t.Fatalf("no problem contains %q: %q", part, rep.Problems)
}

func TestVerifyParallelEqualsSerial(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		c, _ := parallelCase(t)
		rep := requireSameReport(t, c)
		if !rep.OK() || rep.RecordsChecked != 1400 || rep.RecordBatchesChecked == 0 || rep.FTSDocsChecked == 0 {
			t.Fatalf("the clean case must verify clean: %+v", rep)
		}
	})
	// record damage in both ingests and in the first and last chunks, and damage to both full-text indexes
	t.Run("damaged", func(t *testing.T) {
		c, ids := parallelCase(t)
		recordstest.SetRecordSummary(t, c.Dir, 77, "altered in place")     // first ingest
		recordstest.DeleteRecord(t, c.Dir, 900)                            // second ingest
		recordstest.RepointRecord(t, c.Dir, 1100, ids[1])                  // second ingest
		recordstest.SetRecordSummary(t, c.Dir, 1390, "altered at the end") // the last chunk
		recordstest.HideFromIndex(t, c.Dir, "records_fts", 17)             // a record missing from the word index
		recordstest.HideFromIndex(t, c.Dir, "records_fts_sub", 1150)       // and from the substring index
		recordstest.InventIndexText(t, c.Dir, "records_fts_sub", 5, "zzzz", "yyyy")
		recordstest.InventIndexText(t, c.Dir, "records_fts", 1380, "qqqq", "pppp")
		recordstest.CorruptDocsize(t, c.Dir, "records_fts", 300)
		rep := requireSameReport(t, c)
		if rep.OK() {
			t.Fatal("the damaged case must fail verify")
		}
		requireProblem(t, rep, "digest mismatch")
		requireProblem(t, rep, "records_fts")
	})
}

// The progress counters are right when the chunks are worked on by several workers: the records counter reaches
// the total at the end of records.stream, and every snapshot names its own phase.
func TestVerifyParallelProgressCounters(t *testing.T) {
	t.Cleanup(evidence.SetVerifyChunkBytes(50000)) // about 50 rows per chunk
	c := ftsCase(t, 300)
	c.SetVerifyWorkers(8)
	got := snapshotsAtPhaseEnds(t, c)
	if s := got["records.stream"]; !s.Counted || s.Unit != "records" || s.Done != 300 || s.Total != 300 {
		t.Errorf("records.stream at its end = %+v, want records 300 of 300", s)
	}
	for name, s := range got {
		if s.Phase != name {
			t.Errorf("snapshot at the end of %q names phase %q", name, s.Phase)
		}
	}
}
