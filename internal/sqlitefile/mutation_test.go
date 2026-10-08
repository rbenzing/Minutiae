package sqlitefile_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// ptrIndexOf returns the index of the cell pointer of page (whose b-tree header
// starts at base) that holds offset off, or -1.
func ptrIndexOf(page []byte, base, off int) int {
	hdr := base + 8
	if page[base] == 0x02 || page[base] == 0x05 {
		hdr = base + 12
	}
	n := vtU16(page[base+3:])
	for i := 0; i < n; i++ {
		if vtU16(page[hdr+2*i:]) == off {
			return i
		}
	}
	return -1
}

// TestLocCellIsThePointerIndex: Loc.Cell is the index of the cell in the page's
// pointer array, for table rows, index entries (leaf and interior) and rows of
// page 1 (whose header starts at byte 100).
func TestLocCellIsThePointerIndex(t *testing.T) {
	f := newTreeFixture(t, 400, false)
	f.b.CreateTable("more", "create table more(a)").Insert(1, "x")
	data := f.b.Bytes()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	check := func(name string, rows []sqlitefile.Row, ps int) {
		t.Helper()
		if len(rows) < 3 {
			t.Fatalf("%s: %d rows", name, len(rows))
		}
		seenIdx := map[int]bool{}
		for _, r := range rows {
			base := 0
			if r.Loc.Page == 1 {
				base = 100
			}
			page := pageAt(data, ps, r.Loc.Page)
			want := ptrIndexOf(page, base, int(r.Loc.Offset-r.Loc.PageOffset))
			if want < 0 || r.Loc.Cell != want {
				t.Fatalf("%s: row on page %d at offset %d has Loc.Cell %d, the pointer index is %d", name, r.Loc.Page, r.Loc.Offset, r.Loc.Cell, want)
			}
			seenIdx[want] = true
		}
		if len(seenIdx) < 2 {
			t.Fatalf("%s: every row sits at pointer index %v: the check is vacuous", name, seenIdx)
		}
	}
	check("table", scanRows(t, v, f.tb.Root(), sqlitefile.TableTree), 512)
	check("index", scanRows(t, v, f.ix.Root(), sqlitefile.IndexTree), 512)
	check("page 1", scanRows(t, v, 1, sqlitefile.TableTree), 512)
}

