package sqlitefile_test

// History rows (plan 3I, Task 12): hostile cells, caps, determinism, the cost
// of the live lookups and the location model.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// hostile512 builds a 512-byte table-leaf page from raw cells, placed from the
// end of the page downward, with the pointers in the given order. ptr, when not
// nil, overrides the pointer of cell i.
func hostile512(cells [][]byte, ptr map[int]uint16) []byte {
	p := make([]byte, sps)
	p[0] = 0x0d
	binary.BigEndian.PutUint16(p[3:], uint16(len(cells)))
	off := sps
	for i, c := range cells {
		off -= len(c)
		copy(p[off:], c)
		binary.BigEndian.PutUint16(p[8+2*i:], uint16(off))
	}
	binary.BigEndian.PutUint16(p[5:], uint16(off))
	for i, v := range ptr {
		binary.BigEndian.PutUint16(p[8+2*i:], v)
	}
	return p
}

// rawCell is [payload length][rowid][payload].
func rawCell(rowid byte, payload ...byte) []byte {
	return append([]byte{byte(len(payload)), rowid}, payload...)
}

// intCell is a valid cell of table t(a): one int8 value.
func intCell(rowid, v byte) []byte { return rawCell(rowid, 0x02, 0x01, v) }

// hostileWAL builds a 512-byte database whose table t(a) is live on page 2
// (rows 1 and 2 hold 7 and 8) and a WAL whose older committed frame is the
// given page 2.
func hostileWAL(t testing.TB, old []byte) (db, wal []byte) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: sps})
	tb := b.CreateTable("t", "create table t(a)")
	tb.Insert(1, int64(7))
	tb.Insert(2, int64(8))
	live := b.Snapshot()
	db = walMode(withCount(b.Bytes(), live.Pages()))
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, old, live.Pages())
	w.Frame(2, live.Page(2), live.Pages())
	return db, w.Bytes()
}

// TestHistoryRowsHostileCells: a pointer into the header, a cell that overruns
// the page, a record shorter than it declares and a bad record header are
// counted in CellsRejected and give no row; a reserved serial type is read as
// NULL as the engine reads it (the cell is kept, with a warning); nothing
// panics.
func TestHistoryRowsHostileCells(t *testing.T) {
	cells := [][]byte{
		intCell(1, 5),
		intCell(2, 6),
		intCell(3, 1),                      // its pointer is moved into the header
		rawCell(4, 0x02, 0x1b, 'a'),        // text of 7 bytes declared, 1 present
		rawCell(5, 0x09, 0x01, 0x05),       // header length 9 in a payload of 3
		rawCell(6, 0x02, 0x0a),             // reserved serial type 10
		{20, 7, 0x02, 0x01, 0x01, 0x02, 3}, // the last cell of the page: the pointer is moved to its last 3 bytes
	}
	page := hostile512(cells, map[int]uint16{2: 3, 6: sps - 3})
	db, wal := hostileWAL(t, page)
	_, h := openAll(t, db, wal, nil)
	rows, st := collectRows(t, h)
	if st.CellsRejected != 4 { // the pointer, the overrun, the short record, the bad header
		t.Errorf("rejected %d, want 4: %+v", st.CellsRejected, st)
	}
	byRowid := map[int64]sqlitefile.RecoveredRow{}
	for _, r := range rows {
		byRowid[*r.Rowid] = r
	}
	if len(rows) != 3 || byRowid[1].Values[0].Int != 5 || byRowid[2].Values[0].Int != 6 {
		t.Fatalf("rows %d %+v", len(rows), byRowid)
	}
	if r := byRowid[6]; len(r.Values) != 1 || r.Values[0].Kind != sqlitefile.KindNull {
		t.Errorf("reserved serial row %+v", r)
	}
	if !hasCode(h.Warnings(), sqlitefile.WarnRecordReservedSerial) {
		t.Errorf("no record-reserved-serial warning: %v", h.Warnings())
	}

	t.Run("pointer array past the page", func(t *testing.T) {
		p := hostile512([][]byte{intCell(1, 5)}, nil)
		binary.BigEndian.PutUint16(p[3:], 300) // 300 pointers do not fit
		db, wal := hostileWAL(t, p)
		_, h := openAll(t, db, wal, nil)
		rows, st := collectRows(t, h)
		if len(rows) != 0 || st.CellsRejected != 300 {
			t.Errorf("%d rows, rejected %d", len(rows), st.CellsRejected)
		}
	})
}

