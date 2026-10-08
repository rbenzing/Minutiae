package records_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func quoted(id string) string { return `"` + id + `"` }

// crashedIngest starts an ingest (batches of 2 rows) and simulates a dead
// process: it adds n records and the crash seam fires at the nth hit of point.
// Batches that were committed before the crash stay committed.
func crashedIngest(t *testing.T, c *evidence.Case, p records.Parser, a evidence.ManifestRecord, n int, point string, nth int) *records.Writer {
	t.Helper()
	w := startWriter(t, c, p, records.WriterOptions{BatchRows: 2}, a.ID)
	crashAt(w, point, nth)
	recs := recordstest.Records(a.ID, n, 1)
	if !survive(t, func() {
		for _, r := range recs {
			if err := w.Add(ctx, r); err != nil {
				t.Error(err)
			}
		}
		_ = w.Flush(ctx)
	}) {
		t.Fatalf("the crash seam %s/%d never fired", point, nth)
	}
	return w
}

func hasNotice(rep evidence.VerifyReport, parts ...string) bool {
	for _, n := range rep.Notices {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(n, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestVerifyFlagsInterruptedIngest(t *testing.T) {
	c, a := setup(t)
	// 3 records in batches of 2: batch 1 commits, batch 2 is audited and then the process dies
	w := crashedIngest(t, c, testParser, a, 3, "after-batch-audit", 2)
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"ingest " + quoted(w.IngestID()) + ": interrupted"})
	if rep.RecordsChecked != 2 {
		t.Errorf("RecordsChecked = %d, want the 2 committed records", rep.RecordsChecked)
	}
	if !hasNotice(rep, "batch 2 of ingest "+quoted(w.IngestID())) {
		t.Errorf("the announced batch that never reached the database is not reported: %q", rep.Notices)
	}
}

func TestVerifyFlagsAnnouncedAbsentBatch(t *testing.T) {
	c, a := setup(t)
	// the only batch is audited, then the process dies before its rows are written
	w := crashedIngest(t, c, testParser, a, 2, "before-insert", 1)
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"interrupted"})
	if rep.RecordsChecked != 0 {
		t.Errorf("RecordsChecked = %d, want 0", rep.RecordsChecked)
	}
	if !hasNotice(rep, "batch 1 of ingest "+quoted(w.IngestID()), "ids 1..2") {
		t.Errorf("the announced-absent batch is not reported: %q", rep.Notices)
	}
}

func TestIngestRecoverEntryClearsInterruption(t *testing.T) {
	c, a := setup(t)
	w := crashedIngest(t, c, testParser, a, 3, "after-batch-audit", 2)
	if rep := mustVerify(t, c); rep.OK() {
		t.Fatal("an interrupted ingest verified OK")
	}
	// the next Start writes the audited recover entry; the new ingest ends normally
	w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
	add(t, w2, recordstest.Records(a.ID, 1, 9))
	if _, err := w2.End(ctx); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !rep.OK() {
		t.Fatalf("a recovered ingest must verify: %q", rep.Problems)
	}
	if !hasNotice(rep, quoted(w.IngestID()), "recovered", "[2]") {
		t.Errorf("no notice names the recovered ingest and its lost batch: %q", rep.Notices)
	}
	// the interrupted ingest's committed records are still there and still verify
	if rep.RecordsChecked != 3 || rep.RecordRunsChecked != 2 {
		t.Errorf("RecordsChecked = %d, RecordRunsChecked = %d, want 3 (2 interrupted + 1) and 2", rep.RecordsChecked, rep.RecordRunsChecked)
	}
	if n := scalar[int](t, c, `SELECT count(*) FROM records WHERE batch_id IN (SELECT batch_id FROM record_batches WHERE ingest_id = ?)`, w.IngestID()); n != 2 {
		t.Errorf("%d records of the interrupted ingest, want 2", n)
	}
}

