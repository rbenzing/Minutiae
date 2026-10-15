package dbrecover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// visibleOf counts the records of one ingest that the default list shows
// (stored, not superseded): rows a narrowed run wrote are kept and listable.
func visibleOf(t *testing.T, c *evidence.Case, ingest string) (n int64) {
	t.Helper()
	for _, row := range visibleRecords(t, c) {
		if row.IngestID == ingest {
			n++
		}
	}
	return n
}

func requireKeptIncomplete(t *testing.T, c *evidence.Case, res Result, reason string) {
	t.Helper()
	if res.Ingest.Outcome != "incomplete" || !slices.Contains(res.Counts.Narrowed, reason) {
		t.Fatalf("outcome %q narrowed %v, want incomplete and %q", res.Ingest.Outcome, res.Counts.Narrowed, reason)
	}
	if res.Counts.Written == 0 {
		t.Fatalf("the run wrote no rows: the check proves nothing")
	}
	if got := visibleOf(t, c, res.Ingest.IngestID); got != res.Counts.Written {
		t.Errorf("%d of the %d written rows are listable after the run: a narrowed run keeps its rows", got, res.Counts.Written)
	}
	es := readAudit(t, c)
	if count(es, evidence.ActionIngestEnd) != 0 || count(es, evidence.ActionIngestError) == 0 {
		t.Errorf("ingest.end %d ingest.error %d: a narrowed run is aborted, never ended", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError))
	}
	nl, _ := lastEntry(t, es, "analysis.end").Details["narrowed"].([]any)
	if !slices.Contains(nl, any(reason)) {
		t.Errorf("analysis.end narrowed %v, want %q", nl, reason)
	}
}

// TestWriterRejectedRunIsNarrowedAndKeepsItsRows (J112): rows the writer
// refuses are a loss, so the run concludes incomplete; the rows it did write
// are kept and listable.
func TestWriterRejectedRunIsNarrowedAndKeepsItsRows(t *testing.T) {
	e := newRunEnv(t)
	n := 0
	defer SetCheckForTest(func(p map[string]any) error {
		if n++; n <= 2 {
			return fmt.Errorf("%w: db_row: forced", records.ErrInvalidPayload)
		}
		return checkPayload(p)
	})()
	res, err := Run(t.Context(), e.c, e.in(), Options{})
	if err != nil {
		t.Fatalf("a narrowed run is not an error: %v", err)
	}
	if res.Counts.SkippedByReason["writer-rejected"] != 2 {
		t.Errorf("skipped %v", res.Counts.SkippedByReason)
	}
	requireKeptIncomplete(t, e.c, res, "writer-rejected")
}

// TestRejectedRowsRunNeverSupersedesACompleteRun (J112, partial work never
// supersedes complete work).
func TestRejectedRowsRunNeverSupersedesACompleteRun(t *testing.T) {
	e := newRunEnv(t)
	full := mustRun(t, e.c, e.in(), Options{})
	if full.Ingest.Outcome != "complete" {
		t.Fatalf("full run %+v", full.Ingest)
	}
	n := 0
	defer SetCheckForTest(func(p map[string]any) error {
		if n++; n <= 3 {
			return fmt.Errorf("%w: db_row: forced", records.ErrInvalidPayload)
		}
		return checkPayload(p)
	})()
	lossy, err := Run(t.Context(), e.c, e.in(), Options{AllowReingest: true})
	if err != nil {
		t.Fatal(err)
	}
	if lossy.Ingest.Outcome != "incomplete" {
		t.Fatalf("lossy run %q, want incomplete", lossy.Ingest.Outcome)
	}
	if got := visibleOf(t, e.c, full.Ingest.IngestID); got != full.Counts.Written {
		t.Errorf("the complete run shows %d of its %d rows: a run that lost rows superseded it", got, full.Counts.Written)
	}
	for _, f := range storedRecords(t, e.c) {
		if f.IngestID == full.Ingest.IngestID && f.Superseded {
			t.Errorf("record %d of the complete run was superseded", f.ID)
		}
	}
}