// TestHistoryRowCaps: MaxHistoryRows stops the pass; MaxDiffRowsTotal bounds the
// digest sets of all WITHOUT ROWID tables together.
func TestHistoryRowCaps(t *testing.T) {
	t.Run("MaxHistoryRows", func(t *testing.T) {
		s := newRowScen(t)
		for id := int64(1); id <= 3; id++ {
			s.setT(id, 1)
		}
		v1 := s.b.Snapshot()
		for id := int64(1); id <= 3; id++ {
			s.setT(id, 2)
		}
		v2 := s.b.Snapshot()
		w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
		w.Frame(2, v1.Page(2), 3)
		w.Frame(2, v2.Page(2), 3)
		db, wal := walMode(s.db0), w.Bytes()
		_, hAll := openAll(t, db, wal, nil)
		all, _ := collectRows(t, hAll)
		if len(all) != 6 {
			t.Fatalf("the scenario yields %d rows, want 6", len(all))
		}
		d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxHistoryRows: 3}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.AttachWAL(bytes.NewReader(wal), int64(len(wal))); err != nil {
			t.Fatal(err)
		}
		h := d.History()
		t.Cleanup(h.Release)
		rows, st := collectRows(t, h)
		if len(rows) != 3 || !slices.Contains(st.LimitsHit, "MaxHistoryRows") || !hasCode(h.Warnings(), sqlitefile.WarnLimitReached) {
			t.Errorf("%d rows, stats %+v", len(rows), st)
		}
	})
	t.Run("MaxDiffRowsTotal", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
		var tabs []*sqlitetest.Table
		for _, n := range []string{"w1", "w2"} {
			wr := b.CreateTableWithoutRowid(n, fmt.Sprintf("create table %s(k text primary key, v) without rowid", n), 1)
			for id := int64(1); id <= 2; id++ {
				wr.Insert(id, fmt.Sprintf("key%d", id), "value-v0")
			}
			tabs = append(tabs, wr)
		}
		v0 := b.Snapshot()
		db := walMode(withCount(b.Bytes(), v0.Pages()))
		for _, wr := range tabs {
			wr.Update(1, "key1", "value-v1")
		}
		w := b.NewWAL(false, 0x1000, 0x1001, 0)
		b.CommitTo(w, v0)
		d, err := sqlitefile.Open(bytes.NewReader(db), int64(len(db)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxDiffRowsTotal: 3}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.AttachWAL(bytes.NewReader(w.Bytes()), int64(len(w.Bytes()))); err != nil {
			t.Fatal(err)
		}
		h := d.History()
		t.Cleanup(h.Release)
		rows, st := collectRows(t, h)
		if !slices.Contains(st.LimitsHit, "MaxDiffRowsTotal") || st.Unknown == 0 {
			t.Fatalf("stats %+v", st)
		}
		for _, r := range rows {
			if r.Table == "w1" && r.Relation == sqlitefile.RelUnknown {
				t.Errorf("w1 fits the total cap and must be compared: %+v", r)
			}
		}
		if !hasCode(h.Warnings(), sqlitefile.WarnLimitReached) {
			t.Errorf("no limit-reached warning")
		}
	})
}