// TestRecoverOnlyViaAuditedEntry: the interruption is cleared by an audited
// records.ingest.recover entry and by nothing else.
func TestRecoverOnlyViaAuditedEntry(t *testing.T) {
	t.Run("a hand-inserted run row does not clear it", func(t *testing.T) {
		c, a := setup(t)
		w := crashedIngest(t, c, testParser, a, 2, "after-insert", 1)
		recordstest.InsertRun(t, c.Dir, 4242, w.IngestID(), testParser.Name, testParser.Version, "interrupted", 1, 2, 1, 2, strings.Repeat("0", 64))
		rep := mustVerify(t, c)
		expectProblems(t, rep, []string{"interrupted"}, "run row", "records.ingest.start")
	})
	t.Run("a hand-edited recover entry breaks the audit chain", func(t *testing.T) {
		c, a := setup(t)
		w := crashedIngest(t, c, testParser, a, 2, "after-insert", 1)
		w2 := startWriter(t, c, records.Parser{Name: "next", Version: "1"}, records.WriterOptions{}, a.ID)
		if _, err := w2.End(ctx); err != nil {
			t.Fatal(err)
		}
		if rep := mustVerify(t, c); !rep.OK() {
			t.Fatalf("not clean before the edit: %q", rep.Problems)
		}
		recs := auditOf(t, c, evidence.ActionIngestRecover)
		if len(recs) != 1 || recs[0].Details["ingest_id"] != w.IngestID() {
			t.Fatalf("recover entries = %v", recs)
		}
		recordstest.ReplaceInAuditLine(t, c.Dir, int(recs[0].Seq), `"reason":"the ingest never concluded`, `"reason":"edited by hand`)
		rep, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		if rep.OK() || !strings.Contains(strings.Join(rep.Problems, "\n"), "hash mismatch") {
			t.Errorf("the edited recover entry was not caught: %q", rep.Problems)
		}
	})
}

// TestVerifyIngestLifecycleRules (R3): per records.ingest.start, at most one
// end-or-error, at most one recover, at least one of the two; a recover of kind
// unfinished needs no end/error, a run-missing one needs one.
func TestVerifyIngestLifecycleRules(t *testing.T) {
	recoverOf := func(id string, runMissing bool) evidence.IngestRecover {
		outcome := "interrupted"
		if runMissing {
			outcome = "complete"
		}
		return evidence.IngestRecover{
			IngestConclusion: evidence.IngestConclusion{IngestID: id, Outcome: outcome, Rollup: evidence.IngestRollup(nil)},
			ByIngestID:       "ing-forged", Reason: "forged for the test", RunMissing: runMissing, BatchNos: []int{},
		}
	}
	appendRecover := func(t *testing.T, c *evidence.Case, rc evidence.IngestRecover) {
		t.Helper()
		if _, err := c.Audit.Append(evidence.ActionIngestRecover, "", rc.Details()); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a second end entry", func(t *testing.T) {
		g := ingest30(t)
		end := details[evidence.IngestConclusion](t, auditOf(t, g.c, evidence.ActionIngestEnd)[0])
		if _, err := g.c.Audit.Append(evidence.ActionIngestEnd, "", end.Details()); err != nil {
			t.Fatal(err)
		}
		expectProblems(t, mustVerify(t, g.c), []string{"concluded more than once"})
	})
	t.Run("an unfinished-kind recover of an ingest that concluded", func(t *testing.T) {
		g := ingest30(t)
		appendRecover(t, g.c, recoverOf(g.res.IngestID, false))
		expectProblems(t, mustVerify(t, g.c), []string{"never concluded"})
	})
	t.Run("a run-missing recover without an end entry", func(t *testing.T) {
		c, a := setup(t)
		w := crashedIngest(t, c, testParser, a, 2, "after-insert", 1)
		appendRecover(t, c, recoverOf(w.IngestID(), true))
		expectProblems(t, mustVerify(t, c), []string{"holds no records.ingest.end"}, "run row", "differs from the audit log")
	})
	t.Run("two recovers", func(t *testing.T) {
		c, a := setup(t)
		w := crashedIngest(t, c, testParser, a, 2, "after-insert", 1)
		appendRecover(t, c, recoverOf(w.IngestID(), false))
		appendRecover(t, c, recoverOf(w.IngestID(), false))
		expectProblems(t, mustVerify(t, c), []string{"recovered more than once"}, "run row", "differs from the audit log", "does not match its audited batches")
	})
	t.Run("an entry for an ingest that never started", func(t *testing.T) {
		g := ingest30(t)
		appendRecover(t, g.c, recoverOf("ing-ghost", false))
		expectProblems(t, mustVerify(t, g.c), []string{`"ing-ghost"`, "no records.ingest.start"})
	})
}

