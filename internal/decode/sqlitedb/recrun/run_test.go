package recrun_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func TestRecoverMapsEveryClaimedRow(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	order, ref, _ := refRows(t, f.db, nil, nil, "people")
	em := newEmitter(t)
	rep, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("people", 4)}, claim("people"))
	if err != nil {
		t.Fatal(err)
	}
	var wantRows []fxHistory
	for _, h := range f.exp.History {
		if h.Table == "people" && (h.Basis == "schema" || h.Basis == "fit") {
			wantRows = append(wantRows, h)
		}
	}
	if len(wantRows) != 12 || len(order) != 12 {
		t.Fatalf("the oracle has %d claimable people rows, the library %d; want 12", len(wantRows), len(order))
	}
	if len(rep.Tables) != 1 || rep.Tables[0].Table != "people" {
		t.Fatalf("tables %+v", rep.Tables)
	}
	tc := tc0(rep)
	if tc.Delivered != 12 || tc.Delivered != tc.Mapped+tc.Unmapped || tc.Records != 12 || tc.Absent {
		t.Errorf("counts %+v", tc)
	}
	if len(em.recs) != 12 || len(em.unmapped) != 0 || rep.Unmapped != 0 {
		t.Fatalf("%d records, %d unmapped calls, report unmapped %d", len(em.recs), len(em.unmapped), rep.Unmapped)
	}
	if rep.History.Delivered != 12 {
		t.Errorf("history delivered %d", rep.History.Delivered)
	}
	idx := byRange(t, em.recs)
	for _, h := range wantRows {
		e, ok := idx[rangeKey(h)]
		if !ok {
			t.Errorf("rowid %d: no record at %s", *h.Rowid, rangeKey(h))
			continue
		}
		if err := e.Facts.Validate(); err != nil {
			t.Errorf("rowid %d: %v", *h.Rowid, err)
		}
		if want, ok := ref[e.Facts.Locator]; !ok || !reflect.DeepEqual(e.Facts, want) {
			t.Errorf("rowid %d: facts %+v, FactsOf of the row %+v (known %v)", *h.Rowid, e.Facts, want, ok)
		}
		// the independent oracle
		if e.Facts.Method != h.Method || e.Facts.Relation != h.Relation || e.Facts.Origin != h.Origin || e.Facts.Basis != h.Basis ||
			e.Facts.Confidence != h.Confidence || e.Facts.Page != h.Cell.Page {
			t.Errorf("rowid %d: facts %+v, oracle %+v", *h.Rowid, e.Facts, h)
		}
		if want := append(slices.Clone(h.Notes), recrun.NoteJoinsFromLiveView); !slices.Equal(e.Facts.Notes, want) {
			t.Errorf("rowid %d: notes %v, want %v", *h.Rowid, e.Facts.Notes, want)
		}
		p := e.Rec.Payload
		if p["rowid"] != *h.Rowid {
			t.Errorf("rowid %d: payload rowid %v", *h.Rowid, p["rowid"])
		}
		for i, v := range h.Values {
			want := oracleText(v)
			if i == 0 { // the INTEGER PRIMARY KEY alias reads as the rowid
				want = "i" + strconv.FormatInt(*h.Rowid, 10)
			}
			if got := p["c"+strconv.Itoa(i)]; got != want {
				t.Errorf("rowid %d: column %d = %v, want %s", *h.Rowid, i, got, want)
			}
		}
	}
	for i, e := range em.recs {
		if e.Facts.Basis == "guess" || e.Facts.Basis == "none" {
			t.Errorf("record %d typed from a %s row", i, e.Facts.Basis)
		}
	}
	if em.notes["recovered_rows.people"] != "12" {
		t.Errorf("notes %v", em.notes)
	}
}

