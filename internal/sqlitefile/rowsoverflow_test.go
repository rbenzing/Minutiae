package sqlitefile_test

// History rows (plan 3I, Task 12): overflow chains followed in the writer's
// state at the time of the image, and the clipping of large recovered values.

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// bigScen is a database whose table big(a, b) holds one row with a 3500 byte
// text, which spills over three overflow pages of the leaf (page 2).
type bigScen struct {
	b   *sqlitetest.Builder
	tb  *sqlitetest.Table
	ov  []uint32 // the chain of the current version
	v0  *sqlitetest.Image
	db0 []byte // rollback mode: the database at version 0
	n0  uint32 // its size in pages
}

func newBigScen(t testing.TB, pageSize int) *bigScen {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: pageSize})
	s := &bigScen{b: b}
	s.tb = b.CreateTable("big", "create table big(a, b)")
	s.tb.Insert(1, int64(100), longText(3500, 1))
	s.ov = s.tb.Overflow(1)
	if len(s.ov) != 3 {
		t.Fatalf("overflow pages %v", s.ov)
	}
	s.n0 = uint32(len(b.Bytes()) / pageSize)
	s.v0 = b.Snapshot()
	s.db0 = withCount(b.Bytes(), s.n0)
	return s
}

// set rewrites the row (the builder lays the chain out on fresh pages) and
// returns the page image and the chain of the new version.
func (s *bigScen) set(a int64, seed byte) (*sqlitetest.Image, []uint32) {
	s.tb.Update(1, a, longText(3500, seed))
	s.ov = s.tb.Overflow(1)
	return s.b.Snapshot(), s.ov
}

// walScen writes into a WAL, in one generation: the chain of version 1 (slots
// 1-3) and its leaf (slot 4, committed); junk over the chain of version 0
// (5-7) and over the chain of version 1 (8-10); the chain of version 2 (11-13)
// and its leaf (14, committed). Version 1's leaf is therefore superseded and its
// overflow pages were rewritten later in the generation.
func walScen(t testing.TB) (s *bigScen, ov0, ov1 []uint32, f files) {
	s = newBigScen(t, hps)
	ov0 = s.ov
	v1, ov1 := s.set(2, 2)
	v2, ov2 := s.set(3, 3)
	n := uint32(len(s.b.Bytes()) / hps)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	for _, p := range ov1 {
		w.Frame(p, v1.Page(p), 0)
	}
	w.Frame(2, v1.Page(2), n)
	for _, p := range ov0 {
		w.Frame(p, patPage(byte(p)), 0)
	}
	for _, p := range ov1 {
		w.Frame(p, patPage(byte(p)+1), 0)
	}
	for _, p := range ov2 {
		w.Frame(p, v2.Page(p), 0)
	}
	w.Frame(2, v2.Page(2), n)
	return s, ov0, ov1, files{db: walMode(s.db0), wal: w.Bytes()}
}

func rowOfMethod(t testing.TB, rows []sqlitefile.RecoveredRow, method string, frame uint32) sqlitefile.RecoveredRow {
	t.Helper()
	for _, r := range rows {
		if r.Method == method && (frame == 0 || (r.WAL != nil && r.WAL.Frame == frame)) {
			return r
		}
	}
	t.Fatalf("no %s row (frame %d) in %v", method, frame, methods(rows))
	return sqlitefile.RecoveredRow{}
}