func TestVerifyIngestEndTotalsMustMatchBatches(t *testing.T) {
	t.Run("run row records", func(t *testing.T) {
		g := ingest30(t)
		recordstest.SetRunColumn(t, g.c.Dir, g.res.IngestID, "records", 31)
		expectProblems(t, mustVerify(t, g.c), []string{": records is 31, audited 30"})
	})
	t.Run("run row rollup", func(t *testing.T) {
		g := ingest30(t)
		recordstest.SetRunColumn(t, g.c.Dir, g.res.IngestID, "rollup", strings.Repeat("0", 64))
		expectProblems(t, mustVerify(t, g.c), []string{": rollup is"})
	})
	t.Run("an end entry that disagrees with its audited batches", func(t *testing.T) {
		c, a := setup(t)
		w := crashedIngest(t, c, testParser, a, 2, "after-insert", 1) // one committed batch of 2, never concluded
		forged := evidence.IngestConclusion{
			IngestID: w.IngestID(), Outcome: "complete", Batches: 1, Records: 99, FirstID: 1, LastID: 2,
			Rollup: strings.Repeat("0", 64), Types: map[string]int64{},
		}
		if _, err := c.Audit.Append(evidence.ActionIngestEnd, "", forged.Details()); err != nil {
			t.Fatal(err)
		}
		rep := mustVerify(t, c)
		expectProblems(t, rep, []string{"records is 99, the batches hold 2", "rollup is", "run row missing"})
	})
	t.Run("zero batches", func(t *testing.T) {
		c, a := setup(t)
		recordstest.Ingest(t, c, testParser, []string{a.ID}, nil)
		if rep := mustVerify(t, c); !rep.OK() || rep.RecordRunsChecked != 1 {
			t.Fatalf("an ingest of no records must verify: %+v", rep)
		}
	})
}

func TestVerifyDetectsErasedRunRow(t *testing.T) {
	g := ingest30(t)
	recordstest.DeleteRun(t, g.c.Dir, g.res.IngestID)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"run row missing"})
	if !strings.Contains(strings.Join(rep.Problems, "\n"), quoted(g.res.IngestID)) {
		t.Errorf("the problem does not name the ingest: %q", rep.Problems)
	}
}

