package sqlitefile_test

// The fit's overflow probe (LX-4 Task 11 fix round, U78-U83): a page with no schema owner is named
// by shape, and a cell whose overflow chain cannot be read to its end is a truncated record that
// cannot support a strict fit. The probe runs only when the fit is reached, counts its pages in a
// budget of its own and says so when it reaches the cap.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const probeLimit = "FitProbeOverflowPages"

// droppedBig is a database whose live table t(a, b) is the only table and whose dropped table
// gone(a, b) held rows with a long text (a chain of three overflow pages each): its leaf page is
// on the freelist, with the chains intact. ov holds the chain of each row, in order.
func droppedBig(t *testing.T, rows int) (db []byte, ov [][]uint32) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b)")
	live.Insert(1, int64(1), "live")
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x") // dropped first: its page becomes the freelist trunk
	gone := b.CreateTable("gone", "create table gone(a, b)")
	for i := range rows {
		gone.Insert(int64(i+1), int64(100+i), longText(3500, byte(i+1)))
		ov = append(ov, gone.Overflow(int64(i+1)))
		if len(ov[i]) != 3 {
			t.Fatalf("overflow pages %v", ov[i])
		}
	}
	b.DropTable("pad")
	b.DropTable("gone")
	return b.Bytes(), ov
}

func openCapped(t *testing.T, db []byte, limit int64) *sqlitefile.Hist {
	t.Helper()
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxHistoryOverflowPages: limit}})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	return h
}

func freelistRowsOf(rows []sqlitefile.RecoveredRow) []sqlitefile.RecoveredRow {
	var out []sqlitefile.RecoveredRow
	for _, r := range rows {
		if r.Method == sqlitefile.MethodFreelist {
			out = append(out, r)
		}
	}
	return out
}

// A healthy spilling record of a dropped table is still named strictly (the probe finds its chain
// whole): the false-positive side, which fails if the probe reports every chain dead.
func TestHistoryRowOfAHealthySpillingRecordStillFitsStrictly(t *testing.T) {
	db, _ := droppedBig(t, 1)
	_, h := openAll(t, db, nil, nil)
	rows, st := collectRows(t, h)
	fr := freelistRowsOf(rows)
	if len(fr) != 1 {
		t.Fatalf("%d freelist rows: %v", len(fr), methods(rows))
	}
	r := fr[0]
	if r.Truncated || r.Table != "t" || r.TableBasis != sqlitefile.BasisFit || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteIdentityByFitOnly) {
		t.Errorf("healthy spilling row: truncated %v table %q basis %s relation %s notes %v, want whole, t, fit, unknown, identity-by-fit-only", r.Truncated, r.Table, r.TableBasis, r.Relation, r.Notes)
	}
	if st.OverflowPagesFollowed != 3 || len(st.LimitsHit) != 0 {
		t.Errorf("stats %+v, want 3 pages followed and no limit", st)
	}
}

// The probe's cap, with N and N+1: the chain has three pages. At 3 the probe reads them all; at 2
// it reaches the cap, says so (a LimitsHit entry of its own and a note on the row), and the strict
// tier is denied. The pages the history follows are counted by the full read alone.
func TestFitProbeCapIsCountedAndDenies(t *testing.T) {
	db, _ := droppedBig(t, 1)
	t.Run("cap 3: the probe fits", func(t *testing.T) {
		rows, st := collectRows(t, openCapped(t, db, 3))
		r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
		if r.Truncated || r.TableBasis != sqlitefile.BasisFit || hasNote(r, sqlitefile.NoteFitProbeLimit) {
			t.Errorf("row truncated %v basis %s notes %v, want whole, fit, no probe note", r.Truncated, r.TableBasis, r.Notes)
		}
		if st.OverflowPagesFollowed != 3 || len(st.LimitsHit) != 0 {
			t.Errorf("stats %+v, want 3 pages followed and no limit", st)
		}
	})
	t.Run("cap 2: the probe reaches it", func(t *testing.T) {
		rows, st := collectRows(t, openCapped(t, db, 2))
		r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
		if r.TableBasis != sqlitefile.BasisGuess || r.Table != "t" || !hasNote(r, sqlitefile.NoteFitProbeLimit) {
			t.Errorf("row table %q basis %s notes %v, want t, guess, the probe-limit note", r.Table, r.TableBasis, r.Notes)
		}
		want := []string{probeLimit, "MaxHistoryOverflowPages"} // the probe runs first, then the full read reaches the same cap
		if !slices.Equal(st.LimitsHit, want) || st.OverflowPagesFollowed != 2 {
			t.Errorf("stats %+v, want limits %v and 2 pages followed (the probe's pages are not counted there)", st, want)
		}
	})
}

