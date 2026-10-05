package sqlitefile_test

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// treeFixture is a standard table of a few hundred rows in 512-byte pages with
// 32 reserved bytes (usable 480) and an index (its root is a page of the wrong
// kind for a table scan).
type treeFixture struct {
	b     *sqlitetest.Builder
	tb    *sqlitetest.Table
	ix    *sqlitetest.Table
	n     int
	ps    int
	reser int
}

func newTreeFixture(t *testing.T, n int, dummyFirst bool) *treeFixture {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512, Reserved: 32})
	if dummyFirst { // takes page 2, so the table's root is page 3
		b.CreateTable("dummy", "create table dummy(a)")
	}
	tb := b.CreateTable("t", stdTableSQL)
	b.CreateIndex("ib", "t", "create index ib on t(b)", 1)
	for i := int64(1); i <= int64(n); i++ {
		tb.Insert(i, genRow(i)...)
	}
	return &treeFixture{b: b, tb: tb, ix: b.Object("ib"), n: n, ps: 512, reser: 32}
}

// rowidsOnLeaf lists the rowids whose cells lie on page pg.
func (f *treeFixture) rowidsOnLeaf(pg uint32) []int64 {
	var out []int64
	for i := int64(1); i <= int64(f.n); i++ {
		if _, page, _ := f.tb.CellBytes(i); page == pg {
			out = append(out, i)
		}
	}
	return out
}

// expectRows checks that rows are exactly the table's rows without lost, in order.
func (f *treeFixture) expectRows(t *testing.T, rows []sqlitefile.Row, lost func(id int64) bool) {
	t.Helper()
	var want []int64
	for i := int64(1); i <= int64(f.n); i++ {
		if !lost(i) {
			want = append(want, i)
		}
	}
	if got := rowids(rows); !slices.Equal(got, want) {
		t.Fatalf("delivered %d rows, want %d: got %v..., want %v...", len(got), len(want), head(got), head(want))
	}
	for _, r := range rows {
		if err := rowIs(r, genRow(r.Rowid)); err != nil {
			t.Fatalf("rowid %d: %v", r.Rowid, err)
		}
	}
}

func head(s []int64) []int64 { return s[:min(len(s), 12)] }

