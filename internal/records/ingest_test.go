package records_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// TestRecoverUnfinishedIngest simulates a dead process: the hook panics right
// after a batch was audited, and Abort is never called.
func TestRecoverUnfinishedIngest(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	crashAt(w, "after-batch-audit", 2)
	recs := recordstest.Records(a.ID, 3, 1)
	if survive(t, func() { add(t, w, recs[:2]) }) { // batch 1 commits
		t.Fatal("crashed too early")
	}
	if !survive(t, func() {
		if err := w.Add(ctx, recs[2]); err != nil {
			t.Error(err)
		}
		_ = w.Flush(ctx) // batch 2 is audited, then the process "dies"
	}) {
		t.Fatal("the crash seam never fired")
	}
	if n := count(t, c, "record_runs"); n != 0 {
		t.Fatalf("%d runs before recovery", n)
	}

	w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	recs2 := auditOf(t, c, evidence.ActionIngestRecover)
	if len(recs2) != 1 {
		t.Fatalf("%d records.ingest.recover entries after the next Start, want 1", len(recs2))
	}
	rc := details[evidence.IngestRecover](t, recs2[0])
	b1 := details[evidence.BatchCommit](t, auditOf(t, c, evidence.ActionBatch)[0])
	if rc.IngestID != w.IngestID() || rc.ByIngestID != w2.IngestID() || rc.Outcome != "interrupted" || rc.RunMissing ||
		!reflect.DeepEqual(rc.BatchNos, []int{2}) || rc.Batches != 1 || rc.Records != 2 || rc.FirstID != 1 || rc.LastID != 2 ||
		rc.Rollup != evidence.IngestRollup([]string{b1.Digest}) || rc.Reason == "" {
		t.Errorf("recover entry = %+v", rc)
	}
	// the recover entry comes before the new ingest's start entry
	starts := auditOf(t, c, evidence.ActionIngestStart)
	if len(starts) != 2 || starts[1].Seq < recs2[0].Seq {
		t.Errorf("recover (seq %d) must precede the second start (%v)", recs2[0].Seq, starts)
	}
	var outcome string
	var endSeq int64
	var batches, nrec int
	if err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		return h.QueryRow(`SELECT outcome, end_seq, batches, records FROM record_runs WHERE ingest_id = ?`, w.IngestID()).Scan(&outcome, &endSeq, &batches, &nrec)
	}); err != nil {
		t.Fatal(err)
	}
	if outcome != "interrupted" || endSeq != recs2[0].Seq || batches != 1 || nrec != 2 {
		t.Errorf("run = %s end_seq %d batches %d records %d", outcome, endSeq, batches, nrec)
	}
	if n := scalar[int](t, c, `SELECT count(*) FROM record_run_artifacts WHERE ingest_id = ?`, w.IngestID()); n != 1 {
		t.Errorf("%d coverage rows for the recovered run", n)
	}
	if _, err := w2.End(ctx); err != nil {
		t.Fatal(err)
	}
	// a second Start recovers nothing
	startWriter(t, c, records.Parser{Name: "third", Version: "1"}, records.WriterOptions{}, a.ID)
	if n := len(auditOf(t, c, evidence.ActionIngestRecover)); n != 1 {
		t.Errorf("%d recover entries after another Start, want still 1", n)
	}
	if un, err := c.UnresolvedIngests(); err != nil || len(un) != 1 { // only the third, still running
		t.Errorf("unresolved = %v, %v", un, err)
	}
}