// An owned page never reaches the fit, so the probe does not run on it: with a cap far below the
// chains the history follows, no probe limit is recorded.
func TestFitProbeDoesNotRunOnAnOwnedPage(t *testing.T) {
	db, journal, _ := capScen(t)
	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxHistoryOverflowPages: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	rows, st := collectRows(t, h)
	if len(rows) != 4 || !slices.Equal(st.LimitsHit, []string{"MaxHistoryOverflowPages"}) {
		t.Errorf("%d rows, limits %v, want 4 rows and only the full read's cap", len(rows), st.LimitsHit)
	}
	for _, r := range rows {
		if r.TableBasis != sqlitefile.BasisSchema {
			t.Errorf("row %+v is not on the schema basis", r)
		}
	}
}

// A chain that points at itself, and chains that share their pages, on the probe's path.
func TestFitProbeCycleAndSharedChains(t *testing.T) {
	t.Run("a cycle", func(t *testing.T) {
		db, ov := droppedBig(t, 1)
		binary.BigEndian.PutUint32(db[(ov[0][1]-1)*hps:], ov[0][1]) // the second page of the chain names itself
		_, h := openAll(t, db, nil, nil)
		rows, st := collectRows(t, h)
		r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
		if !r.Truncated || r.TableBasis != sqlitefile.BasisGuess || len(st.LimitsHit) != 0 {
			t.Errorf("cycle: truncated %v basis %s limits %v, want truncated, guess, no limit", r.Truncated, r.TableBasis, st.LimitsHit)
		}
	})
	t.Run("two rows share one chain", func(t *testing.T) {
		db, ov := droppedBig(t, 2)
		leaf := freelistLeafOf(t, db)
		p := db[(leaf-1)*hps : leaf*hps]
		patched := 0
		for _, off := range cellOffsets(p) {
			if i := bytes.Index(p[off:], chainHeadBytes(ov[1][0])); i >= 0 {
				copy(p[off+i:], chainHeadBytes(ov[0][0])) // the second row's chain is the first row's
				patched++
			}
		}
		if patched != 1 {
			t.Fatalf("patched %d chain heads", patched)
		}
		_, h := openAll(t, db, nil, nil)
		rows, st := collectRows(t, h)
		fr := freelistRowsOf(rows)
		if len(fr) != 2 {
			t.Fatalf("%d freelist rows", len(fr))
		}
		for _, r := range fr {
			if r.Truncated || r.TableBasis != sqlitefile.BasisFit {
				t.Errorf("row truncated %v basis %s, want whole and fit", r.Truncated, r.TableBasis)
			}
		}
		if len(st.LimitsHit) != 0 {
			t.Errorf("limits %v", st.LimitsHit)
		}
	})
}