// TestHistoryOverflowAsOfSnapshot: a long text in a superseded frame whose
// overflow pages were later rewritten in the same generation is reassembled
// from the frame's own state; the database image under the log cannot follow
// pages the log overlays; a stale-generation cell and a freelist leaf whose
// chain pages belong to other states are truncated, with their local values
// kept.
func TestHistoryOverflowAsOfSnapshot(t *testing.T) {
	t.Run("superseded frame", func(t *testing.T) {
		_, ov0, _, f := walScen(t)
		_, h := openAll(t, f.db, f.wal, nil)
		rows, _ := collectRows(t, h)
		r := rowOfMethod(t, rows, sqlitefile.MethodWALPrior, 4)
		if r.Truncated || r.OverflowHead != 0 || intOf(t, r.Values[0]) != 2 || r.Values[1].Omitted || txtOf(t, r.Values[1]) != longText(3500, 2) {
			t.Errorf("row truncated %v head %d a %d", r.Truncated, r.OverflowHead, r.Values[0].Int)
		}
		// the database image: its overflow pages are overlaid by the log
		var db *sqlitefile.RecoveredRow
		for i := range rows {
			if rows[i].Loc.File == sqlitefile.FileDB {
				db = &rows[i]
			}
		}
		if db == nil || !db.Truncated || db.OverflowHead != ov0[0] || intOf(t, db.Values[0]) != 100 || !db.Values[1].Omitted || db.Values[1].Len != 3500 {
			t.Fatalf("database row %+v", db)
		}
		if !hasCode(h.Warnings(), sqlitefile.WarnSnapshotUnavailable) {
			t.Errorf("no snapshot-unavailable warning: %v", h.Warnings())
		}
	})
	t.Run("stale generation", func(t *testing.T) {
		s := newBigScen(t, hps)
		v1, ov1 := s.set(2, 2)
		v2, _ := s.set(3, 3)
		n := uint32(len(s.b.Bytes()) / hps)
		w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(2, v1.Page(2), n)
		w.Frame(2, v1.Page(2), n)
		w.Reset(0x2000, 0x2001)
		w.Frame(2, v2.Page(2), n)
		_, h := openAll(t, walMode(s.db0), w.Bytes(), nil)
		rows, _ := collectRows(t, h)
		r := rowOfMethod(t, rows, sqlitefile.MethodWALStale, 0)
		if !r.Truncated || r.OverflowHead != ov1[0] || intOf(t, r.Values[0]) != 2 || !r.Values[1].Omitted || r.Values[1].Len != 3500 {
			t.Errorf("stale row %+v", r)
		}
		if !hasCode(h.Warnings(), sqlitefile.WarnSnapshotUnavailable) {
			t.Errorf("no snapshot-unavailable warning: %v", h.Warnings())
		}
	})
	t.Run("freelist leaf whose chain page was reused", func(t *testing.T) {
		rs := newRowScen(t)
		rs.b.SetFreelist([][]uint32{{4, 5}})
		db := rs.b.Bytes()
		donor := newBigScen(t, hps)
		page := donor.v0.Page(2)
		cell, _, off := donor.tb.CellBytes(1)
		binary.BigEndian.PutUint32(page[off+len(cell)-4:], 3) // the chain starts at page 3: a live page here
		copy(db[4*hps:], page)
		_, h := openAll(t, db, nil, nil)
		rows, _ := collectRows(t, h)
		r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
		if !r.Truncated || r.OverflowHead != 3 || !r.Values[1].Omitted || r.Values[1].Len != 3500 {
			t.Errorf("row %+v", r)
		}
		if !hasCode(h.Warnings(), sqlitefile.WarnSnapshotUnavailable) {
			t.Errorf("no snapshot-unavailable warning: %v", h.Warnings())
		}
	})
}

// TestHistoryOverflowChainCycle: a chain in a stale image that points at itself
// is bounded and truncated.
func TestHistoryOverflowChainCycle(t *testing.T) {
	rs := newRowScen(t)
	rs.b.SetFreelist([][]uint32{{4, 5, 6}})
	db := rs.b.Bytes()
	donor := newBigScen(t, hps)
	page := donor.v0.Page(2)
	cell, _, off := donor.tb.CellBytes(1)
	binary.BigEndian.PutUint32(page[off+len(cell)-4:], 6)
	copy(db[4*hps:], page)
	loop := patPage(0x33)
	binary.BigEndian.PutUint32(loop, 6) // page 6 points at itself
	copy(db[5*hps:], loop)
	_, h := openAll(t, db, nil, nil)
	rows, st := collectRows(t, h)
	r := rowOfMethod(t, rows, sqlitefile.MethodFreelist, 0)
	if !r.Truncated || r.OverflowHead != 6 || !r.Values[1].Omitted {
		t.Errorf("row %+v", r)
	}
	if st.OverflowPagesFollowed != 1 {
		t.Errorf("followed %d pages of a one-page cycle", st.OverflowPagesFollowed)
	}
}

// aBodyOffset is the offset in the page of the first body byte of the cell at
// off: the value of column a, an int8.
func aBodyOffset(cell []byte, off int) int {
	_, n1 := sqlitefile.GetVarint(cell)
	_, n2 := sqlitefile.GetVarint(cell[n1:])
	hl, _ := sqlitefile.GetVarint(cell[n1+n2:])
	return off + n1 + n2 + int(hl)
}

// withA returns a copy of the leaf with column a of its only row set to a.
func withA(page []byte, bodyOff int, a byte) []byte {
	p := bytes.Clone(page)
	p[bodyOff] = a
	return p
}

// capScen: four before-images of the leaf of a big row (a = 2..5) in a journal
// that is not applied; each one's chain is the same three database pages.
func capScen(t testing.TB) (db, journal []byte, head uint32) {
	s := newBigScen(t, hps)
	cell, _, off := s.tb.CellBytes(1)
	body := aBodyOffset(cell, off)
	j := s.b.NewJournal(512, 0x7777aaaa, s.n0)
	for a := byte(2); a <= 5; a++ {
		j.Record(2, withA(s.v0.Page(2), body, a))
	}
	j.SuperJournal("absent-super-journal")
	return s.db0, j.Bytes(), s.ov[0]
}