// TestFitDecoyIsAttributedToTheClaimedTable pins a KNOWN misattribution (4G-R23), it does not
// show correctness: the rows of a table dropped from the database (d_fit) have the shape of the
// claimed live table people, so the structural fit names them people. They are delivered as
// people with basis fit and the note identity-by-fit-only, which is the only warning the record carries.
func TestFitDecoyIsAttributedToTheClaimedTable(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	em := newEmitter(t)
	if _, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("people", 4)}, claim("people")); err != nil {
		t.Fatal(err)
	}
	decoys := 0
	for _, e := range em.recs {
		name, _ := e.Rec.Payload["c1"].(string)
		if !strings.HasPrefix(name, "tfitted-old-") {
			continue
		}
		decoys++
		if e.Facts.Table != "people" || e.Facts.Basis != "fit" || e.Facts.Confidence != 60 || !slices.Contains(e.Facts.Notes, "identity-by-fit-only") {
			t.Errorf("decoy %s: facts %+v", name, e.Facts)
		}
	}
	if decoys != 12 {
		t.Errorf("%d decoy rows attributed to people, want 12", decoys)
	}
}

func TestRecoverFactsFromWALAndJournal(t *testing.T) {
	t.Run("wal", func(t *testing.T) {
		f := loadFixture(t, "builder-wal-generations")
		em := newEmitter(t)
		rep, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("notes", 3)}, claim("notes"))
		if err != nil {
			t.Fatal(err)
		}
		idx := byRange(t, em.recs)
		page := int64(f.exp.Header.PageSize)
		var wal, db, found int
		for _, h := range f.exp.History {
			if h.Table != "notes" || h.Basis != "schema" {
				continue
			}
			e, ok := idx[rangeKey(h)]
			if !ok {
				t.Errorf("rowid %d (%s at %s): no record", *h.Rowid, h.Method, rangeKey(h))
				continue
			}
			found++
			if err := e.Facts.Validate(); err != nil {
				t.Errorf("%v", err)
			}
			if string(e.Facts.Role) != h.Cell.File || e.Facts.Method != h.Method || e.Facts.Relation != h.Relation || e.Facts.Confidence != h.Confidence {
				t.Errorf("rowid %d: facts %+v, oracle %+v", *h.Rowid, e.Facts, h)
			}
			if h.Cell.File != "wal" {
				db++
				if e.Facts.WAL != nil || e.Facts.Journal != nil {
					t.Errorf("rowid %d in the database file carries %+v %+v", *h.Rowid, e.Facts.WAL, e.Facts.Journal)
				}
				continue
			}
			wal++
			var fr *fxFrame
			for i := range f.exp.WAL.Frames {
				if o := f.exp.WAL.Frames[i].Offset; h.Cell.Offset >= o+24 && h.Cell.Offset < o+24+page {
					fr = &f.exp.WAL.Frames[i]
				}
			}
			if fr == nil {
				t.Fatalf("rowid %d: no frame holds offset %d", *h.Rowid, h.Cell.Offset)
			}
			want := parse.RecoveredWAL{Frame: fr.Slot, Salt1: fr.Salt1, Salt2: fr.Salt2, Committed: fr.State == "committed"}
			if e.Facts.WAL == nil || *e.Facts.WAL != want || e.Facts.Journal != nil {
				t.Errorf("rowid %d: WAL facts %+v, want %+v", *h.Rowid, e.Facts.WAL, want)
			}
		}
		if wal == 0 || db == 0 || found != int(tc0(rep).Delivered)-fitDelivered(em) {
			t.Errorf("%d wal rows, %d database rows, %d found of %d delivered", wal, db, found, tc0(rep).Delivered)
		}
	})
	t.Run("journal", func(t *testing.T) {
		f := loadFixture(t, "persist-journal")
		em := newEmitter(t)
		if _, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("kv", 3)}, claim("kv")); err != nil {
			t.Fatal(err)
		}
		idx := byRange(t, em.recs)
		n := 0
		for _, h := range f.exp.History {
			if h.Table != "kv" || h.Basis != "schema" {
				continue
			}
			e, ok := idx[rangeKey(h)]
			if !ok {
				t.Errorf("rowid %d: no record at %s", *h.Rowid, rangeKey(h))
				continue
			}
			n++
			j := e.Facts.Journal
			if e.Facts.Role != parse.RoleJournal || j == nil || e.Facts.WAL != nil || j.Record < 0 || j.Hot || j.Applied {
				t.Errorf("rowid %d: role %s journal %+v wal %+v (the journal is not hot and not applied)", *h.Rowid, e.Facts.Role, j, e.Facts.WAL)
			}
			if e.Facts.Method != h.Method || e.Facts.Relation != h.Relation || e.Facts.Confidence != h.Confidence {
				t.Errorf("rowid %d: facts %+v, oracle %+v", *h.Rowid, e.Facts, h)
			}
		}
		if n == 0 {
			t.Error("no journal row compared")
		}
	})
	t.Run("hot journal hides database pages", func(t *testing.T) {
		f := loadFixture(t, "builder-hot-journal")
		em := newEmitter(t)
		if _, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("a", 2)}, claim("a")); err != nil {
			t.Fatal(err)
		}
		idx := byRange(t, em.recs)
		n := 0
		for _, h := range f.exp.History {
			if h.Table != "a" || h.Basis != "schema" {
				continue
			}
			e, ok := idx[rangeKey(h)]
			if !ok {
				t.Errorf("rowid %d: no record at %s", *h.Rowid, rangeKey(h))
				continue
			}
			n++
			// the rolled-back image lies in the database file: no journal position is claimed for it
			if e.Facts.Role != parse.RoleDB || e.Facts.Journal != nil || e.Facts.Method != h.Method || e.Facts.Relation != h.Relation || e.Facts.Confidence != h.Confidence {
				t.Errorf("rowid %d: facts %+v, oracle %+v", *h.Rowid, e.Facts, h)
			}
		}
		if n != 40 {
			t.Errorf("%d rows compared, want 40", n)
		}
	})
}