func slackTwoCellScene(t *testing.T) *slackScene {
	t.Helper()
	s := plainScene()
	off, _ := s.gap(t, 2)
	s.plant(2, off+10, slackCell(7, 70, "first-slack-"+filler(24, 7)))
	s.plant(2, off+120, slackCell(8, 80, "second-slack-"+filler(24, 8)))
	if rows, _ := s.carve(t); len(rows) != 2 {
		t.Fatalf("scene carves %d rows, want 2", len(rows))
	}
	return s
}

// TestSlackRowCapRunIsNarrowedAndKeepsItsRows (J112): the slack MaxHistoryRows
// stop leaves rows unlisted, so the run is incomplete.
func TestSlackRowCapRunIsNarrowedAndKeepsItsRows(t *testing.T) {
	s := slackTwoCellScene(t)
	defer func(old int64) { slackMaxRows = old }(slackMaxRows)
	slackMaxRows = 1
	c := recordstest.NewCase(t)
	db := addFile(t, c, "/data/slack.db", s.b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	if res.Counts.ByMethod["sqlite-page-slack"] != 1 {
		t.Errorf("slack rows %d, want 1 (the cap)", res.Counts.ByMethod["sqlite-page-slack"])
	}
	requireKeptIncomplete(t, c, res, "slack-row-cap")
}

// TestSlackWorkCapRunIsNarrowedAndKeepsItsRows (J112): a page whose free space
// was not fully examined makes the run incomplete.
func TestSlackWorkCapRunIsNarrowedAndKeepsItsRows(t *testing.T) {
	s := slackTwoCellScene(t)
	off, n := s.gap(t, 2)
	s.plant(2, off+300, bytesRepeat(0x7f, (n-300)/2))
	defer func(old int64) { slackPageBudget = old }(slackPageBudget)
	slackPageBudget = 3000
	c := recordstest.NewCase(t)
	db := addFile(t, c, "/data/slack.db", s.b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	requireKeptIncomplete(t, c, res, "slack-work-cap")
}

// TestClippedPayloadRunIsNotNarrowed (J112): clipping flags each row and the
// row set stays complete, so the run concludes complete.
func TestClippedPayloadRunIsNotNarrowed(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{MaxBodyBytes: 64})
	if res.Ingest.Outcome != "complete" || len(res.Counts.Narrowed) != 0 || len(res.Counts.LimitsHit) != 0 {
		t.Errorf("outcome %q narrowed %v limits %v", res.Ingest.Outcome, res.Counts.Narrowed, res.Counts.LimitsHit)
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, max(n, 0))
	for i := range out {
		out[i] = b
	}
	return out
}

// fitWRDB: a live WITHOUT ROWID table with five rows, and a dropped twin whose
// three rows are named by the fit alone and equal live rows.
func fitWRDB() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	live := b.CreateTableWithoutRowid("w", "create table w(k primary key, v) without rowid", 1)
	for i := 0; i < 5; i++ {
		live.Insert(int64(i), string(rune('a'+i)), "live")
	}
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTableWithoutRowid("gone", "create table gone(k primary key, v) without rowid", 1)
	for i := 0; i < 3; i++ {
		gone.Insert(int64(i), string(rune('a'+i)), "live")
	}
	b.DropTable("pad")
	b.DropTable("gone")
	return b.Bytes()
}