func TestRecoverRunMissing(t *testing.T) {
	c, a := setup(t)
	old := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	w := startWriter(t, c, records.Parser{Name: "p", Version: "2"}, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 4, 2))
	crashAt(w, "after-end-audit", 1)
	if !survive(t, func() { _, _ = w.End(ctx) }) {
		t.Fatal("the crash seam never fired")
	}
	if n := count(t, c, "record_runs"); n != 1 {
		t.Fatalf("%d runs before recovery, want only the first ingest's", n)
	}
	end := auditOf(t, c, evidence.ActionIngestEnd)[1]

	w2 := startWriter(t, c, records.Parser{Name: "x", Version: "1"}, records.WriterOptions{}, a.ID)
	rcs := auditOf(t, c, evidence.ActionIngestRecover)
	if len(rcs) != 1 {
		t.Fatalf("%d recover entries", len(rcs))
	}
	rc := details[evidence.IngestRecover](t, rcs[0])
	orig := details[evidence.IngestConclusion](t, end)
	if !rc.RunMissing || rc.Outcome != "complete" || rc.IngestID != w.IngestID() || rc.ByIngestID != w2.IngestID() ||
		rc.Records != 4 || rc.Rollup != orig.Rollup || len(rc.BatchNos) != 0 {
		t.Errorf("recover entry = %+v", rc)
	}
	var outcome, rollup string
	var endSeq int64
	if err := c.ReadTx(ctx, func(h evidence.ReadHandle) error {
		return h.QueryRow(`SELECT outcome, end_seq, rollup FROM record_runs WHERE ingest_id = ?`, w.IngestID()).Scan(&outcome, &endSeq, &rollup)
	}); err != nil {
		t.Fatal(err)
	}
	if outcome != "complete" || endSeq != end.Seq || rollup != orig.Rollup {
		t.Errorf("run = %s end_seq %d (want the end entry's %d) rollup %s", outcome, endSeq, end.Seq, rollup)
	}
	// the recovered complete run supersedes the older one
	if got, want := superseded(t, c), (map[pair]bool{{old.IngestID, a.ID}: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("superseded = %v, want %v", got, want)
	}
	if _, err := w2.End(ctx); err != nil {
		t.Fatal(err)
	}
	startWriter(t, c, records.Parser{Name: "y", Version: "1"}, records.WriterOptions{}, a.ID)
	if n := len(auditOf(t, c, evidence.ActionIngestRecover)); n != 1 {
		t.Errorf("%d recover entries, want 1", n)
	}
}

// TestLateCompleteRunStillSupersededByNewer: a run recovered late keeps its
// original end_seq, so it hides the older run and is hidden by the newer one.
func TestLateCompleteRunStillSupersededByNewer(t *testing.T) {
	c, a := setup(t)
	r1 := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	w := startWriter(t, c, records.Parser{Name: "p", Version: "2"}, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 2))
	crashAt(w, "after-end-audit", 1)
	if !survive(t, func() { _, _ = w.End(ctx) }) {
		t.Fatal("no crash")
	}
	r3 := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "3"}, []string{a.ID}, recordstest.Records(a.ID, 2, 3)) // recovers r2 first
	want := map[pair]bool{{r1.IngestID, a.ID}: true, {w.IngestID(), a.ID}: true}
	if got := superseded(t, c); !reflect.DeepEqual(got, want) {
		t.Errorf("superseded = %v, want %v (run %s is the newest and stays visible)", got, want, r3.IngestID)
	}
}

// TestRecoverEntrySurvivesFailingStart: Start recovers first; the recovery stays
// audited and recorded even when the Start that triggered it then fails.
func TestRecoverEntrySurvivesFailingStart(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	crashAt(w, "after-batch-audit", 1)
	if !survive(t, func() { add(t, w, recordstest.Records(a.ID, 2, 1)) }) {
		t.Fatal("no crash")
	}
	conflicting := records.Parser{Name: testParser.Name, Version: testParser.Version, Hash: "other-hash"}
	err := newWriter(t, c, conflicting, records.WriterOptions{}).Start(ctx, records.StartOptions{Artifacts: []string{a.ID}})
	if !errors.Is(err, records.ErrParserIdentityConflict) {
		t.Fatalf("Start = %v, want ErrParserIdentityConflict", err)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestRecover)); n != 1 {
		t.Errorf("%d recover entries, want the recovery to stay", n)
	}
	if n := len(auditOf(t, c, evidence.ActionIngestStart)); n != 1 {
		t.Errorf("%d start entries, want only the crashed ingest's", n)
	}
	if o := scalar[string](t, c, `SELECT outcome FROM record_runs WHERE ingest_id = ?`, w.IngestID()); o != "interrupted" {
		t.Errorf("recovered run outcome = %q", o)
	}
}

// TestRecoverFinishesHalfDoneRecovery: the process died after auditing a
// recover but before writing its run row; the next Start writes the row without
// auditing a second recover.
func TestRecoverFinishesHalfDoneRecovery(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 1)) // buffered, never flushed
	// a recover audited by hand, as a dead recovering process would have left it
	cn := evidence.IngestConclusion{IngestID: w.IngestID(), Outcome: "interrupted", Rollup: evidence.IngestRollup(nil)}
	rec := evidence.IngestRecover{IngestConclusion: cn, ByIngestID: "ing-dead", Reason: "test"}
	if _, err := c.Audit.Append(evidence.ActionIngestRecover, "", rec.Details()); err != nil {
		t.Fatal(err)
	}
	startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	if n := len(auditOf(t, c, evidence.ActionIngestRecover)); n != 1 {
		t.Errorf("%d recover entries, want the hand-written one only", n)
	}
	if o := scalar[string](t, c, `SELECT outcome FROM record_runs WHERE ingest_id = ?`, w.IngestID()); o != "interrupted" {
		t.Errorf("run outcome = %q", o)
	}
}
