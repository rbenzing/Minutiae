package rowrun_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// echoMapping emits one record per row of t whose Body is the value of column a.
func echoMapping(extra func(m parse.MapContext, row parse.Row) error) parse.TableMapping {
	return parse.TableMapping{
		Table:   "t",
		Columns: []parse.ColumnSpec{{Name: "a", Required: true}, {Name: "s", Required: true}},
		Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			place, err := rowrun.Where(m, row)
			if err != nil {
				return nil, err
			}
			if extra != nil {
				if err := extra(m, row); err != nil {
					return nil, err
				}
			}
			a, _ := row.Int(m.Column("t", "a"))
			return []records.Record{{
				Type: "message", ArtifactID: place.ArtifactID, Locator: place.Locator,
				Range: &place.Range, Body: strconv.FormatInt(a, 10),
			}}, nil
		},
	}
}

func runEcho(t *testing.T, n int, mp parse.TableMapping) (*recEmitter, rowrun.Report, error) {
	t.Helper()
	em := &recEmitter{}
	rep, err := rowrun.Run(t.Context(), dbInput(t, tinyDB(t, n)), em, []rowrun.Step{{Mapping: mp}})
	return em, rep, err
}

func TestRunEmitsRecordsInRowidOrderWithPlaces(t *testing.T) {
	em, rep, err := runEcho(t, 5, echoMapping(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(em.records) != 5 || len(em.warns) != 0 {
		t.Fatalf("%d records, %d warnings", len(em.records), len(em.warns))
	}
	for i, r := range em.records {
		wantLoc := fmt.Sprintf("sqlite:table=t;rowid=%d", i+1)
		if r.Body != strconv.Itoa((i+1)*10) || r.Locator != wantLoc || r.ArtifactID != "A-DB" || r.Range == nil {
			t.Errorf("record %d = %+v", i, r)
		}
	}
	if len(rep.Tables) != 1 || rep.Tables[0] != (rowrun.TableReport{Table: "t", Rows: 5, Records: 5}) {
		t.Errorf("report = %+v", rep)
	}
}

func TestRunWarnsAndCountsUnmappableRows(t *testing.T) {
	mp := echoMapping(func(_ parse.MapContext, row parse.Row) error {
		if id, _ := row.Rowid(); id%2 == 0 {
			return rowrun.Unmappable("row %d of t is odd", id)
		}
		return nil
	})
	em, rep, err := runEcho(t, 5, mp)
	if err != nil {
		t.Fatal(err)
	}
	if len(em.records) != 3 || len(em.warns) != 2 {
		t.Fatalf("%d records, %d warnings; want 3 and 2", len(em.records), len(em.warns))
	}
	if em.warns[0].Locator != "sqlite:table=t;rowid=2" || em.warns[0].Reason != "row 2 of t is odd" {
		t.Errorf("warning = %+v", em.warns[0])
	}
	if rep.Tables[0].Unmapped != 2 || em.notes["unmapped_rows.t"] != "2" {
		t.Errorf("unmapped = %d, note %q; want 2", rep.Tables[0].Unmapped, em.notes["unmapped_rows.t"])
	}
}

func TestRunTurnsAnUndecidableJoinFromAMapperIntoAWarningAndNoRecord(t *testing.T) {
	mp := echoMapping(func(parse.MapContext, parse.Row) error {
		return fmt.Errorf("lookup of part: %w", sqlitedb.ErrKeyUndecidable)
	})
	em, rep, err := runEcho(t, 3, mp)
	if err != nil {
		t.Fatalf("an undecidable join must not fail the job: %v", err)
	}
	if len(em.records) != 0 || len(em.warns) != 3 || rep.Tables[0].Unmapped != 3 {
		t.Errorf("%d records, %d warnings, %d unmapped; want 0, 3, 3", len(em.records), len(em.warns), rep.Tables[0].Unmapped)
	}
}

func TestRunFailsOnAnyOtherMapperError(t *testing.T) {
	boom := errors.New("boom")
	_, _, err := runEcho(t, 3, echoMapping(func(parse.MapContext, parse.Row) error { return boom }))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

// Backlog item "4D (mapper runner)": a Context failure that Column cannot report
// must fail the job, and the row that saw it must not be emitted.
func TestRunFailsWhenContextErrIsSet(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	em := &recEmitter{}
	mp := echoMapping(func(m parse.MapContext, row parse.Row) error {
		if id, _ := row.Rowid(); id == 2 {
			cancel()
			// a table the Context has not cached: the cancelled context is the failure, not "no such table"
			if m.Column("u", "x") != -1 {
				t.Error("Column of an unknown table is not -1")
			}
		}
		return nil
	})
	_, err := rowrun.Run(ctx, dbInput(t, tinyDB(t, 5)), em, []rowrun.Step{{Mapping: mp}})
	if !errors.Is(err, rowrun.ErrContext) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrContext wrapping context.Canceled", err)
	}
	if len(em.records) != 1 {
		t.Errorf("%d records emitted, want only row 1 (row 2 saw the failure)", len(em.records))
	}
}

func TestRunFailsWhenDatabaseIsReleasedMidRun(t *testing.T) {
	var db *sqlitedb.DB
	em := &recEmitter{}
	mp := echoMapping(func(m parse.MapContext, row parse.Row) error {
		if id, _ := row.Rowid(); id == 2 {
			db.Release()
			m.Column("u", "x")
		}
		return nil
	})
	_, err := rowrun.RunWithSeam(t.Context(), dbInput(t, tinyDB(t, 5)), em, []rowrun.Step{{Mapping: mp}}, func(d *sqlitedb.DB) { db = d })
	if !errors.Is(err, rowrun.ErrContext) || !errors.Is(err, sqlitedb.ErrReleased) {
		t.Fatalf("err = %v, want ErrContext wrapping ErrReleased", err)
	}
	if len(em.records) != 1 {
		t.Errorf("%d records emitted, want 1", len(em.records))
	}
}

func TestCloseChecksContextErr(t *testing.T) {
	in := dbInput(t, tinyDB(t, 2))
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	ctx, cancel := context.WithCancel(t.Context())
	mc := sqlitedb.NewContext(ctx, d, in, nil)
	cancel()
	mc.Column("t", "a") // the cancelled context is recorded
	if err := rowrun.CloseForTest(mc); !errors.Is(err, rowrun.ErrContext) || !errors.Is(err, context.Canceled) {
		t.Errorf("close of a failed context = %v, want ErrContext wrapping context.Canceled", err)
	}
	clean := sqlitedb.NewContext(t.Context(), d, in, nil)
	if err := rowrun.CloseForTest(clean); err != nil {
		t.Errorf("close of a healthy context = %v", err)
	}
}

func TestRunRotatesContextUnderBudgetPressure(t *testing.T) {
	// the mapping charges one owned row per Get; measure the unrotated peak first
	peakOf := func(limit int64) (peak int64, em *recEmitter, rep rowrun.Report) {
		in := dbInput(t, tinyDB(t, 3000))
		in.Budget = parse.NewBudget(limit).View()
		em = &recEmitter{}
		mp := echoMapping(func(m parse.MapContext, row parse.Row) error {
			id, _ := row.Rowid()
			if _, _, err := m.Get("t", id); err != nil {
				return err
			}
			peak = max(peak, in.Budget.Used())
			return nil
		})
		rep, err := rowrun.Run(t.Context(), in, em, []rowrun.Step{{Mapping: mp}})
		if err != nil {
			t.Fatal(err)
		}
		return peak, em, rep
	}
	full, _, rep := peakOf(1 << 30)
	if rep.Rotations != 0 {
		t.Fatalf("an ample budget rotated %d times", rep.Rotations)
	}
	peak, em, rep := peakOf(full + full/4)
	if rep.Rotations < 1 {
		t.Errorf("Rotations = %d under pressure (unrotated peak %d), want at least 1", rep.Rotations, full)
	}
	if peak >= full {
		t.Errorf("peak %d with rotation is not below the unrotated peak %d", peak, full)
	}
	if len(em.records) != 3000 {
		t.Errorf("%d records, want 3000: rotating must not lose a row", len(em.records))
	}
}

func TestRunNotesTheSchemaAndTheColumnsNoMappingReads(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("t", "create table t(id integer primary key, a integer, s text, extra text)").Insert(1, int64(1), int64(5), "x", "y")
	em := &recEmitter{}
	mp := echoMapping(nil)
	mp.Columns = []parse.ColumnSpec{{Name: "a", Required: true}, {Name: "s"}}
	if _, err := rowrun.Run(t.Context(), dbInput(t, b.Bytes()), em, []rowrun.Step{{Mapping: mp}}); err != nil {
		t.Fatal(err)
	}
	if em.notes["schema.t"] != "columns:4;optional:s" || em.notes["unmapped_columns.t"] != "extra" {
		t.Errorf("notes = %v", em.notes)
	}
}

func TestRunOptionalAndRequiredTables(t *testing.T) {
	absent := parse.TableMapping{Table: "nope", Columns: []parse.ColumnSpec{{Name: "a"}}}
	em := &recEmitter{}
	rep, err := rowrun.Run(t.Context(), dbInput(t, tinyDB(t, 1)), em, []rowrun.Step{{Mapping: echoMapping(nil)}, {Mapping: absent, Optional: true}})
	if err != nil || em.notes["table.nope"] != "absent" || !rep.Tables[1].Absent {
		t.Errorf("optional absent table: err %v, notes %v, report %+v", err, em.notes, rep.Tables)
	}
	_, err = rowrun.Run(t.Context(), dbInput(t, tinyDB(t, 1)), &recEmitter{}, []rowrun.Step{{Mapping: absent}})
	if !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("a required absent table: err = %v, want ErrNoSuchTable", err)
	}
	needy := echoMapping(nil)
	needy.Columns = []parse.ColumnSpec{{Name: "missing_col", Required: true}}
	_, err = rowrun.Run(t.Context(), dbInput(t, tinyDB(t, 1)), &recEmitter{}, []rowrun.Step{{Mapping: needy}})
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(err, &us) {
		t.Errorf("a missing required column: err = %v, want *UnsupportedSchemaError", err)
	}
}

// Ruling T18: a database without the identifying table (another kind of database that reached
// Parse because a companion made Probe defer) is not a failed job.
func TestRunIsNotApplicableWhenTheIdentifyingTableIsAbsent(t *testing.T) {
	absent := parse.TableMapping{Table: "calls", Columns: []parse.ColumnSpec{{Name: "number", Required: true}}}
	em := &recEmitter{}
	rep, err := rowrun.Run(t.Context(), dbInput(t, tinyDB(t, 2)), em, []rowrun.Step{{Mapping: absent, Identifying: true}, {Mapping: echoMapping(nil)}})
	if err != nil {
		t.Fatalf("a database of another kind failed the job: %v", err)
	}
	if len(em.records) != 0 || rep.NotApplicable != "table calls is absent" || em.notes["not_applicable"] != "table calls is absent" {
		t.Errorf("records %d, report %q, notes %v", len(em.records), rep.NotApplicable, em.notes)
	}
	if len(em.warns) != 1 || !strings.Contains(em.warns[0].Reason, "not applicable at parse time") {
		t.Errorf("warnings %+v: the skip must be audited as one analysis.warning", em.warns)
	}
	// a required table that is not the identifying one still fails loudly
	_, err = rowrun.Run(t.Context(), dbInput(t, tinyDB(t, 1)), &recEmitter{}, []rowrun.Step{{Mapping: echoMapping(nil)}, {Mapping: absent}})
	if !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("a second required table that is absent: %v, want ErrNoSuchTable", err)
	}
}