// TestHistoryOverflowPagesCapped: MaxHistoryOverflowPages bounds the pages
// followed across many rows that share the same real pages.
func TestHistoryOverflowPagesCapped(t *testing.T) {
	db, journal, head := capScen(t)
	_, h := openAll(t, db, nil, journal)
	rows, st := collectRows(t, h)
	if len(rows) != 4 || st.OverflowPagesFollowed != 12 || len(st.LimitsHit) != 0 {
		t.Fatalf("uncapped: %d rows, stats %+v", len(rows), st)
	}
	for _, r := range rows {
		if r.Truncated {
			t.Errorf("row %+v truncated", r)
		}
	}

	d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxHistoryOverflowPages: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AttachJournal(bytes.NewReader(journal), int64(len(journal))); err != nil {
		t.Fatal(err)
	}
	h2 := d.History()
	t.Cleanup(h2.Release)
	rows, st = collectRows(t, h2)
	if len(rows) != 4 || st.OverflowPagesFollowed != 5 || !slices.Contains(st.LimitsHit, "MaxHistoryOverflowPages") {
		t.Fatalf("capped: %d rows, stats %+v", len(rows), st)
	}
	cut := 0
	for _, r := range rows {
		if r.Truncated {
			cut++
			if r.OverflowHead != head {
				t.Errorf("truncated row without its chain head: %+v", r)
			}
		}
	}
	if cut != 3 {
		t.Errorf("%d truncated rows, want 3", cut)
	}
	if !hasCode(h2.Warnings(), sqlitefile.WarnLimitReached) {
		t.Errorf("no limit-reached warning: %v", h2.Warnings())
	}
}

// TestOverflowProvenanceOfRecoveredValue: Loc.Overflow names, for every overflow
// page that supplied bytes, the page and where its bytes really came from.
func TestOverflowProvenanceOfRecoveredValue(t *testing.T) {
	_, _, ov1, f := walScen(t)
	_, h := openAll(t, f.db, f.wal, nil)
	rows, _ := collectRows(t, h)
	r := rowOfMethod(t, rows, sqlitefile.MethodWALPrior, 4)
	if r.Loc.OverflowTotal != 3 || len(r.Loc.Overflow) != 3 || !r.Loc.OverflowMixed {
		t.Fatalf("Loc %+v", r.Loc)
	}
	for i, p := range r.Loc.Overflow {
		slot := uint32(i + 1)
		want := sqlitefile.PageLoc{File: sqlitefile.FileWAL, Offset: walOff(int(slot)), Frame: slot}
		if p.Page != ov1[i] || p.At != want {
			t.Errorf("overflow %d: %+v, want page %d at %+v", i, p, ov1[i], want)
		}
	}
	// the bytes of the first overflow page are where At says: the chain page of
	// version 1, not the junk written over it later
	first := r.Loc.Overflow[0].At
	if bytes.Equal(f.wal[first.Offset:first.Offset+int64(hps)], patPage(byte(ov1[0])+1)) {
		t.Errorf("the overflow page at %+v is the later junk", first)
	}
	if binary.BigEndian.Uint32(f.wal[first.Offset:]) != ov1[1] {
		t.Errorf("the page at %+v does not continue the chain", first)
	}
}

// TestHistoryRowsClipLargeValues: a 3 MiB text in an older image keeps its first
// MaxRecoveredValueBytes (1 MiB), marked Clipped, with the true length.
func TestHistoryRowsClipLargeValues(t *testing.T) {
	const ps = 4096
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tb := b.CreateTable("big", "create table big(a, b)")
	text := longText(3<<20, 9)
	tb.Insert(1, int64(100), text)
	n := uint32(len(b.Bytes()) / ps)
	db := withCount(b.Bytes(), n)
	cell, _, off := tb.CellBytes(1)
	j := b.NewJournal(512, 0x5555aaaa, n)
	j.Record(2, withA(b.Snapshot().Page(2), aBodyOffset(cell, off), 2))
	j.SuperJournal("absent-super-journal")
	_, h := openAll(t, db, nil, j.Bytes())
	rows, st := collectRows(t, h)
	if len(rows) != 1 {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
	v := rows[0].Values[1]
	if !v.Clipped || v.Omitted || len(v.Bytes) != 1<<20 || v.Len != 3<<20 || !bytes.Equal(v.Bytes, []byte(text[:1<<20])) {
		t.Errorf("value clipped %v omitted %v len(bytes) %d Len %d", v.Clipped, v.Omitted, len(v.Bytes), v.Len)
	}
	if rows[0].Truncated || intOf(t, rows[0].Values[0]) != 2 || rows[0].Relation != sqlitefile.RelSupersededVersion {
		t.Errorf("row %+v", rows[0])
	}
}