// fitDelivered counts the delivered rows whose basis is fit (the oracle comparison above covers schema rows).
func fitDelivered(em *recEmitter) int {
	n := 0
	for _, e := range em.recs {
		if e.Facts.Basis == "fit" {
			n++
		}
	}
	return n
}

func TestRecoverUnmappedRowsAreCountedWarnings(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	order, ref, _ := refRows(t, f.db, nil, nil, "people")
	m := fakeMapping("people", 4)
	inner := m.Map
	m.Map = func(mc parse.MapContext, row parse.Row) ([]records.Record, error) {
		id, _ := row.Rowid()
		switch {
		case id == 104:
			return nil, nil
		case id%2 == 1:
			return nil, rowrun.Unmappable("x")
		}
		return inner(mc, row)
	}
	em := newEmitter(t)
	rep, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{m}, claim("people"))
	if err != nil {
		t.Fatal(err)
	}
	// rowids 101..112: six odd ones are unmappable, 104 yields no record, five are mapped
	tc := tc0(rep)
	if tc.Delivered != 12 || tc.Mapped != 5 || tc.Unmapped != 7 || tc.Records != 5 || rep.Unmapped != 7 {
		t.Errorf("counts %+v, report unmapped %d", tc, rep.Unmapped)
	}
	if len(em.recs) != 5 || len(em.unmapped) != 7 {
		t.Fatalf("%d records, %d unmapped calls", len(em.recs), len(em.unmapped))
	}
	seen := map[string]bool{}
	for _, u := range em.unmapped {
		if u.Facts.Locator == "" || !strings.HasPrefix(u.Facts.Locator, "sqlite:table=people;page=") || !slices.Contains(order, u.Facts.Locator) || seen[u.Facts.Locator] {
			t.Errorf("unmapped locator %q", u.Facts.Locator)
		}
		seen[u.Facts.Locator] = true
		if !reflect.DeepEqual(u.Facts, ref[u.Facts.Locator]) || u.Facts.Validate() != nil {
			t.Errorf("unmapped facts %+v", u.Facts)
		}
		if u.Reason != "x" && u.Reason != "the mapping produced no record" {
			t.Errorf("reason %q", u.Reason)
		}
	}
	if em.notes["unmapped_recovered.people"] != "7" || em.notes["recovered_rows.people"] != "12" {
		t.Errorf("notes %v", em.notes)
	}
	for _, e := range em.recs {
		if seen[e.Facts.Locator] {
			t.Errorf("%s was mapped and reported unmapped", e.Facts.Locator)
		}
	}
}