func rowsFingerprint(rows []sqlitefile.RecoveredRow, st sqlitefile.RowStats) [32]byte {
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%s|%s|%s|%s|%v|%s|%+v|%v|%+v|%+v|%d|%v|%v\n", r.Method, r.Table, r.Index, r.TableBasis, r.Rowid != nil, r.Relation,
			r.Loc, r.WAL, r.WAL, r.Journal, r.OverflowHead, r.Truncated, r.Notes)
		if r.Rowid != nil {
			fmt.Fprintf(h, "rowid %d\n", *r.Rowid)
		}
		for _, v := range r.Values {
			fmt.Fprintf(h, "%d %d %v %x %d %v %v\n", v.Kind, v.Int, v.Float, v.Bytes, v.Len, v.Omitted, v.Clipped)
		}
	}
	for _, m := range []string{sqlitefile.MethodWALPrior, sqlitefile.MethodWALUncommitted, sqlitefile.MethodWALStale, sqlitefile.MethodFreelist, sqlitefile.MethodJournalBefore, sqlitefile.MethodJournalRolledBack} {
		fmt.Fprintf(h, "%s=%d\n", m, st.RowsByMethod[m])
	}
	fmt.Fprintf(h, "%d %d %d %d %d %v", st.DuplicateOfLive, st.CellsRejected, st.InteriorSkipped, st.Unknown, st.OverflowPagesFollowed, st.LimitsHit)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// TestHistoryRowsDeterministic: 50 passes over fresh instances give one hash.
func TestHistoryRowsDeterministic(t *testing.T) {
	m := newHistMaster(t)
	var first [32]byte
	for i := range 50 {
		d := m.open(t, sqlitefile.Options{}, i%2 == 0)
		rows, st := collectRows(t, d.History())
		fp := rowsFingerprint(rows, st)
		if i == 0 {
			first = fp
			if len(rows) == 0 {
				t.Fatal("the master scenario yields no rows")
			}
		} else if fp != first {
			t.Fatalf("pass %d differs", i)
		}
	}
}

// TestHistoryLookupsHitCache: relating many rows of one page to the live state
// costs a few page reads; the cache serves the rest.
func TestHistoryLookupsHitCache(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("t", "create table t(a, b)")
	const n = 400
	for id := int64(1); id <= n; id++ {
		tb.Insert(id, id, fmt.Sprintf("row-%04d-v0-padding", id))
	}
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	for id := int64(1); id <= n; id++ {
		tb.Update(id, id+1000, fmt.Sprintf("row-%04d-v1-padding", id))
	}
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	if len(rows) != n {
		t.Fatalf("%d rows, want %d", len(rows), n)
	}
	st := sqlitefile.HistLiveStats(h)
	if st.CacheHits < 10*st.PageReads || st.PageReads > int64(v0.Pages())+4 {
		t.Errorf("live stats %+v for %d lookups over %d pages", st, n, v0.Pages())
	}
	if st.CacheHits < n { // at least one cached page access per lookup
		t.Errorf("cache hits %d for %d lookups", st.CacheHits, n)
	}
}

// parseAt reads the cell at the row's Loc as the format lays it out.
func parseAt(t testing.TB, f files, loc sqlitefile.Loc, usable int) sqlitefile.Cell {
	t.Helper()
	b := f.of(loc.File)
	if loc.Offset < 0 || loc.Offset+loc.Length > int64(len(b)) || loc.Length <= 0 {
		t.Fatalf("Loc %+v outside the %s (%d bytes)", loc, loc.File, len(b))
	}
	buf := make([]byte, usable)
	copy(buf, b[loc.Offset:loc.Offset+loc.Length])
	c, err := sqlitefile.ParseCell(buf, usable, sqlitefile.PageHeader{Type: sqlitefile.PageTableLeaf}, 0)
	if err != nil {
		t.Fatalf("the bytes at %+v do not parse as a cell: %v", loc, err)
	}
	if int64(c.Length) != loc.Length {
		t.Fatalf("the cell at %+v is %d bytes long", loc, c.Length)
	}
	return c
}