// TestFitEqualityBudgetSpentDoesNotNarrowTheRun (J110): spending the fit-only
// equality budget annotates unknown; every row is kept, slack is carved, the
// run concludes complete, and one counted warning says what was left.
func TestFitEqualityBudgetSpentDoesNotNarrowTheRun(t *testing.T) {
	c := recordstest.NewCase(t)
	db := addFile(t, c, "/data/fit.db", fitWRDB())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{MaxFitEqualityRows: 2})
	if res.Ingest.Outcome != "complete" || len(res.Counts.Narrowed) != 0 || len(res.Counts.LimitsHit) != 0 {
		t.Fatalf("outcome %q narrowed %v limits %v: an annotation budget must not narrow", res.Ingest.Outcome, res.Counts.Narrowed, res.Counts.LimitsHit)
	}
	if res.Counts.FitOnly < 3 || res.Counts.FitEqualityCapped != 3 || res.Counts.Written < 3 {
		t.Errorf("FitOnly %d FitEqualityCapped %d Written %d, want 3 or more, 3, 3 or more", res.Counts.FitOnly, res.Counts.FitEqualityCapped, res.Counts.Written)
	}
	unknown := 0
	for _, f := range storedRecords(t, c) {
		p := payloadOf(t, f)
		if notes, _ := p["notes"].([]any); slices.Contains(notes, any("fit-equality-budget")) {
			unknown++
		}
		if notes, _ := p["notes"].([]any); slices.Contains(notes, any("equals-live-row")) {
			t.Errorf("record %d is annotated equal to live after the budget was spent: %v", f.ID, notes)
		}
	}
	if unknown != 3 {
		t.Errorf("%d records carry fit-equality-budget, want 3", unknown)
	}
	es := readAudit(t, c)
	warned := 0
	for _, i := range indexes(es, "analysis.warning") {
		if r, _ := es[i].Details["reason"].(string); strings.Contains(r, "fit-only equality was cut") && strings.Contains(r, "3 fit-named rows") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d audited fit-equality warnings, want exactly 1", warned)
	}
	if v := lastEntry(t, es, "analysis.end").Details["fit_equality_capped"]; fmt.Sprint(v) != "3" {
		t.Errorf("analysis.end fit_equality_capped %v, want 3", v)
	}
}

// TestSlackPanicIsAPanicError (J114): a panic while carving free space is a
// *PanicError (errors.Is ErrInternal), like every other recovered panic.
func TestSlackPanicIsAPanicError(t *testing.T) {
	s := slackTwoCellScene(t)
	db := s.open(t)
	h := db.History()
	defer h.Release()
	_, err := carveSpans(t.Context(), db, h, func(sqlitefile.RecoveredRow) bool { panic("boom") })
	var pe *PanicError
	if !errors.Is(err, ErrInternal) || !errors.As(err, &pe) || pe.Value != "boom" {
		t.Errorf("err %v, want a *PanicError that is ErrInternal", err)
	}
}

// TestSlackCrossPageEqualAndOverlapAreSurfaced (J115): the counted slack drops
// and annotations are in Counts and in analysis.end, not only in the scanner.
func TestSlackCrossPageEqualAndOverlapAreSurfaced(t *testing.T) {
	s := newSlackScene(func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a integer, b text)")
		tb.Insert(1, 10, "live-row-one-"+filler(30, 1))
		u := b.CreateTable("u", "create table u(p integer)")
		u.Insert(1, 5)
	})
	off, _ := s.gap(t, 3)
	s.plant(3, off+20, slackCell(1, 10, "live-row-one-"+filler(30, 1)))
	c := recordstest.NewCase(t)
	db := addFile(t, c, "/data/slack.db", s.b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	if res.Counts.SlackCrossPageEqual != 1 {
		t.Errorf("SlackCrossPageEqual %d, want 1", res.Counts.SlackCrossPageEqual)
	}
	end := lastEntry(t, readAudit(t, c), "analysis.end").Details
	for key, want := range map[string]int64{"slack_cross_page_equal": res.Counts.SlackCrossPageEqual, "slack_dropped_overlap": res.Counts.SlackDroppedOverlap} {
		if got := fmt.Sprint(end[key]); got != fmt.Sprint(want) {
			t.Errorf("analysis.end %s = %s, want %d", key, got, want)
		}
	}
}

// TestSlackCrossedCheckIsChargedBeforeItRuns (J121): the clone and decode of a
// cell, and the walk of the crossed check, are charged to the page budget before
// the work is done; a candidate whose check cannot be paid for is not delivered
// as clean, the page is capped and the cap is counted.
func TestSlackCrossedCheckIsChargedBeforeItRuns(t *testing.T) {
	s := plainScene()
	off, _ := s.gap(t, 2)
	s.plant(2, off+10, slackCell(7, 70, "charged-"+filler(60, 7)))
	if rows, st := s.carve(t); len(rows) != 1 || st.WorkCapped != 0 {
		t.Fatalf("with the default budget: %d rows, %+v", len(rows), st)
	}
	defer func(old int64) { slackPageBudget = old }(slackPageBudget)
	slackPageBudget = 50 // the zero run before the cell, then less than the cell's own cost
	rows, st := s.carve(t)
	if len(rows) != 0 || st.WorkCapped != 1 {
		t.Errorf("%d rows, WorkCapped %d: a cell whose check exceeds the budget must be capped, not delivered", len(rows), st.WorkCapped)
	}
}