func TestVerifyRunRowMustMatchAudit(t *testing.T) {
	build := func(t *testing.T) (*evidence.Case, string, int64) {
		t.Helper()
		c, a := setup(t)
		w := newWriter(t, c, testParser, records.WriterOptions{BatchRows: 10})
		if err := w.Start(ctx, records.StartOptions{AnalysisID: "an-1", Artifacts: []string{a.ID}}); err != nil {
			t.Fatal(err)
		}
		add(t, w, recordstest.Records(a.ID, 30, 7))
		res, err := w.End(ctx)
		if err != nil {
			t.Fatal(err)
		}
		other := records.Parser{Name: "other-parser", Version: "2.0", Hash: "def456"}
		recordstest.Ingest(t, c, other, []string{a.ID}, recordstest.Records(a.ID, 1, 3))
		p2 := scalar[int64](t, c, `SELECT id FROM parsers WHERE name = 'other-parser'`)
		return c, res.IngestID, p2
	}
	cases := []struct {
		column string
		value  func(p2 int64) any
		want   string
	}{
		{"end_seq", func(int64) any { return 1 }, ": end_seq is 1,"},
		{"parser_id", func(p2 int64) any { return p2 }, ": parser is "},
		{"analysis_id", func(int64) any { return "forged" }, `: analysis_id is "forged",`},
		{"analysis_id to NULL", func(int64) any { return nil }, `: analysis_id is "",`},
		{"outcome", func(int64) any { return "incomplete" }, `: outcome is "incomplete",`},
		{"batches", func(int64) any { return 4 }, ": batches is 4, audited 3"},
		{"records", func(int64) any { return 29 }, ": records is 29, audited 30"},
		{"first_id", func(int64) any { return 2 }, ": first_id is 2, audited 1"},
		{"last_id", func(int64) any { return 29 }, ": last_id is 29, audited 30"},
		{"rollup", func(int64) any { return strings.Repeat("0", 64) }, ": rollup is"},
		{"ended", func(int64) any { return "2001-01-01T00:00:00Z" }, ": ended is"},
	}
	for _, tc := range cases {
		t.Run(tc.column, func(t *testing.T) {
			c, ing, p2 := build(t)
			if rep := mustVerify(t, c); !rep.OK() {
				t.Fatalf("not clean before the change: %q", rep.Problems)
			}
			col := strings.TrimSuffix(tc.column, " to NULL")
			recordstest.SetRunColumn(t, c.Dir, ing, col, tc.value(p2))
			rep := mustVerify(t, c)
			// a swapped parser also changes which runs share a parser name, hence the supersession
			expectProblems(t, rep, []string{"run row of ingest " + quoted(ing) + " differs from the audit log", tc.want}, "record_superseded is missing the pair")
		})
	}
}

func TestVerifyRunArtifactsMustMatchStart(t *testing.T) {
	setup2 := func(t *testing.T) (*evidence.Case, evidence.ManifestRecord, evidence.ManifestRecord, string) {
		t.Helper()
		c, a := setup(t)
		b := recordstest.AddArtifact(t, c, "b.db", append([]byte("second"), mib...))
		res := recordstest.Ingest(t, c, testParser, []string{a.ID, b.ID}, recordstest.Records(a.ID, 4, 1))
		return c, a, b, res.IngestID
	}
	t.Run("a coverage row is missing", func(t *testing.T) {
		c, _, b, ing := setup2(t)
		recordstest.DeleteRunArtifact(t, c.Dir, ing, b.ID)
		expectProblems(t, mustVerify(t, c), []string{"run coverage of ingest " + quoted(ing), quoted(b.ID) + " is in the records.ingest.start entry but not in record_run_artifacts"})
	})
	t.Run("a coverage row is extra", func(t *testing.T) {
		c, a := setup(t)
		b := recordstest.AddArtifact(t, c, "b.db", append([]byte("second"), mib...))
		res := recordstest.Ingest(t, c, testParser, []string{a.ID}, recordstest.Records(a.ID, 4, 1))
		recordstest.InjectRunArtifact(t, c.Dir, res.IngestID, b.ID)
		expectProblems(t, mustVerify(t, c), []string{"run coverage of ingest " + quoted(res.IngestID), quoted(b.ID) + " is in record_run_artifacts but not in the records.ingest.start entry"})
	})
	t.Run("a coverage row of no run", func(t *testing.T) {
		c, a, _, _ := setup2(t)
		recordstest.InjectRunArtifact(t, c.Dir, "ing-ghost", a.ID)
		expectProblems(t, mustVerify(t, c), []string{`ingest "ing-ghost"`, "belongs to no run row"})
	})
}