func TestDamagedPageSkippedWithWarning(t *testing.T) {
	type damage struct {
		name string
		// apply patches data (and may choose the root and the pages that are lost)
		// and returns the rows lost, the warning (code, page) expected and
		// whether a page is skipped as a whole.
		apply func(t *testing.T, f *treeFixture, data []byte) (lost func(int64) bool, code string, page uint32, wholePage bool)
	}
	rootChildDamage := func(child func(f *treeFixture) uint32, code string, page func(child uint32) uint32) func(t *testing.T, f *treeFixture, data []byte) (func(int64) bool, string, uint32, bool) {
		return func(_ *testing.T, f *treeFixture, data []byte) (func(int64) bool, string, uint32, bool) {
			root := f.tb.Root()
			key := cellKey(data, f.ps, root, 0) // the subtree under cell 0 holds the rowids up to key
			c := child(f)
			setChild(data, f.ps, root, 0, c)
			return func(id int64) bool { return id <= key }, code, page(c), true
		}
	}
	same := func(c uint32) uint32 { return c }
	var table []damage
	table = append(table,
		damage{"child pointer 0", rootChildDamage(func(*treeFixture) uint32 { return 0 }, sqlitefile.WarnPageRange, same)},
		damage{"child pointer past the end", rootChildDamage(func(f *treeFixture) uint32 { return uint32(len(f.b.Bytes())/f.ps) + 9 }, sqlitefile.WarnPageRange, same)},
		damage{"child pointer to the wrong kind of page", rootChildDamage(func(f *treeFixture) uint32 { return f.ix.Root() }, sqlitefile.WarnPageTypeInvalid, same)},
		damage{"invalid page type", func(_ *testing.T, f *treeFixture, data []byte) (func(int64) bool, string, uint32, bool) {
			leaf := f.tb.Leaves()[1]
			pageAt(data, f.ps, leaf)[0] = 0x07
			ids := f.rowidsOnLeaf(leaf)
			return func(id int64) bool { return slices.Contains(ids, id) }, sqlitefile.WarnPageTypeInvalid, leaf, true
		}},
		damage{"cell count too large", func(_ *testing.T, f *treeFixture, data []byte) (func(int64) bool, string, uint32, bool) {
			leaf := f.tb.Leaves()[1]
			binary.BigEndian.PutUint16(pageAt(data, f.ps, leaf)[3:], 320)
			ids := f.rowidsOnLeaf(leaf)
			return func(id int64) bool { return slices.Contains(ids, id) }, sqlitefile.WarnCellPointer, leaf, true
		}},
		damage{"cell pointer outside the usable size", func(t *testing.T, f *treeFixture, data []byte) (func(int64) bool, string, uint32, bool) {
			leaf := f.tb.Leaves()[1]
			p := pageAt(data, f.ps, leaf)
			victimOff := cellOffsets(p)[2]
			binary.BigEndian.PutUint16(p[8+2*2:], 0x01f0) // 496 is in the reserved bytes
			var victim int64
			for _, id := range f.rowidsOnLeaf(leaf) {
				if _, _, off := f.tb.CellBytes(id); off == victimOff {
					victim = id
				}
			}
			if victim == 0 {
				t.Fatal("no row at the victim cell")
			}
			return func(id int64) bool { return id == victim }, sqlitefile.WarnCellPointer, leaf, false
		}},
		damage{"cell header overrunning the page", func(_ *testing.T, f *treeFixture, data []byte) (func(int64) bool, string, uint32, bool) {
			leaf := f.tb.Leaves()[1]
			var victim int64
			var voff int
			for _, id := range f.rowidsOnLeaf(leaf) {
				cell, _, off := f.tb.CellBytes(id)
				if len(cell) < 100 && off > voff {
					victim, voff = id, off
				}
			}
			// The payload length byte now claims 127 bytes, which run past the usable size.
			pageAt(data, f.ps, leaf)[voff] = 0x7f
			return func(id int64) bool { return id == victim }, sqlitefile.WarnCellPointer, leaf, false
		}},
	)
	for _, d := range table {
		t.Run(d.name, func(t *testing.T) {
			f := newTreeFixture(t, 400, false)
			data := f.b.Bytes()
			if f.tb.Depth() < 2 || len(f.tb.Leaves()) < 3 {
				t.Fatalf("depth %d, %d leaves", f.tb.Depth(), len(f.tb.Leaves()))
			}
			lost, code, page, whole := d.apply(t, f, data)
			_, v := openLive(t, data, sqlitefile.Options{})
			rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
			f.expectRows(t, rows, lost)
			if !viewWarns(v, code, page) {
				t.Errorf("no %s warning for page %d: %v", code, page, v.Warnings())
			}
			skipped := v.Stats().PagesSkipped
			if whole && skipped < 1 {
				t.Errorf("PagesSkipped = %d after a whole page was skipped", skipped)
			}
			if !whole && skipped != 0 {
				t.Errorf("PagesSkipped = %d: only the bad cell is lost, the page is read", skipped)
			}
		})
	}
}

// TestDamagedPageLockBytePointerMapPage covers the two child numbers that
// need the header to say so: the lock-byte page (a file over 1 GiB) and a
// pointer-map page (auto-vacuum).
func TestDamagedPageLockBytePointerMapPage(t *testing.T) {
	t.Run("lock-byte page", func(t *testing.T) {
		f := newTreeFixture(t, 400, false)
		lock := sqlitefile.LockBytePage(512)
		f.b.SetHeaderPages(lock+20, true) // the header counts pages up to past the lock-byte page
		data := f.b.Bytes()
		key := cellKey(data, f.ps, f.tb.Root(), 0)
		setChild(data, f.ps, f.tb.Root(), 0, lock)
		_, v := openLive(t, data, sqlitefile.Options{})
		rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		f.expectRows(t, rows, func(id int64) bool { return id <= key })
		if !viewWarns(v, sqlitefile.WarnPageRange, lock) || v.Stats().PagesSkipped != 1 {
			t.Errorf("warnings %v, skipped %d", v.Warnings(), v.Stats().PagesSkipped)
		}
	})
	t.Run("pointer-map page", func(t *testing.T) {
		f := newTreeFixture(t, 1500, true)
		data := f.b.Bytes()
		const mapPage = 99 // pages 2 and 99 are pointer-map pages when U is 480
		if len(data)/f.ps < mapPage+1 {
			t.Fatalf("only %d pages", len(data)/f.ps)
		}
		binary.BigEndian.PutUint32(data[52:], 7) // the largest root page: auto-vacuum is on
		key := cellKey(data, f.ps, f.tb.Root(), 0)
		setChild(data, f.ps, f.tb.Root(), 0, mapPage)
		_, v := openLive(t, data, sqlitefile.Options{})
		if v.Info().AutoVacuum == sqlitefile.AVNone {
			t.Fatal("auto-vacuum not seen")
		}
		rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		// The pointer-map pages (2, 99, 196, ...) that really hold leaves are skipped too: their rows are lost
		onMap := map[int64]bool{}
		for pg := uint32(mapPage); int(pg) <= len(data)/f.ps; pg += 97 {
			if slices.Contains(f.tb.Interiors(), pg) {
				t.Fatalf("page %d is an interior page of the table: pick another layout", pg)
			}
			for _, id := range f.rowidsOnLeaf(pg) {
				onMap[id] = true
			}
		}
		f.expectRows(t, rows, func(id int64) bool { return id <= key || onMap[id] })
		if !viewWarns(v, sqlitefile.WarnPageRange, mapPage) || v.Stats().PagesSkipped < 1 {
			t.Errorf("warnings %v, skipped %d", v.Warnings(), v.Stats().PagesSkipped)
		}
	})
	t.Run("without auto-vacuum the same page is an ordinary page", func(t *testing.T) {
		f := newTreeFixture(t, 1500, true)
		data := f.b.Bytes()
		_, v := openLive(t, data, sqlitefile.Options{})
		scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		if viewWarns(v, sqlitefile.WarnPageRange, 0) {
			t.Errorf("warnings %v", v.Warnings())
		}
	})
}