// TestRecoveredRowLocationReproducesCell: for every recovered row of the master
// scenario, the bytes at [Loc.Offset, Loc.Offset+Loc.Length) of the named file
// are a cell with the row's rowid and, when it spills nowhere, its values.
func TestRecoveredRowLocationReproducesCell(t *testing.T) {
	m := newHistMaster(t)
	d := m.open(t, sqlitefile.Options{}, true)
	rows, _ := collectRows(t, d.History())
	f := files{db: m.db, wal: m.wal, journal: m.journal}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		c := parseAt(t, f, r.Loc, hps)
		if r.Rowid == nil || c.Rowid != *r.Rowid {
			t.Errorf("row %+v: cell rowid %d", r.Loc, c.Rowid)
		}
		if c.OverflowHead == 0 {
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
	}
	for _, sc := range []struct {
		name string
		f    files
		o    sqlitefile.FileKind
	}{{"wal", f, sqlitefile.FileWAL}, {"journal", f, sqlitefile.FileJournal}} {
		if !slices.ContainsFunc(rows, func(r sqlitefile.RecoveredRow) bool { return r.Loc.File == sc.o }) {
			t.Logf("no %s row in the master scenario", sc.name)
		}
	}
}

// TestRecoveredRowShareLocationModelWithLiveRows: live and recovered rows use
// the same Loc, and its invariants hold for both, FileJournal live rows included.
func TestRecoveredRowShareLocationModelWithLiveRows(t *testing.T) {
	r := newRollbackScen(t)
	r.j.Record(2, r.before2)
	f := files{db: r.db, journal: r.j.Bytes()}
	d, h := openAll(t, f.db, nil, f.journal)
	rows, _ := collectRows(t, h)
	var locs []sqlitefile.Loc
	for _, row := range rows {
		locs = append(locs, row.Loc)
	}
	tb, err := d.Live().Table(t.Context(), "t")
	if err != nil {
		t.Fatal(err)
	}
	var liveFiles []sqlitefile.FileKind
	if err := tb.Rows(t.Context(), func(row sqlitefile.Row) bool {
		locs = append(locs, row.Loc)
		liveFiles = append(liveFiles, row.Loc.File)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(liveFiles, sqlitefile.FileJournal) {
		t.Fatalf("no live row in the journal: %v", liveFiles)
	}
	for _, l := range locs {
		if l.Length <= 0 || l.Offset < l.PageOffset || l.Offset+l.Length > l.PageOffset+hps || l.Page == 0 {
			t.Errorf("Loc %+v violates the invariants", l)
		}
		if l.File == sqlitefile.FileDB && l.PageOffset != int64(l.Page-1)*hps {
			t.Errorf("Loc %+v: database page offset", l)
		}
		c := parseAt(t, f, l, hps)
		if c.Length != int(l.Length) {
			t.Errorf("Loc %+v: cell length %d", l, c.Length)
		}
	}
}

// swapLeafPointers returns a copy of the leaf with its cell pointers i and j swapped
// (the cells are then out of key order).
func swapLeafPointers(page []byte, i, j int) []byte {
	p := bytes.Clone(page)
	a, b := 8+2*i, 8+2*j
	p[a], p[a+1], p[b], p[b+1] = p[b], p[b+1], p[a], p[a+1]
	return p
}

// TestHistoryRowsNeverSayAbsentOnAnUncertainLive: when the live lookup cannot
// prove its answer (an unsorted leaf), the relation is unknown with a note,
// never absent-from-live.
func TestHistoryRowsNeverSayAbsentOnAnUncertainLive(t *testing.T) {
	s := newRowScen(t)
	s1, _, _ := s.setT(2, 1)
	s.t.Delete(3)
	live := s.b.Snapshot() // row 3 is gone from the live state
	bad := swapLeafPointers(live.Page(2), 0, 1)
	w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, s1.Page(2), 3)
	w.Frame(2, bad, 3)
	_, h := openAll(t, walMode(s.db0), w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if r.Relation == sqlitefile.RelAbsentFromLive {
			t.Errorf("absent claimed against an unprovable live page: %+v", r)
		}
	}
	r3 := rowOfMethod(t, rows, sqlitefile.MethodWALPrior, 1)
	for _, r := range rows {
		if r.Rowid != nil && *r.Rowid == 3 {
			r3 = r
		}
	}
	if r3.Relation != sqlitefile.RelUnknown || !hasNote(r3, sqlitefile.NoteLiveUncertain) || st.Unknown == 0 {
		t.Errorf("row 3 %+v stats %+v", r3, st)
	}
}

// TestHistoryRowsWithoutRowidNeverAbsentOnDamagedLive: a live row the scan could
// not read is not "absent" for the history row that carries it.
func TestHistoryRowsWithoutRowidNeverAbsentOnDamagedLive(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	wr := b.CreateTableWithoutRowid("wr", "create table wr(k text primary key, v) without rowid", 1)
	for id := int64(1); id <= 3; id++ {
		wr.Insert(id, fmt.Sprintf("key%d", id), "value-v0")
	}
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	bad := bytes.Clone(v0.Page(2))
	// the pointer of the last row now points into the page header
	cnt := int(binary.BigEndian.Uint16(bad[3:]))
	binary.BigEndian.PutUint16(bad[8+2*(cnt-1):], 3)
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(2, v0.Page(2), v0.Pages())
	w.Frame(2, bad, v0.Pages())
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	for _, r := range rows {
		if r.Relation == sqlitefile.RelAbsentFromLive {
			t.Errorf("absent claimed against a damaged live scan: %+v", r)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no row delivered: the unreadable live row is not proven equal")
	}
}

// TestHistoryRowsClippedValueIsNeverProvenEqual: a clipped value cannot be
// proven equal to the live value, so the row is delivered with an unknown
// relation, not counted as a duplicate.
func TestHistoryRowsClippedValueIsNeverProvenEqual(t *testing.T) {
	const ps = 4096
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tb := b.CreateTable("big", "create table big(a, b)")
	tb.Insert(1, int64(100), longText(3<<20, 9))
	n := uint32(len(b.Bytes()) / ps)
	db := withCount(b.Bytes(), n)
	j := b.NewJournal(512, 0x5555aaaa, n)
	j.Record(2, b.Snapshot().Page(2)) // the same page as the live one
	j.SuperJournal("absent-super-journal")
	_, h := openAll(t, db, nil, j.Bytes())
	rows, st := collectRows(t, h)
	if len(rows) != 1 || st.DuplicateOfLive != 0 || rows[0].Relation != sqlitefile.RelUnknown || !hasNote(rows[0], sqlitefile.NoteCompareIncomplete) {
		t.Fatalf("%d rows, stats %+v", len(rows), st)
	}
}

// TestBasisSchemaNeedsTheSameTreeKind: a table-leaf image of a page that is now a
// leaf of an index never gets BasisSchema, even when its cells fit the index.
func TestBasisSchemaNeedsTheSameTreeKind(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	tb := b.CreateTable("i", "create table i(a, b)")
	for id := int64(1); id <= 3; id++ {
		tb.Insert(id, fmt.Sprintf("key-%d", id), id)
	}
	b.CreateIndex("ia", "i", "create index ia on i(a)", 0)
	ixRoot := b.Object("ia").Root()
	live := b.Snapshot()
	db := walMode(withCount(b.Bytes(), live.Pages()))
	donor := sqlitetest.New(sqlitetest.Options{PageSize: hps})
	z := donor.CreateTable("z", "create table z(x, y)")
	for id := int64(1); id <= 3; id++ {
		z.Insert(id, fmt.Sprintf("old-%d", id), id*9)
	}
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(ixRoot, donor.Snapshot().Page(2), live.Pages()) // a table leaf where the index lives
	w.Frame(ixRoot, live.Page(ixRoot), live.Pages())
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, _ := collectRows(t, h)
	var n int
	for _, r := range rows {
		if r.Loc.File == sqlitefile.FileWAL {
			n++
			if r.TableBasis == sqlitefile.BasisSchema || r.Index != "" || !hasNote(r, sqlitefile.NoteOwnerChanged) {
				t.Errorf("row %+v", r)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d rows from the foreign table leaf, want 3", n)
	}
}