func TestRunForwardsLibraryWarningsAndDoesNotHideLostRows(t *testing.T) {
	em := &recEmitter{}
	mp := parse.TableMapping{
		Table: "t", Columns: []parse.ColumnSpec{{Name: "a", Required: true}},
		Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			place, err := rowrun.Where(m, row)
			return []records.Record{{Type: "message", ArtifactID: place.ArtifactID, Locator: place.Locator}}, err
		},
	}
	if _, err := rowrun.Run(t.Context(), dbInput(t, damagedDB(t)), em, []rowrun.Step{{Mapping: mp}}); err != nil {
		t.Fatal(err)
	}
	if len(em.records) >= 300 || len(em.records) == 0 {
		t.Fatalf("%d records from a table with a lost leaf", len(em.records))
	}
	found := false
	for _, w := range em.warns {
		if len(w.Locator) > 15 && w.Locator[:15] == "sqlite:file=db;" {
			found = true
		}
	}
	if !found {
		t.Errorf("no library warning was forwarded: %+v", em.warns)
	}
}

func TestRunCapsItsOwnWarningsAndCountsTheRest(t *testing.T) {
	mp := echoMapping(func(parse.MapContext, parse.Row) error { return rowrun.Unmappable("never") })
	em, rep, err := runEcho(t, 1500, mp)
	if err != nil {
		t.Fatal(err)
	}
	if len(em.warns) != 1000 || rep.Tables[0].Unmapped != 1500 || em.notes["unmapped_rows.t"] != "1500" {
		t.Errorf("%d warnings, %d unmapped, note %q; want 1000, 1500, 1500", len(em.warns), rep.Tables[0].Unmapped, em.notes["unmapped_rows.t"])
	}
}

func TestRunNotesAnIncompleteInput(t *testing.T) {
	in := dbInput(t, tinyDB(t, 1))
	in.Primary.Incomplete = true
	em := &recEmitter{}
	if _, err := rowrun.Run(t.Context(), in, em, []rowrun.Step{{Mapping: echoMapping(nil)}}); err != nil {
		t.Fatal(err)
	}
	if em.notes["incomplete.db"] != "true" {
		t.Errorf("notes = %v, want incomplete.db", em.notes)
	}
}

func TestRunRefusesAnInputWithoutAReader(t *testing.T) {
	in := dbInput(t, tinyDB(t, 1))
	in.Primary.R = nil
	if _, err := rowrun.Run(t.Context(), in, &recEmitter{}, nil); err == nil {
		t.Error("a primary artifact without a reader was accepted")
	}
}