func TestVerifyDetectsSupersedeTamper(t *testing.T) {
	two := func(t *testing.T, second func(c *evidence.Case, a evidence.ManifestRecord) string) (*evidence.Case, evidence.ManifestRecord, string, string) {
		t.Helper()
		c, a := setup(t)
		r1 := recordstest.Ingest(t, c, records.Parser{Name: "sms", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 3, 1))
		return c, a, r1.IngestID, second(c, a)
	}
	complete := func(c *evidence.Case, a evidence.ManifestRecord) string {
		return recordstest.Ingest(t, c, records.Parser{Name: "sms", Version: "2"}, []string{a.ID}, recordstest.Records(a.ID, 3, 2)).IngestID
	}
	aborted := func(c *evidence.Case, a evidence.ManifestRecord) string {
		w := startWriter(t, c, records.Parser{Name: "sms", Version: "2"}, records.WriterOptions{}, a.ID)
		add(t, w, recordstest.Records(a.ID, 3, 2))
		if _, err := w.Abort(ctx, io.ErrUnexpectedEOF); err != nil {
			t.Fatal(err)
		}
		return w.IngestID()
	}
	t.Run("deleting a pair resurrects old records", func(t *testing.T) {
		c, a, r1, _ := two(t, complete)
		if rep := mustVerify(t, c); !rep.OK() {
			t.Fatalf("not clean before the tamper: %q", rep.Problems)
		}
		recordstest.DeleteSuperseded(t, c.Dir, r1, a.ID)
		rep := mustVerify(t, c)
		expectProblems(t, rep, []string{"record_superseded is missing the pair", quoted(r1)})
	})
	t.Run("injecting a pair hides records", func(t *testing.T) {
		c, a, _, r2 := two(t, complete)
		recordstest.InjectSuperseded(t, c.Dir, r2, a.ID)
		expectProblems(t, mustVerify(t, c), []string{"record_superseded holds a pair no run implies", quoted(r2)})
	})
	t.Run("only a complete run supersedes (R11)", func(t *testing.T) {
		c, a, r1, _ := two(t, aborted)
		if rep := mustVerify(t, c); !rep.OK() {
			t.Fatalf("an aborted newer run must leave the older one visible: %q", rep.Problems)
		}
		recordstest.InjectSuperseded(t, c.Dir, r1, a.ID)
		expectProblems(t, mustVerify(t, c), []string{"record_superseded holds a pair no run implies", quoted(r1)})
	})
}

// TestVerifyRecoveredCompleteRunSupersedes: an ingest that concluded in the audit
// log but whose run row was written late (by a recovery) keeps its end_seq and
// supersedes exactly as a timely run would.
func TestVerifyRecoveredCompleteRunSupersedes(t *testing.T) {
	c, a := setup(t)
	r1 := recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	w := startWriter(t, c, records.Parser{Name: "p", Version: "2"}, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 2))
	crashAt(w, "after-end-audit", 1)
	if !survive(t, func() { _, _ = w.End(ctx) }) {
		t.Fatal("no crash")
	}
	// the run row is missing until the next Start recovers it
	expectProblems(t, mustVerify(t, c), []string{"run row missing"}, "superseded")
	recordstest.Ingest(t, c, records.Parser{Name: "q", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 1, 3))
	rep := mustVerify(t, c)
	if !rep.OK() {
		t.Fatalf("a recovered run must verify: %q", rep.Problems)
	}
	if !hasNotice(rep, quoted(w.IngestID()), "recovered", "run row") {
		t.Errorf("no notice names the recovered ingest: %q", rep.Notices)
	}
	if n := scalar[int](t, c, `SELECT count(*) FROM record_superseded WHERE ingest_id = ?`, r1.IngestID); n != 1 {
		t.Errorf("the older run is superseded %d times, want 1", n)
	}
}