// TestEndFailureConcludesTheWriter (J118): when End fails, the writer is
// concluded (Abort), so the case's live-ingest slot is not held until exit.
func TestEndFailureConcludesTheWriter(t *testing.T) {
	e := newRunEnv(t)
	defer SetAfterFlushHookForTest(func() {
		if err := e.c.Audit.Close(); err != nil { // the audit append of End now fails
			t.Error(err)
		}
	})()
	_, err := Run(t.Context(), e.c, e.in(), Options{})
	if err == nil {
		t.Fatal("Run succeeded although the audit log was closed before End")
	}
	active, free := e.c.BeginIngest("probe-after-end-failure")
	if !free {
		t.Fatalf("the live-ingest slot is still held by %s after End failed: the writer was not concluded", active)
	}
	e.c.EndIngest("probe-after-end-failure")
}

// TestIntegrityFailureHasItsOwnAuditReason (J120): any integrity failure is
// audited under the reason "integrity" (an input that changed keeps
// "input-changed"), never the generic "error".
func TestIntegrityFailureHasItsOwnAuditReason(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"manifest snapshot mismatch": {fmt.Errorf("%w: db input a differs from the manifest snapshot", evidence.ErrIntegrity), "integrity"},
		"joined with a cancel":       {errors.Join(context.Canceled, evidence.ErrIntegrity), "integrity"},
		"input changed":              {&InputChangedError{ArtifactID: "a", Want: "x", Got: "y"}, "input-changed"},
		"plain":                      {errors.New("boom"), "error"},
		"refusal":                    {&RefusalError{Reason: ReasonBadOption}, ReasonBadOption},
	} {
		if got := reasonToken(tc.err); got != tc.want {
			t.Errorf("%s: reason %q, want %q", name, got, tc.want)
		}
	}
}

// TestPlanSeesWhatRunWouldReject (J117): Plan judges each mapped record with
// the writer's validator (writing nothing), so its counts and narrowing match
// those of a Run under the same refusals.
func TestPlanSeesWhatRunWouldReject(t *testing.T) {
	e := newRunEnv(t)
	reject := func() func() {
		n := 0
		return SetCheckForTest(func(p map[string]any) error {
			if n++; n <= 2 {
				return fmt.Errorf("%w: db_row: forced", records.ErrInvalidPayload)
			}
			return checkPayload(p)
		})
	}
	restore := reject()
	plan, err := Plan(t.Context(), e.c, e.in(), Options{})
	restore()
	if err != nil {
		t.Fatal(err)
	}
	restore = reject()
	run := mustRun(t, e.c, e.in(), Options{})
	restore()
	if plan.Counts.SkippedByReason["writer-rejected"] != 2 || plan.Counts.Written != run.Counts.Written {
		t.Errorf("plan skipped %v written %d; run skipped %v written %d", plan.Counts.SkippedByReason, plan.Counts.Written, run.Counts.SkippedByReason, run.Counts.Written)
	}
	if !slices.Contains(plan.Counts.Narrowed, "writer-rejected") {
		t.Errorf("plan narrowed %v, want writer-rejected", plan.Counts.Narrowed)
	}
}

// TestPlanReportsAnInputThatChanged (J117): like Run, Plan hashes its inputs
// afresh at the end; a changed input is an integrity error (exit 4).
func TestPlanReportsAnInputThatChanged(t *testing.T) {
	e := newRunEnv(t)
	defer changeWAL(t, e)()
	_, err := Plan(t.Context(), e.c, e.in(), Options{})
	var ch *InputChangedError
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.As(err, &ch) || ch.ArtifactID != e.wal.ID {
		t.Errorf("err %v, want an *InputChangedError for the WAL", err)
	}
}