func TestRecoverUndecidableJoinIsUnmappedNotFatal(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	m := fakeMapping("people", 4)
	m.Map = func(parse.MapContext, parse.Row) ([]records.Record, error) { return nil, sqlitedb.ErrKeyUndecidable }
	em := newEmitter(t)
	rep, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{m}, claim("people"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unmapped != 12 || len(em.unmapped) != 12 || len(em.recs) != 0 || !strings.Contains(firstReason(em), "undecidable") {
		t.Errorf("unmapped %d (%d calls), %d records, first reason %q", rep.Unmapped, len(em.unmapped), len(em.recs), firstReason(em))
	}
}

func TestRecoverOtherMappingErrorIsFatal(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	boom := errors.New("boom")
	calls := 0
	m := fakeMapping("people", 4)
	m.Map = func(parse.MapContext, parse.Row) ([]records.Record, error) { calls++; return nil, boom }
	em := newEmitter(t)
	_, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{m}, claim("people"))
	if !errors.Is(err, boom) || calls != 1 || len(em.unmapped) != 0 {
		t.Errorf("err %v after %d calls, %d unmapped", err, calls, len(em.unmapped))
	}
}

func TestRecoverAbsentClaimedTableIsNotSilent(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	order, _, _ := refRows(t, f.db, nil, nil, "no_such_table")
	if len(order) != 0 {
		t.Fatalf("%d rows claimed for a table the live schema lacks", len(order))
	}
	em := newEmitter(t)
	rep, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("no_such_table", 2)}, claim("no_such_table"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Tables) != 1 || !rep.Tables[0].Absent || rep.Tables[0].Delivered != 0 || len(em.recs) != 0 {
		t.Errorf("tables %+v, %d records", rep.Tables, len(em.recs))
	}
	if em.notes["table.no_such_table"] != "absent" {
		t.Errorf("notes %v", em.notes)
	}
	// every row of the history is accounted for by a verdict: nothing is dropped without a count
	_, _, want := refRows(t, f.db, nil, nil, "no_such_table")
	var skipped, total int64
	for _, v := range rep.History.Skipped {
		skipped += v
	}
	for _, v := range want {
		total += v
	}
	if skipped == 0 || skipped != total || !reflect.DeepEqual(rep.History.Skipped, want) {
		t.Errorf("skipped %d of %d history rows: %v, want %v", skipped, total, rep.History.Skipped, want)
	}
}

func TestRecoverMissingRequiredColumnIsUnmappedNotSilent(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	m := fakeMapping("people", 4)
	m.Columns = []parse.ColumnSpec{{Name: "name", Required: true}, {Name: "no_such_column", Required: true}}
	em := newEmitter(t)
	rep, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{m}, claim("people"))
	if err != nil {
		t.Fatal(err)
	}
	if tc0(rep).Delivered != 12 || tc0(rep).Unmapped != 12 || rep.Unmapped != 12 || len(em.recs) != 0 || len(em.unmapped) != 12 {
		t.Fatalf("counts %+v, %d records, %d unmapped calls", tc0(rep), len(em.recs), len(em.unmapped))
	}
	for _, u := range em.unmapped {
		if !strings.Contains(u.Reason, "no_such_column") || u.Facts.Validate() != nil {
			t.Errorf("reason %q facts %+v", u.Reason, u.Facts)
		}
	}
}

