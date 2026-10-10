package dbrecover

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
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
	slackPageBudget = 300
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
