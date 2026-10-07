package sqlitefile_test

// Task 12 review pins (plan 3I, Task 13 step 0): the owner of a page is the
// owner when the image was written, and the branches of the history-row code the
// review found unpinned.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// ownerScen: the live table t(a, b not null) is on page 2. An older era of the
// database had table z(x, y) on page 2; the WAL holds the old page 1 (schema of
// that era) and the old page 2, then the live pages.
func ownerScen() (db, wal []byte) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b not null)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	liveImg := b.Snapshot()
	db = walMode(withCount(b.Bytes(), liveImg.Pages()))
	b2 := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	z := b2.CreateTable("z", "create table z(x, y)")
	for id := int64(1); id <= 3; id++ {
		z.Insert(id, id*7, "old")
	}
	old := b2.Snapshot()
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(1, old.Page(1), 2) // slot 1: the schema of the old era
	w.Frame(2, old.Page(2), 2) // slot 2: the cells of z
	w.Frame(1, liveImg.Page(1), 2)
	w.Frame(2, liveImg.Page(2), 2)
	return db, w.Bytes()
}

// TestOwnerAtWriteTimeNotToday (ruling I-1): cells written when page 2 belonged
// to z are never BasisSchema of today's owner t and never compared with the rows
// of t: the relation is unknown with the owner-changed note.
func TestOwnerAtWriteTimeNotToday(t *testing.T) {
	db, wal := ownerScen()
	_, h := openAll(t, db, wal, nil)
	rows, _ := collectRows(t, h)
	n := 0
	for _, r := range rows {
		if r.WAL == nil || r.WAL.Frame != 2 {
			continue
		}
		n++
		if r.TableBasis == sqlitefile.BasisSchema || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteOwnerChanged) {
			t.Errorf("row of the old era: basis %s relation %s notes %v", r.TableBasis, r.Relation, r.Notes)
		}
	}
	if n != 3 {
		t.Fatalf("%d rows of slot 2 in %v", n, methods(rows))
	}
}

// TestOwnerAtWriteTimeUnavailableIsUnknown: when the as-of schema of the image
// cannot be read (page 1 has a later frame, so the database-file page is not
// trusted for the time of slot 1), the owner is not taken from today's schema.
func TestOwnerAtWriteTimeUnavailableIsUnknown(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	live := b.CreateTable("t", "create table t(a, b not null)")
	for id := int64(1); id <= 3; id++ {
		live.Insert(id, id, "live")
	}
	liveImg := b.Snapshot()
	db := walMode(withCount(b.Bytes(), liveImg.Pages()))
	b2 := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	z := b2.CreateTable("t", "create table t(a, b not null)")
	for id := int64(1); id <= 3; id++ {
		z.Insert(id, id*7, "old")
	}
	old := b2.Snapshot()
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, old.Page(2), 2) // slot 1: page 1 has no frame yet, only a later one
	w.Frame(1, liveImg.Page(1), 2)
	w.Frame(2, liveImg.Page(2), 2) // the later frame that makes slot 1 history
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	n := 0
	for _, r := range rows {
		if r.WAL != nil && r.WAL.Frame == 1 {
			n++
			if r.TableBasis == sqlitefile.BasisSchema || r.Relation != sqlitefile.RelUnknown || !hasNote(r, sqlitefile.NoteOwnerChanged) {
				t.Errorf("row %+v notes %v", r, r.Notes)
			}
		}
	}
	if n == 0 {
		t.Fatalf("no row of slot 1 in %v", methods(rows))
	}
}