func TestRecoverClaimWithoutMappingIsAnErrorBeforeAnyRow(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	em := newEmitter(t)
	_, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping("events", 2)}, claim("people"))
	if !errors.Is(err, recrun.ErrNoMapping) || len(em.recs) != 0 || len(em.unmapped) != 0 {
		t.Errorf("err %v, %d records", err, len(em.recs))
	}
	_, err = recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{{Table: "people"}}, claim("people"))
	if !errors.Is(err, rowrun.ErrNilMap) {
		t.Errorf("a mapping without Map: %v", err)
	}
	_, err = recrun.Run(t.Context(), nil, em, nil, claim("people"))
	if !errors.Is(err, rowrun.ErrNilInput) {
		t.Errorf("no input: %v", err)
	}
}

// TestRecoverNeverEmitsLive (PASSES ON ARRIVAL: Run has no Emit call to make). The recording
// emitter fails the test on any Emit call; every other test here shares it, and this one pins
// the property over all fixtures at once. Mutation: route one record through out.Emit in
// Run, see it fail, restore.
func TestRecoverNeverEmitsLive(t *testing.T) {
	for _, c := range []struct {
		fixture, table string
		cols           int
	}{{"sqlite3-freelist", "people", 4}, {"builder-wal-generations", "notes", 3}, {"persist-journal", "kv", 3}, {"builder-hot-journal", "a", 2}} {
		f := loadFixture(t, c.fixture)
		em := newEmitter(t)
		if _, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{fakeMapping(c.table, c.cols)}, claim(c.table)); err != nil {
			t.Fatal(err)
		}
		if len(em.recs) == 0 || em.liveEmit != 0 {
			t.Errorf("%s: %d recovered records, %d live Emit calls", c.fixture, len(em.recs), em.liveEmit)
		}
	}
}

func TestRecoverStopsOnContextFailure(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := fakeMapping("people", 4)
	inner := m.Map
	calls := 0
	m.Map = func(mc parse.MapContext, row parse.Row) ([]records.Record, error) {
		calls++
		cancel()
		mc.Column("never_looked_up", "x") // the Context records the cancellation it cannot report
		return inner(mc, row)
	}
	em := newEmitter(t)
	_, err := recrun.Run(ctx, f.input(), em, []parse.TableMapping{m}, claim("people"))
	if !errors.Is(err, rowrun.ErrContext) {
		t.Errorf("err = %v, want rowrun.ErrContext", err)
	}
	if calls != 1 || len(em.recs) != 0 || len(em.unmapped) != 0 {
		t.Errorf("%d mapping calls, %d records, %d unmapped after the context failed", calls, len(em.recs), len(em.unmapped))
	}
}

func TestRecoverJoinsReadTheLiveView(t *testing.T) {
	f := loadFixture(t, "sqlite3-freelist")
	live := f.exp.Live["people"].Rows
	if len(live) == 0 {
		t.Fatal("the fixture has no live people")
	}
	m := fakeMapping("people", 4)
	inner := m.Map
	m.Map = func(mc parse.MapContext, row parse.Row) ([]records.Record, error) {
		recs, err := inner(mc, row)
		if err != nil {
			return nil, err
		}
		lr, ok, err := mc.Get("people", 1)
		if err != nil || !ok {
			return nil, rowrun.Unmappable("no live row 1: %v", err)
		}
		recs[0].Payload["live1"] = cell(lr, 1)
		return recs, nil
	}
	em := newEmitter(t)
	if _, err := recrun.Run(t.Context(), f.input(), em, []parse.TableMapping{m}, claim("people")); err != nil {
		t.Fatal(err)
	}
	if len(em.recs) != 12 {
		t.Fatalf("%d records", len(em.recs))
	}
	want := oracleText(live[0][2]) // the name of live people row 1
	for _, e := range em.recs {
		if e.Rec.Payload["live1"] != want {
			t.Errorf("join read %v, the live row says %s", e.Rec.Payload["live1"], want)
		}
		if !slices.Contains(e.Facts.Notes, recrun.NoteJoinsFromLiveView) {
			t.Errorf("notes %v lack %s", e.Facts.Notes, recrun.NoteJoinsFromLiveView)
		}
	}
}