func TestPageUnavailableIsPerPage(t *testing.T) {
	f := newTreeFixture(t, 400, false)
	pages := len(f.b.Bytes()) / f.ps
	f.b.SetHeaderPages(uint32(pages), true)
	full := f.b.Bytes()
	keep := pages * 6 / 10
	var want []int64
	for i := int64(1); i <= int64(f.n); i++ {
		if _, pg, _ := f.tb.CellBytes(i); int(pg) <= keep {
			want = append(want, i)
		}
	}
	for _, l := range f.tb.Leaves() {
		if int(l) > keep {
			goto ok
		}
	}
	t.Fatal("no leaf beyond the cut")
ok:
	data := full[:keep*f.ps]
	db, v := openLive(t, data, sqlitefile.Options{})
	if db.Info().PageCount != uint32(pages) || v.Addressable() != uint32(keep) {
		t.Fatalf("PageCount %d Addressable %d, want %d and %d", db.Info().PageCount, v.Addressable(), pages, keep)
	}
	rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
	if got := rowids(rows); !slices.Equal(got, want) {
		t.Fatalf("%d rows delivered, want %d (the rows on pages up to %d)", len(got), len(want), keep)
	}
	if !viewWarns(v, sqlitefile.WarnPageUnavailable, 0) || v.Stats().PagesSkipped == 0 {
		t.Errorf("warnings %v, skipped %d", v.Warnings(), v.Stats().PagesSkipped)
	}
	// ReadPage says the same.
	if _, err := v.ReadPage(uint32(keep) + 1); err == nil {
		t.Error("ReadPage past the end succeeded")
	}
	if p, err := v.ReadPage(uint32(keep)); err != nil || p.Number != uint32(keep) || len(p.Data) != f.ps || p.Loc.File != sqlitefile.FileDB {
		t.Errorf("ReadPage(last) = %+v, %v", p.Loc, err)
	}
}