// TestOldLeafOfNowInteriorPageKeepsItsOwner: the page that was a leaf of t and
// is an interior page of t today is still t's (the owner class check accepts an
// interior page).
func TestOldLeafOfNowInteriorPageKeepsItsOwner(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		tb.Insert(id, id, longText(100, byte(id)))
	}
	old := b.Snapshot()
	for id := int64(4); id <= 12; id++ {
		tb.Insert(id, id, longText(100, byte(id)))
	}
	tb.Update(2, int64(99), longText(100, 77))
	if got := tb.Pages(); len(got) != 3 {
		t.Fatalf("pages %v", got)
	}
	pages := b.Snapshot().Pages()
	db := walMode(withCount(b.Bytes(), pages))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, old.Page(2), pages)
	w.Frame(2, b.Snapshot().Page(2), pages) // page 2 is an interior page today
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	n := 0
	for _, r := range rows {
		if r.WAL != nil {
			n++
			if r.TableBasis != sqlitefile.BasisSchema || r.Table != "t" {
				t.Errorf("row %+v", r)
			}
		}
	}
	if n == 0 {
		t.Fatalf("no row of the old leaf in %v", methods(rows))
	}
}

// TestUncommittedAbsentFromLiveIsUncommitted: an uncommitted row whose rowid is
// not live is `uncommitted`, never absent-from-live.
func TestUncommittedAbsentFromLiveIsUncommitted(t *testing.T) {
	s := newRowScen(t)
	s.t.Insert(7, int64(70), tText(7, 0))
	s1 := s.b.Snapshot()
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s1.Page(2), 0)
	_, h := openAll(t, walMode(s.db0), w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	found := false
	for _, r := range rows {
		if r.Rowid != nil && *r.Rowid == 7 {
			found = true
			if r.Relation != sqlitefile.RelUncommitted || r.Method != sqlitefile.MethodWALUncommitted {
				t.Errorf("row %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("row 7 not delivered")
	}
}

// TestBrokenWALFrameYieldsStaleRows: a frame with a broken checksum
// (OriginWALUnverified) yields its rows with the stale method.
func TestBrokenWALFrameYieldsStaleRows(t *testing.T) {
	s := newRowScen(t)
	s1, cell1, _ := s.setT(2, 1)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s.v0.Page(2), 2)
	w.Frame(2, s1.Page(2), 2)
	wb := w.Bytes()
	w.PatchFrame(2, 16, wb[walOff(2)-24+16]^0xff) // the stored checksum of slot 2
	f := files{db: walMode(s.db0), wal: w.Bytes()}
	_, h := openAll(t, f.db, f.wal, nil)
	rows, _ := collectRows(t, h)
	found := false
	for _, r := range rows {
		if r.WAL != nil && r.WAL.Frame == 2 {
			found = true
			if r.Method != sqlitefile.MethodWALStale || r.WAL.State != sqlitefile.FrameBroken {
				t.Errorf("row %+v wal %+v", r, r.WAL)
			}
			if *r.Rowid == 2 {
				assertCell(t, f, r, cell1)
			}
		}
	}
	if !found {
		t.Fatalf("no row of the broken frame in %v", methods(rows))
	}
}

// TestJournalRecordOfAnotherPageSizeYieldsNoRows: a record of 512 bytes in a
// journal of a 1024-byte database is not a page of this database.
func TestJournalRecordOfAnotherPageSizeYieldsNoRows(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(a, b)")
	for id := int64(1); id <= 3; id++ {
		tb.Insert(id, id, "x")
	}
	db := withCount(b.Bytes(), b.Snapshot().Pages())
	small := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	st := small.CreateTable("t", "create table t(a, b)")
	st.Insert(1, int64(5), "y")
	j := small.NewJournal(512, 0x1111, 2)
	j.Record(2, small.Snapshot().Page(2))
	j.SuperJournal("gone-super-journal")
	_, h := openAll(t, db, nil, j.Bytes())
	// The journal is hot and not applied (page-size-mismatch): Live refuses, so
	// no row of its records is ever delivered; the page-size guard in the row
	// pass is defence in depth behind that refusal.
	var n int
	_, err := h.Rows(context.Background(), func(sqlitefile.RecoveredRow) bool { n++; return true })
	if !errors.Is(err, sqlitefile.ErrLiveUnavailable) || n != 0 {
		t.Errorf("%d rows, error %v", n, err)
	}
}

// TestCellPointerArrayInTheReservedRegionIsRejected: a pointer array that ends
// inside the reserved bytes is an unreadable array; its cells are counted.
func TestCellPointerArrayInTheReservedRegionIsRejected(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps, Reserved: 8})
	tb := b.CreateTable("t", "create table t(a, b)")
	tb.Insert(1, int64(1), "x")
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	page := v0.Page(2)
	binary.BigEndian.PutUint16(page[3:], 505) // the array ends at 8+1010 = 1018, past the usable 1016
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, page, 2)
	w.Frame(2, v0.Page(2), 2)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	for _, r := range rows {
		if r.WAL != nil {
			t.Errorf("a row from the damaged frame: %+v", r)
		}
	}
	if st.CellsRejected != 505 {
		t.Errorf("rejected %d, want 505", st.CellsRejected)
	}
}