func TestVerifyParserRowNeedsAuditedStart(t *testing.T) {
	g := ingest30(t)
	recordstest.InjectParser(t, g.c.Dir, "ghost-parser", "9", "deadbeef")
	expectProblems(t, mustVerify(t, g.c), []string{`parser "ghost-parser" "9"`, "not named by any records.ingest.start"})
}

func TestVerifyDetectsOrphanRecordTimes(t *testing.T) {
	c, a := setup(t)
	recordstest.Ingest(t, c, testParser, []string{a.ID}, []records.Record{
		fullRecord(a.ID, 1, records.BasisLocalOffset, 60, true),
		fullRecord(a.ID, 2, records.BasisLocalUnknown, 0, false),
	})
	recordstest.DeleteRecordKeepTimes(t, c.Dir, 1)
	expectProblems(t, mustVerify(t, c), []string{"record_times row", "of record 1 has no record"}, "records stored", "digest mismatch", "records_fts")
}

func TestVerifyArtifactHashInBatchAuditMustMatchManifest(t *testing.T) {
	g := ingest30(t)
	forged := append([]byte("forged"), mib[:len(mib)-6]...)
	recordstest.RewriteArtifactConsistently(t, g.c.Dir, g.art.ID, forged)
	rep := mustVerify(t, g.c)
	expectProblems(t, rep, []string{"differs from the manifest", "digest mismatch", "does not match its artifact.create audit entry"})
	joined := strings.Join(rep.Problems, "\n")
	if !strings.Contains(joined, "batch 1 of ingest "+quoted(g.res.IngestID)) || !strings.Contains(joined, quoted(g.art.ID)) {
		t.Errorf("the problem does not name the batch and the artifact: %q", rep.Problems)
	}
	if n := countProblems(rep, "differs from the manifest"); n != 3 {
		t.Errorf("%d P7 problems, want one per audited batch (3)", n)
	}
}

func countProblems(rep evidence.VerifyReport, sub string) int {
	n := 0
	for _, p := range rep.Problems {
		if strings.Contains(p, sub) {
			n++
		}
	}
	return n
}

func TestVerifyFlagsRecordRangeBeyondArtifact(t *testing.T) {
	size := int64(len(mib))
	cases := []struct {
		name    string
		off, ln int64
		beyond  bool
	}{
		{"offset past the end", size + 1, 5, true},
		{"length past the end", size - 4, 5, true},
		{"offset near the integer limit", math.MaxInt64 - 2, 1, true},
		{"length near the integer limit", 10, math.MaxInt64, true},
		{"ends exactly at the end", size - 5, 5, false},
		{"empty range at the end", size, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := ingest30(t)
			recordstest.SetRecordColumns(t, g.c.Dir, 7, map[string]any{"src_offset": tc.off, "src_length": tc.ln})
			rep := mustVerify(t, g.c)
			if tc.beyond {
				expectProblems(t, rep, []string{"range beyond artifact", "digest mismatch"})
				if !strings.Contains(strings.Join(rep.Problems, "\n"), "record 7") {
					t.Errorf("the problem does not name the record: %q", rep.Problems)
				}
				return
			}
			expectProblems(t, rep, []string{"digest mismatch"})
		})
	}
}

func TestVerifyReportsRunCounts(t *testing.T) {
	c, a := setup(t)
	if rep := mustVerify(t, c); rep.RecordRunsChecked != 0 {
		t.Fatalf("RecordRunsChecked = %d on a case with no ingest", rep.RecordRunsChecked)
	}
	recordstest.Ingest(t, c, records.Parser{Name: "p", Version: "1"}, []string{a.ID}, recordstest.Records(a.ID, 2, 1))
	w := startWriter(t, c, records.Parser{Name: "q", Version: "1"}, records.WriterOptions{}, a.ID)
	add(t, w, recordstest.Records(a.ID, 2, 2))
	if _, err := w.Abort(ctx, io.ErrUnexpectedEOF); err != nil {
		t.Fatal(err)
	}
	rep := mustVerify(t, c)
	if !rep.OK() || rep.RecordRunsChecked != 2 || rep.RecordsChecked != 2 {
		t.Fatalf("report = %+v", rep)
	}
	b, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(b), `"record_runs_checked":2`) {
		t.Errorf("json = %s, %v", b, err)
	}
	runs := auditOf(t, c, "verify.run")
	if last := runs[len(runs)-1]; last.Details["record_runs_checked"] != json.Number("2") {
		t.Errorf("verify.run = %v", last.Details)
	}
}