func TestAddressableClampedByPhysicalPages(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", stdTableSQL)
	for i := int64(1); i <= 40; i++ {
		tb.Insert(i, genRow(i)...)
	}
	physical := uint32(len(b.Bytes()) / 512)
	b.SetHeaderPages(0xffffffff, true)
	data := b.Bytes()
	rb := newRecBudget(1 << 30)
	db, v := openLive(t, data, sqlitefile.Options{Budget: rb})
	if db.Info().PageCount != 0xffffffff {
		t.Fatalf("PageCount = %d: the claim must stay visible", db.Info().PageCount)
	}
	if v.Addressable() != physical {
		t.Fatalf("Addressable = %d, want %d", v.Addressable(), physical)
	}
	if !viewWarns(v, sqlitefile.WarnPageCountClamped, 0) {
		t.Errorf("warnings: %v", v.Warnings())
	}
	if rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree); len(rows) != 40 {
		t.Errorf("%d rows", len(rows))
	}
	// What the scan charged on top of the page cache is the bitset of the
	// addressable pages, a few words: never a claim-sized array.
	if rb.peak > int64(physical)*512+64*1024 {
		t.Errorf("peak charge %d bytes for a %d-page file", rb.peak, physical)
	}
	v.Release()
	rb.check(t)

	// MaxPages lowered in the test clamps a larger real file.
	f := newTreeFixture(t, 400, false)
	big := f.b.Bytes()
	_, v = openLive(t, big, sqlitefile.Options{Limits: sqlitefile.Limits{MaxPages: 5}})
	if v.Addressable() != 5 || !viewWarns(v, sqlitefile.WarnPageCountClamped, 0) {
		t.Errorf("MaxPages 5: Addressable %d, warnings %v", v.Addressable(), v.Warnings())
	}
	if rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree); len(rows) >= f.n {
		t.Errorf("%d rows from a file clamped to 5 pages", len(rows))
	}
	if _, err := v.ReadPage(6); err == nil {
		t.Error("page 6 is above MaxPages")
	}
}

func TestBTreeCycleVisitedSet(t *testing.T) {
	const n = 5000
	setup := func(t *testing.T) (*treeFixture, []byte, []uint32) {
		b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, n)
		f := &treeFixture{b: b, tb: tb, n: n, ps: 512}
		if tb.Depth() < 3 {
			t.Fatalf("depth %d", tb.Depth())
		}
		data := b.Bytes()
		// The root's children: interior pages of the middle level.
		root := tb.Root()
		var kids []uint32
		p := pageAt(data, 512, root)
		for _, off := range cellOffsets(p) {
			kids = append(kids, vtU32(p[off:]))
		}
		kids = append(kids, vtU32(p[8:]))
		return f, data, kids
	}
	check := func(t *testing.T, v *sqlitefile.View, code string) {
		t.Helper()
		st := v.Stats()
		if st.PageReads > int64(v.Addressable()) {
			t.Errorf("%d page reads for %d pages", st.PageReads, v.Addressable())
		}
		if !viewWarns(v, code, 0) {
			t.Errorf("no %s warning: %v", code, v.Warnings())
		}
	}
	t.Run("a page that is its own child", func(t *testing.T) {
		f, data, _ := setup(t)
		setChild(data, 512, f.tb.Root(), 0, f.tb.Root())
		_, v := openLive(t, data, sqlitefile.Options{})
		rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		check(t, v, sqlitefile.WarnBTreeCycle)
		if len(rows) == 0 || len(rows) >= n {
			t.Errorf("%d rows", len(rows))
		}
	})
	t.Run("two interior pages pointing at each other", func(t *testing.T) {
		f, data, kids := setup(t)
		a, b := kids[0], kids[1]
		setChild(data, 512, a, 0, b)
		setChild(data, 512, b, 0, a)
		_, v := openLive(t, data, sqlitefile.Options{})
		scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		check(t, v, sqlitefile.WarnBTreeCycle)
	})
	t.Run("a leaf shared by two parents", func(t *testing.T) {
		f, data, kids := setup(t)
		a, b := kids[0], kids[1]
		pa := pageAt(data, 512, a)
		shared := vtU32(pa[cellOffsets(pa)[0]:])
		setChild(data, 512, b, 0, shared)
		_, v := openLive(t, data, sqlitefile.Options{})
		rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		check(t, v, sqlitefile.WarnBTreeShape)
		seen := map[int64]bool{}
		for _, r := range rows {
			if seen[r.Rowid] {
				t.Fatalf("rowid %d delivered twice", r.Rowid)
			}
			seen[r.Rowid] = true
		}
	})
	t.Run("an overflow pointer into a leaf", func(t *testing.T) {
		f, data, _ := setup(t)
		leaf := f.tb.Leaves()[5]
		ov := f.tb.Overflow(97)
		if len(ov) == 0 {
			t.Fatal("row 97 has no overflow")
		}
		binary.BigEndian.PutUint32(pageAt(data, 512, ov[0]), leaf) // the chain now continues into a live leaf
		_, v := openLive(t, data, sqlitefile.Options{})
		rows := scanRows(t, v, f.tb.Root(), sqlitefile.TableTree)
		if st := v.Stats(); st.PageReads > int64(v.Addressable()) {
			t.Errorf("%d page reads", st.PageReads)
		}
		if !viewWarns(v, sqlitefile.WarnBTreeShape, leaf) && !viewWarns(v, sqlitefile.WarnCellOverflowChain, 0) {
			t.Errorf("a page that is both overflow and leaf went unnoticed: %v", v.Warnings())
		}
		if len(rows) < n-300 {
			t.Errorf("%d rows", len(rows))
		}
	})
}