// TestRowsCancellationIsPolledPeriodically: a cancellation that arrives while
// rows are delivered ends the pass within about a thousand cells.
func TestRowsCancellationIsPolledPeriodically(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 65536})
	tb := b.CreateTable("t", "create table t(a)")
	for id := int64(1); id <= 3000; id++ {
		tb.Insert(id, id)
	}
	v0 := b.Snapshot()
	for id := int64(1); id <= 3000; id++ {
		tb.Update(id, -id)
	}
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	for _, pg := range tb.Leaves() {
		w.Frame(pg, v0.Page(pg), v0.Pages())
	}
	_, h := openAll(t, db, w.Bytes(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered := 0
	_, err := h.Rows(ctx, func(sqlitefile.RecoveredRow) bool {
		delivered++
		if delivered == 1 {
			cancel()
		}
		return true
	})
	if err == nil || delivered > 600 {
		t.Errorf("a cancellation was not honoured promptly: err %v after %d rows", err, delivered)
	}
}

// TestFitOnlyTableLeafNeverNamesWithoutRowidTable: a dropped rowid table's leaf
// is not named after a live WITHOUT ROWID table of the same shape.
func TestFitOnlyTableLeafNeverNamesWithoutRowidTable(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k primary key, v) without rowid", 1)
	wr.Insert(0, "k1", "v1")
	pad := b.CreateTable("pad", "create table pad(z)")
	pad.Insert(1, "x")
	gone := b.CreateTable("gone", "create table gone(a, b)")
	gone.Insert(7, "ga", "gb")
	b.DropTable("pad")
	b.DropTable("gone")
	_, h := openAll(t, b.Bytes(), nil, nil)
	rows, _ := collectRows(t, h)
	n := 0
	for _, r := range freelistRows(rows) {
		n++
		if r.Table != "" || r.TableBasis != sqlitefile.BasisNone {
			t.Errorf("a rowid table leaf was named: %+v", r)
		}
	}
	if n != 1 {
		t.Fatalf("%d freelist rows in %v", n, methods(rows))
	}
}