// TestVerifyCountsCommittedBatchesOnly: an audited batch explained by a
// records.batch.error is not part of the run's totals, and the run verifies.
func TestVerifyCountsCommittedBatchesOnly(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	recs := recordstest.Records(a.ID, 4, 1)
	add(t, w, recs[:2]) // batch 1 commits
	w.SetHook(func(p string) error {
		if p == "before-insert" {
			return fmt.Errorf("disk full")
		}
		return nil
	})
	if err := func() error {
		for _, r := range recs[2:] {
			if err := w.Add(ctx, r); err != nil {
				return err
			}
		}
		return nil
	}(); err == nil {
		t.Fatal("the failing batch did not fail")
	}
	res, err := w.Abort(ctx, io.ErrUnexpectedEOF) // concludes with the committed batch only
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 1 || res.Records != 2 {
		t.Fatalf("result = %+v", res)
	}
	rep := mustVerify(t, c)
	if !rep.OK() {
		t.Fatalf("an aborted ingest with a failed batch must verify: %q", rep.Problems)
	}
	if !hasNotice(rep, "batch 2 of ingest "+quoted(w.IngestID())) {
		t.Errorf("the failed batch is not reported as a notice: %q", rep.Notices)
	}
}

func TestVerifyFlagsRecordWithBrokenDerivedChain(t *testing.T) {
	c := recordstest.NewCase(t)
	var chain []evidence.ManifestRecord
	for i := 0; i <= 17; i++ {
		src := evidence.Source{Kind: "file", DeviceID: "dev1"}
		if i > 0 {
			src.Derived = &evidence.Derivation{ParentID: chain[i-1].ID, ParentSHA256: chain[i-1].SHA256}
		}
		data := []byte(fmt.Sprintf("artifact %d", i))
		if i == 17 {
			data = append(data, mib...)
		}
		rec, err := c.Capture("dev1", "acq1", fmt.Sprintf("chain%d", i), src, func(w io.Writer) error {
			_, err := w.Write(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, rec)
	}
	recordstest.Ingest(t, c, testParser, []string{chain[17].ID}, recordstest.Records(chain[17].ID, 3, 1))
	rep := mustVerify(t, c)
	expectProblems(t, rep, []string{"derived chain"})
	if !strings.Contains(strings.Join(rep.Problems, "\n"), chain[17].ID) {
		t.Errorf("the problem does not name the deepest artifact: %q", rep.Problems)
	}
	if rep.RecordsChecked != 3 {
		t.Errorf("RecordsChecked = %d: verify must go on after the chain problem", rep.RecordsChecked)
	}
}

// TestVerifyLiveIngestIsNotInterrupted: an ingest running in this process has no
// conclusion yet; verify reports it as a notice, not as an interruption.
func TestVerifyLiveIngestIsNotInterrupted(t *testing.T) {
	c, a := setup(t)
	w := startWriter(t, c, testParser, records.WriterOptions{BatchRows: 2}, a.ID)
	add(t, w, recordstest.Records(a.ID, 3, 1))
	rep := mustVerify(t, c)
	if !rep.OK() || !hasNotice(rep, quoted(w.IngestID()), "still running") {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if rep := mustVerify(t, c); !rep.OK() || len(rep.Notices) != 0 {
		t.Fatalf("after End: %+v", rep)
	}
}