// TestRowPayloadLenIsTheDeclaredPayload: Row.PayloadLen is the payload length
// the cell declares (overflow bytes included), for table rows and index entries.
func TestRowPayloadLenIsTheDeclaredPayload(t *testing.T) {
	f := newTreeFixture(t, 400, false)
	_, v := openLive(t, f.b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	spilled := 0
	for _, r := range scanRows(t, v, f.tb.Root(), sqlitefile.TableTree) {
		cell, _, _ := f.tb.CellBytes(r.Rowid)
		p, _ := vtVarint(cell)
		if r.PayloadLen != int64(p) || p == 0 {
			t.Fatalf("rowid %d: PayloadLen %d, the cell declares %d", r.Rowid, r.PayloadLen, p)
		}
		if r.Loc.OverflowTotal > 0 {
			spilled++
		}
	}
	if spilled == 0 {
		t.Fatal("no overflow row: the check does not cover the spilled case")
	}
	data := f.b.Bytes()
	for _, r := range scanRows(t, v, f.ix.Root(), sqlitefile.IndexTree) {
		cell := data[r.Loc.Offset:]
		if data[r.Loc.PageOffset] == 0x02 { // interior index cell: a 4 byte child first
			cell = cell[4:]
		}
		if p, _ := vtVarint(cell); r.PayloadLen != int64(p) || p == 0 {
			t.Fatalf("index entry on page %d: PayloadLen %d, the cell declares %d", r.Loc.Page, r.PayloadLen, p)
		}
	}
}

// TestPageOneIsNeverAChild: a child pointer to page 1 is a page-range damage
// (page 1 roots the schema table); its rows are never delivered as the rows of
// another tree.
func TestPageOneIsNeverAChild(t *testing.T) {
	f := newTreeFixture(t, 400, false)
	data := f.b.Bytes()
	key := cellKey(data, f.ps, f.tb.Root(), 0)
	setChild(data, f.ps, f.tb.Root(), 0, 1)
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
	f.expectRows(t, rows, func(id int64) bool { return id <= key })
	if !viewWarns(v, sqlitefile.WarnPageRange, 1) || v.Stats().PagesSkipped != 1 {
		t.Errorf("warnings %v, skipped %d", v.Warnings(), v.Stats().PagesSkipped)
	}
	if got, _ := lookupOutcome(t, data, f.tb.Root(), key-1); got != "corrupt" {
		t.Errorf("Get through the page 1 child = %s", got)
	}
}

// TestDuplicateRowidWarns: two cells of one leaf with the same rowid are a
// btree-order warning on that leaf (both rows are still delivered).
func TestDuplicateRowidWarns(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 100; i++ {
		tb.Insert(i, "r")
	}
	data := b.Bytes()
	// Rows 60 and 61 on one leaf, 61 not first on it: 61 now says 60.
	_, p60, _ := tb.CellBytes(60)
	_, p61, off61 := tb.CellBytes(61)
	if p60 != p61 {
		t.Skipf("rows 60 and 61 are on pages %d and %d", p60, p61)
	}
	pg := pageAt(data, 512, p61)
	if pg[off61+1] != 61 {
		t.Fatalf("rowid byte %d", pg[off61+1])
	}
	pg[off61+1] = 60
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if len(rows) != 100 {
		t.Errorf("%d rows", len(rows))
	}
	if !viewWarns(v, sqlitefile.WarnBTreeOrder, p61) {
		t.Errorf("no btree-order warning on page %d: %v", p61, v.Warnings())
	}
}

// TestLookupFindsEveryInteriorSeparatorKey: an interior key is the largest
// rowid of the subtree on its left, so Get of that rowid (and of its
// neighbours) goes left; pinned on purpose at every separator of a 3 level tree.
func TestLookupFindsEveryInteriorSeparatorKey(t *testing.T) {
	b, tb := bigTable(t)
	data := b.Bytes()
	var keys []int64
	for _, pg := range tb.Interiors() {
		for i := range cellOffsets(pageAt(data, 512, pg)) {
			keys = append(keys, cellKey(data, 512, pg, i))
		}
	}
	if len(keys) < 30 {
		t.Fatalf("%d separator keys", len(keys))
	}
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	for _, k := range keys {
		for _, id := range []int64{k - 1, k, k + 1} {
			r, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
			if err != nil || !ok || r.Rowid != id || rowIs(r, genRow(id)) != nil {
				t.Fatalf("Get(%d) (separator %d): ok %v err %v", id, k, ok, err)
			}
		}
	}
}

// TestRecordHeaderBufferIsBoundedByRealBytes: a cell declaring an 18000 byte
// record header over 196 real bytes costs memory in proportion to the real
// bytes, however often it is read.
func TestRecordHeaderBufferIsBoundedByRealBytes(t *testing.T) {
	const ps = 512
	data := make([]byte, 196)
	copy(data, []byte{0x81, 0x8c, 0x50}) // header length 1<<14 + 12<<7 + 80 = 18000
	c := sqlitefile.Cell{LocalBytes: data, PayloadLen: 20000}
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	src := sqlitefile.NewFakeSource()
	run := func() {
		l := env.Ledger()
		vis, err := env.NewMapVisited(l, 4)
		if err != nil {
			t.Fatal(err)
		}
		p := env.NewTestPayload(src, l, vis, ps, c)
		_, held, _, err := env.ReadRecord(l, p, sqlitefile.EncUTF8, nil)
		if err == nil {
			t.Fatal("a header longer than the bytes that exist was accepted")
		}
		p.Release()
		vis.Release(l)
		l.Free(held)
	}
	run()
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	const reads = 200
	for range reads {
		run()
	}
	runtime.ReadMemStats(&ms1)
	if got := ms1.TotalAlloc - ms0.TotalAlloc; got > 2<<20 {
		t.Errorf("%d reads of a hostile header allocated %d KiB, want under 2 MiB (18 KB each is the declared length)", reads, got>>10)
	}
}

// TestCellPointerSlicesAreCharged: the list of followable cell pointers of a
// page is charged to the budget while the page is walked and given back after.
func TestCellPointerSlicesAreCharged(t *testing.T) {
	peak := func(cells int) int64 {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 65536})
		tb := b.CreateTable("t", "create table t(a)")
		for i := int64(1); i <= int64(cells); i++ {
			tb.Insert(i, i)
		}
		if tb.Depth() != 1 {
			t.Fatalf("depth %d", tb.Depth())
		}
		rb := newRecBudget(1 << 30)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
		scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		v.Release()
		rb.check(t)
		return rb.peak
	}
	small, big := peak(1), peak(5000)
	if big-small < 5000*16 {
		t.Errorf("peak charge %d with 5000 cells against %d with one: the pointer list is not charged", big, small)
	}
}

// TestOverlappingCellPointersAreWarnedAndRead: pointers that make cells overlap
// (two pointers at one cell, or a cell starting inside another) raise one
// cell-pointer warning for the page; the cells are still read.
func TestOverlappingCellPointersAreWarnedAndRead(t *testing.T) {
	build := func(t *testing.T) (*sqlitetest.Builder, *sqlitetest.Table) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "create table t(a)")
		for i := int64(1); i <= 12; i++ {
			tb.Insert(i, "row-value-padding")
		}
		if tb.Depth() != 1 {
			t.Fatalf("depth %d", tb.Depth())
		}
		return b, tb
	}
	t.Run("two pointers at one cell", func(t *testing.T) {
		b, tb := build(t)
		data := b.Bytes()
		pg := pageAt(data, 512, tb.Root())
		copy(pg[8+2*5:], pg[8+2*4:8+2*4+2]) // pointer 5 now equals pointer 4
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		if len(rows) != 12 {
			t.Errorf("%d rows: the cells are still read", len(rows))
		}
		n := 0
		for _, w := range v.Warnings() {
			if w.Code == sqlitefile.WarnCellPointer && w.Page == tb.Root() {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d cell-pointer warnings for the page, want 1: %v", n, v.Warnings())
		}
	})
	t.Run("a cell starting inside another", func(t *testing.T) {
		b, tb := build(t)
		data := b.Bytes()
		pg := pageAt(data, 512, tb.Root())
		offs := cellOffsets(pg)
		// Cell 4 now claims 3 more payload bytes: it runs into cell 3, which sits just above it.
		pg[offs[4]] += 3
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		if !viewWarns(v, sqlitefile.WarnCellPointer, tb.Root()) {
			t.Errorf("no cell-pointer warning: %v", v.Warnings())
		}
	})
	t.Run("a clean page has no such warning", func(t *testing.T) {
		b, tb := build(t)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
		defer v.Release()
		scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		if viewWarns(v, sqlitefile.WarnCellPointer, 0) {
			t.Errorf("warnings %v", v.Warnings())
		}
	})
}