func chainHeadBytes(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// freelistLeafOf is the table leaf page that is not page 2 (the live table's).
func freelistLeafOf(t *testing.T, db []byte) uint32 {
	t.Helper()
	for pg := uint32(3); int(pg)*hps <= len(db); pg++ {
		if db[(pg-1)*hps] == 0x0d {
			return pg
		}
	}
	t.Fatal("no leaf page")
	return 0
}

// The history rows of a dropped table's page, not a donor page copied onto a freelist page: a
// chain that is cut, and a record with a reserved serial type, are named at BasisGuess at best.
func TestHistoryRowOfADroppedTablePageIsNotNamedByAStrictFit(t *testing.T) {
	t.Run("a cut chain", func(t *testing.T) {
		db, ov := droppedBig(t, 1)
		binary.BigEndian.PutUint32(db[(ov[0][1]-1)*hps:], 0x00ffffff) // the second page names a page past the file
		_, h := openAll(t, db, nil, nil)
		rows, st := collectRows(t, h)
		r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
		if !r.Truncated || r.Table != "t" || r.TableBasis != sqlitefile.BasisGuess || hasNote(r, sqlitefile.NoteFitProbeLimit) || len(st.LimitsHit) != 0 {
			t.Errorf("cut chain: truncated %v table %q basis %s notes %v limits %v, want truncated, t, guess, no probe note, no limit", r.Truncated, r.Table, r.TableBasis, r.Notes, st.LimitsHit)
		}
	})
	t.Run("a reserved serial type", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		live := b.CreateTable("t", "create table t(a, b)")
		live.Insert(1, int64(1), "live")
		pad := b.CreateTable("pad", "create table pad(z)")
		pad.Insert(1, "x")
		gone := b.CreateTable("gone", "create table gone(a, b)")
		gone.InsertRaw(1, []uint64{1, 10}, []byte{5}) // b has the reserved serial type 10
		b.DropTable("pad")
		b.DropTable("gone")
		_, h := openAll(t, b.Bytes(), nil, nil)
		rows, _ := collectRows(t, h)
		r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
		if r.Truncated || r.Values[1].Serial != 10 || r.Table != "t" || r.TableBasis != sqlitefile.BasisGuess {
			t.Errorf("reserved: truncated %v values %+v table %q basis %s, want whole, serial 10, t, guess", r.Truncated, r.Values, r.Table, r.TableBasis)
		}
	})
}

// cancelAt cancels a context when an overflow page of the chain is read after arm was set, and
// counts the reads of those pages.
type cancelAt struct {
	data   []byte
	pages  map[int64]bool // file offsets of the chain pages
	armed  bool
	reads  int
	cancel func()
}

func (c *cancelAt) ReadAt(p []byte, off int64) (int, error) {
	if c.armed && c.pages[off/hps*hps] {
		c.reads++
		c.cancel()
	}
	return bytes.NewReader(c.data).ReadAt(p, off)
}

// A cancel that arrives while the fit's probe reads an overflow chain (a dropped table's page has no
// owner, so the fit is reached) ends the Rows call with the context error: no row is delivered with
// a label the probe did not finish proving. The full read that follows also meets the cancelled
// context, so this pins the outcome, not which layer reports it.
func TestCancelWhileTheFitProbeReadsReachesTheCaller(t *testing.T) {
	db, ov := droppedBig(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := &cancelAt{data: db, pages: map[int64]bool{}, cancel: cancel}
	for _, pg := range ov[0] {
		cr.pages[int64(pg-1)*hps] = true
	}
	d, err := sqlitefile.Open(cr, int64(len(db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	cr.armed = true
	var got []sqlitefile.RecoveredRow
	_, err = h.Rows(ctx, func(r sqlitefile.RecoveredRow) bool {
		got = append(got, r)
		return true
	})
	if cr.reads == 0 {
		t.Fatal("the overflow chain was never read after the history started: the cancel did not fire")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Rows error %v, want context.Canceled", err)
	}
	for _, r := range got {
		if r.Method == sqlitefile.MethodFreelist {
			t.Errorf("a freelist row was delivered after the cancel: table %q basis %s", r.Table, r.TableBasis)
		}
	}
}

// The same outcome when the cancel lands on the second page of the chain, with the history's
// values clipped to 16 bytes (so the full read of the row does not follow the chain): Rows ends
// with the context error and delivers no row. The error can come from the probe or from the
// reads and polls that follow it; this test pins the outcome, not which of them reports it.
func TestCancelDuringTheFitProbeReachesTheCaller(t *testing.T) {
	db, ov := droppedBig(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := &cancelAt{data: db, pages: map[int64]bool{int64(ov[0][1]-1) * hps: true}, cancel: cancel}
	d, err := sqlitefile.Open(cr, int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxRecoveredValueBytes: 16}})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	cr.armed = true
	var got []sqlitefile.RecoveredRow
	_, err = h.Rows(ctx, func(r sqlitefile.RecoveredRow) bool {
		got = append(got, r)
		return true
	})
	if cr.reads == 0 {
		t.Fatal("the second page of the chain was never read: the cancel did not fire")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Rows error %v, want context.Canceled", err)
	}
	if len(got) != 0 {
		t.Errorf("%d rows delivered after the cancel: %+v", len(got), got[0])
	}
}