// TestSchemaTableRowsAreComparedWithLiveSchemaRows (M-3): an old image of page 1
// has the schema table as its owner; its rows are compared with the live schema
// rows by rowid like any rowid table.
func TestSchemaTableRowsAreComparedWithLiveSchemaRows(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	b.CreateTable("t", "create table t(a, b)")
	old := b.Snapshot()
	b.DropTable("t")
	b.CreateTable("u", "create table u(a)")
	live := b.Snapshot()
	db := walMode(withCount(b.Bytes(), live.Pages()))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(1, old.Page(1), live.Pages())
	w.Frame(1, live.Page(1), live.Pages())
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	var got []sqlitefile.RecoveredRow
	for _, r := range rows {
		if r.WAL != nil && r.Loc.Page == 1 {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d schema rows in %v", len(got), methods(rows))
	}
	r := got[0]
	if r.Table != "sqlite_schema" || r.TableBasis != sqlitefile.BasisSchema || (r.Relation != sqlitefile.RelSupersededVersion && r.Relation != sqlitefile.RelAbsentFromLive) {
		t.Errorf("schema row %+v", r)
	}
}

// TestFitBuildStepsAreExact (M-1): the build charges one step per index column
// and one per column of each table whose column map it builds.
func TestFitBuildStepsAreExact(t *testing.T) {
	I := sqlitefile.AffInteger
	wide := make([]sqlitefile.Column, 200)
	for i := range wide {
		wide[i] = fcol(fmt.Sprintf("c%d", i), I)
	}
	small := make([]sqlitefile.Column, 30)
	for i := range small {
		small[i] = fcol(fmt.Sprintf("d%d", i), I)
	}
	two := []sqlitefile.IndexColumn{{Name: "c0"}, {Name: "c1"}}
	objs := []sqlitefile.SchemaObject{
		tableObj("a", tableDef(false, -1, wide...)),
		tableObj("b", tableDef(false, -1, small...)),
		indexObj("i1", "a", two...), indexObj("i2", "a", two...), indexObj("i3", "a", two...),
		indexObj("j1", "b", sqlitefile.IndexColumn{Name: "d0"}),
	}
	s := schemaOf(objs...)
	want := int64(3*2 + 200 + 1 + 30)
	if n := sqlitefile.FitBuildSteps(s); n != want {
		t.Errorf("the build charged %d steps, want %d", n, want)
	}
}

// TestShortRecordsAreCountedApart (M-5): a record that declares more bytes than
// its cell holds is counted in CellsShort as well as CellsRejected, so it can be
// told from a hostile pointer or a bad record header.
func TestShortRecordsAreCountedApart(t *testing.T) {
	cells := [][]byte{
		intCell(1, 5),
		rawCell(4, 0x02, 0x1b, 'a'),  // text of 7 bytes declared, 1 present
		rawCell(5, 0x09, 0x01, 0x05), // header length 9 in a payload of 3: a bad header, not a short record
	}
	page := hostile512(cells, nil)
	db, wal := hostileWAL(t, page)
	_, h := openAll(t, db, wal, nil)
	_, st := collectRows(t, h)
	if st.CellsRejected != 2 || st.CellsShort != 1 {
		t.Errorf("rejected %d short %d, want 2 and 1", st.CellsRejected, st.CellsShort)
	}
}

// checkRowLocs re-reads the bytes at Loc of every row: they are a cell with the
// row's rowid and, when it does not spill, its values; a spilling cell keeps the
// bytes of its local part.
func checkRowLocs(t *testing.T, f files, rows []sqlitefile.RecoveredRow, usable int) map[sqlitefile.FileKind]int {
	t.Helper()
	seen := map[sqlitefile.FileKind]int{}
	for _, r := range rows {
		c := parseAt(t, f, r.Loc, usable)
		seen[r.Loc.File]++
		if r.Rowid == nil || c.Rowid != *r.Rowid {
			t.Errorf("row %+v: cell rowid %d", r.Loc, c.Rowid)
		}
		if c.OverflowHead != 0 {
			if int64(len(c.Local)) >= c.PayloadLen {
				t.Errorf("row %+v: a spilling cell holds its whole payload locally", r.Loc)
			}
			continue
		}
		rec, err := sqlitefile.DecodeRecord(c.Local, sqlitefile.EncUTF8, sqlitefile.Limits{})
		if err != nil || len(rec.Values) != len(r.Values) {
			t.Errorf("row %+v: record %v %v", r.Loc, err, rec)
			continue
		}
		for i, v := range rec.Values {
			if v.Kind != r.Values[i].Kind || v.Int != r.Values[i].Int || !bytes.Equal(v.Bytes, r.Values[i].Bytes) {
				t.Errorf("row %+v: value %d %+v, cell holds %+v", r.Loc, i, r.Values[i], v)
			}
		}
	}
	return seen
}

// TestRowLocationsEveryOrigin (M-2): the bytes at Loc are the cell for rows of
// every origin kind: stale and uncommitted WAL frames, a rolled-back database
// image, a cold journal, a persist journal and a freelist leaf.
func TestRowLocationsEveryOrigin(t *testing.T) {
	t.Run("wal generations and uncommitted", func(t *testing.T) {
		s := newRowScen(t)
		s1, _, _ := s.setT(2, 1)
		s2, _, _ := s.setT(2, 2)
		s3, _, _ := s.setT(2, 3)
		w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(2, s1.Page(2), 2)
		w.Frame(2, s2.Page(2), 0)
		w.Frame(2, s3.Page(2), 2)
		w.Reset(0x2000, 0x2001)
		w.Frame(2, s3.Page(2), 2)
		f := files{db: walMode(s.db0), wal: w.Bytes()}
		_, h := openAll(t, f.db, f.wal, nil)
		rows, _ := collectRows(t, h)
		seen := checkRowLocs(t, f, rows, hps)
		if seen[sqlitefile.FileWAL] == 0 {
			t.Errorf("no WAL row in %v", methods(rows))
		}
	})
	t.Run("rolled back database image", func(t *testing.T) {
		r := newRollbackScen(t)
		r.j.Record(2, r.before2)
		f := files{db: r.db, journal: r.j.Bytes()}
		_, h := openAll(t, f.db, nil, f.journal)
		rows, _ := collectRows(t, h)
		if seen := checkRowLocs(t, f, rows, hps); seen[sqlitefile.FileDB] == 0 {
			t.Errorf("no database row in %v", methods(rows))
		}
	})
	t.Run("cold and persist journals", func(t *testing.T) {
		for _, persist := range []bool{false, true} {
			s := newRowScen(t)
			s.setT(2, 1)
			db := withCount(s.b.Bytes(), 3)
			j := s.b.NewJournal(512, 0x1234abcd, 3)
			j.Record(2, s.v0.Page(2))
			if persist {
				j.Zero()
			} else {
				j.SuperJournal("gone-super-journal")
			}
			f := files{db: db, journal: j.Bytes()}
			_, h := openAll(t, f.db, nil, f.journal)
			rows, _ := collectRows(t, h)
			if seen := checkRowLocs(t, f, rows, hps); seen[sqlitefile.FileJournal] == 0 {
				t.Errorf("persist %v: no journal row in %v", persist, methods(rows))
			}
		}
	})
	t.Run("freelist leaf", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		pad := b.CreateTable("pad", "create table pad(z)")
		pad.Insert(1, "x")
		gone := b.CreateTable("gone", "create table gone(a, b)")
		for id := int64(10); id <= 12; id++ {
			gone.Insert(id, id, "gone-text")
		}
		b.DropTable("pad")
		b.DropTable("gone")
		f := files{db: b.Bytes()}
		_, h := openAll(t, f.db, nil, nil)
		rows, _ := collectRows(t, h)
		if seen := checkRowLocs(t, f, rows, hps); seen[sqlitefile.FileDB] != 3 {
			t.Errorf("%d freelist rows in %v", seen[sqlitefile.FileDB], methods(rows))
		}
	})
	t.Run("spilling cell", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		tb := b.CreateTable("t", "create table t(a, b)")
		tb.Insert(1, int64(1), longText(3000, 1))
		v0 := b.Snapshot()
		tb.Update(1, int64(2), longText(3000, 2))
		v1 := b.Snapshot()
		pages := v1.Pages()
		f := files{db: walMode(withCount(b.Bytes(), pages))}
		w := b.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(2, v0.Page(2), pages)
		w.Frame(2, v1.Page(2), pages)
		f.wal = w.Bytes()
		_, h := openAll(t, f.db, f.wal, nil)
		rows, _ := collectRows(t, h)
		if seen := checkRowLocs(t, f, rows, hps); seen[sqlitefile.FileWAL] == 0 {
			t.Errorf("no WAL row in %v", methods(rows))
		}
	})
}