func TestBTreeDepthCap(t *testing.T) {
	// 40 interior pages in a chain (page i's right child is page i+1), then a leaf.
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	const chain = 40
	b.SetHeaderPages(chain+2, true)
	data := b.Bytes()
	for i := 0; i < chain; i++ {
		p := make([]byte, 512)
		p[0] = 0x05
		binary.BigEndian.PutUint16(p[5:], 512&0xffff)
		binary.BigEndian.PutUint32(p[8:], uint32(i+3))
		data = append(data, p...)
	}
	leaf := make([]byte, 512)
	leaf[0] = 0x0d
	binary.BigEndian.PutUint16(leaf[5:], 512&0xffff)
	data = append(data, leaf...)
	_, v := openLive(t, data, sqlitefile.Options{})
	rows := scanRows(t, v, 2, sqlitefile.TableTree)
	if len(rows) != 0 {
		t.Errorf("%d rows", len(rows))
	}
	if !viewWarns(v, sqlitefile.WarnBTreeDepth, 0) {
		t.Errorf("warnings: %v", v.Warnings())
	}
	if got := v.Stats().PageReads; got > 32 {
		t.Errorf("%d pages read, the cap is 32 levels", got)
	}
	// A lowered MaxBTreeDepth is honoured by lookups too.
	_, v = openLive(t, data, sqlitefile.Options{Limits: sqlitefile.Limits{MaxBTreeDepth: 4}})
	// (the path is damaged, so the answer is uncertain, never "absent")
	if _, ok, err := v.LookupRowid(context.Background(), 2, 1); ok || !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Errorf("lookup in a too deep tree: %v %v", ok, err)
	}
	if got := v.Stats().PageReads; got > 4 || !viewWarns(v, sqlitefile.WarnBTreeDepth, 0) {
		t.Errorf("%d page reads, warnings %v", got, v.Warnings())
	}
}

func TestBTreeOrderViolationWarns(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	for i := int64(1); i <= 100; i++ {
		tb.Insert(i, "r")
	}
	data := b.Bytes()
	_, page, off := tb.CellBytes(60)
	p := pageAt(data, 512, page)
	// cell: payload length (1 byte), rowid (1 byte for 60), payload
	if p[off+1] != 60 {
		t.Fatalf("rowid byte %d", p[off+1])
	}
	p[off+1] = 5 // rowid 60 now says 5: out of order
	_, v := openLive(t, data, sqlitefile.Options{})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if len(rows) != 100 {
		t.Errorf("%d rows: out of order rows are still delivered", len(rows))
	}
	if !viewWarns(v, sqlitefile.WarnBTreeOrder, 0) {
		t.Errorf("warnings: %v", v.Warnings())
	}
	// An interior key below the rows under it is noticed too.
	f := newTreeFixture(t, 400, false)
	data = f.b.Bytes()
	root := f.tb.Root()
	rp := pageAt(data, 512, root)
	kOff := cellOffsets(rp)[0] + 4
	_, n := vtVarint(rp[kOff:])
	if n != 1 {
		t.Skip("key is not a one byte varint")
	}
	rp[kOff] = 1 // the subtree under cell 0 holds rowids above 1
	_, v = openLive(t, data, sqlitefile.Options{})
	scanRows(t, v, root, sqlitefile.TableTree)
	if !viewWarns(v, sqlitefile.WarnBTreeOrder, root) {
		t.Errorf("a bounding key below its subtree's rows: %v", v.Warnings())
	}
}

func TestBTreeShapeWarns(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 5000)
	other := b.CreateTable("other", "create table other(a)")
	other.Insert(1, "x")
	data := b.Bytes()
	if tb.Depth() < 3 || other.Depth() != 1 {
		t.Fatalf("depths %d %d", tb.Depth(), other.Depth())
	}
	// Cell 0 of the root now points at a leaf, while its siblings lead to interior pages.
	setChild(data, 512, tb.Root(), 0, other.Root())
	_, v := openLive(t, data, sqlitefile.Options{})
	scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if !viewWarns(v, sqlitefile.WarnBTreeShape, 0) {
		t.Errorf("warnings: %v", v.Warnings())
	}
}
