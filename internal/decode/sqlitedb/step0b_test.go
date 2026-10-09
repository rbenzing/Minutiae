package sqlitedb_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// The decoder's own cancellation poll (parse.Tick) ends a scan with the context
// error: the context is cancelled in the callback of row TickEvery-1, and the
// poll fires on row TickEvery before the reader's own page-level check can.
func TestScanTickErrorEndsTheScan(t *testing.T) {
	data := smallTable(t, parse.TickEvery+300)
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	n := 0
	err = tb.Scan(ctx, func(sqlitedb.Row) error {
		n++
		if n == parse.TickEvery {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan = %v after %d rows, want context.Canceled", err, n)
	}
	if n != parse.TickEvery {
		t.Errorf("%d rows delivered, want %d (the Tick poll ends the scan before row %d is delivered)", n, parse.TickEvery, parse.TickEvery+1)
	}
}

// The layer's own recover: a panic in the layer's code AFTER the library has
// returned is ErrInternal (never ErrCorrupt, never an escaping panic).
func TestOpenAndGetOwnRecoverIsErrInternal(t *testing.T) {
	data := smallTable(t, 20)
	boom := func() { panic("layer bug") }
	t.Run("Open", func(t *testing.T) {
		b := bigBudget()
		_, err := sqlitedb.OpenAfter(t.Context(), filesOf(data, nil, nil), b, boom)
		if !errors.Is(err, sqlitedb.ErrInternal) || errors.Is(err, sqlitedb.ErrCorrupt) {
			t.Fatalf("Open = %v, want ErrInternal only", err)
		}
		if b.used != 0 {
			t.Errorf("a panicking Open left %d bytes charged", b.used)
		}
	})
	t.Run("Get", func(t *testing.T) {
		d := openBytes(t, data, nil, nil, bigBudget())
		tb, err := d.Table(t.Context(), "t", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, found, err := tb.GetAfter(t.Context(), 3, boom)
		if !errors.Is(err, sqlitedb.ErrInternal) || errors.Is(err, sqlitedb.ErrCorrupt) || found {
			t.Fatalf("Get = %v found=%v, want ErrInternal only", err, found)
		}
	})
}

// A refusal inside the library, before the schema is read: sqlitefile.Open
// itself charges nothing, so the first charges are those of attaching the WAL
// and the journal. Each is parse.ErrBudget with nothing left charged.
func TestBudgetRefusedWhileAttachingCompanions(t *testing.T) {
	check := func(t *testing.T, f sqlitedb.Files) {
		t.Helper()
		b := &testBudget{limit: 0}
		_, err := sqlitedb.Open(t.Context(), f, b)
		if !errors.Is(err, parse.ErrBudget) {
			t.Fatalf("Open = %v, want parse.ErrBudget", err)
		}
		if b.used != 0 {
			t.Errorf("a refused Open left %d bytes charged", b.used)
		}
	}
	t.Run("WAL", func(t *testing.T) {
		db, wal := walDB(t)
		check(t, filesOf(db, wal(), nil))
	})
	t.Run("journal", func(t *testing.T) {
		db, journal := hotJournalDB(t, 1024, "")
		check(t, filesOf(db, nil, journal))
	})
}

// Clone copies the WAL and journal provenance (not the pointers), and charges
// the column states.
func TestCloneCopiesProvenanceAndChargesStates(t *testing.T) {
	data := smallTable(t, 5)
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := tb.Get(t.Context(), 2)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	w := &sqlitefile.WALProv{Frame: 7, Note: "w"}
	j := &sqlitefile.JournalProv{Record: 3, Note: "j"}
	rec := sqlitedb.WithRecovered(row, w, j)
	c, err := rec.Clone()
	if err != nil {
		t.Fatal(err)
	}
	cw, cj := sqlitedb.Prov(c)
	if cw == w || cj == j || cw == nil || cj == nil {
		t.Fatalf("Clone shares or loses provenance pointers: %p %p", cw, cj)
	}
	if *cw != *w || *cj != *j {
		t.Errorf("Clone changed provenance: %+v %+v", *cw, *cj)
	}
	cw.Note, cj.Note = "x", "x"
	if w.Note != "w" || j.Note != "j" {
		t.Error("writing the clone's provenance changed the original's")
	}
	// The states term: the charge covers one byte per column on top of the
	// values (2 columns here), so it is at least that much above the bare cost.
	want := int64(2)*sqlitedb.RowValueCost + 2 + 30 // values, one state byte per column, the 30 text bytes
	if got := row.CloneCost(); got != want {
		t.Errorf("cloneCost = %d, want %d", got, want)
	}
}
