package sqlitefile_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// putVarint2 encodes v (0..16383) as one or two bytes.
func putVarint2(v uint64) []byte {
	if v < 0x80 {
		return []byte{byte(v)}
	}
	return []byte{0x80 | byte(v>>7), byte(v & 0x7f)}
}

// setCellKey rewrites the rowid key of interior table cell idx of page pgno
// (the encoding must keep its length).
func setCellKey(t *testing.T, data []byte, ps int, pgno uint32, idx int, key int64) {
	t.Helper()
	p := pageAt(data, ps, pgno)
	off := cellOffsets(p)[idx] + 4
	_, n := vtVarint(p[off:])
	enc := putVarint2(uint64(key))
	if len(enc) != n {
		t.Fatalf("key %d does not keep the %d byte encoding", key, n)
	}
	copy(p[off:], enc)
}

// bigTable is the reviewer's table: 5000 rows in 512-byte pages, depth 3.
func bigTable(t *testing.T) (*sqlitetest.Builder, *sqlitetest.Table) {
	t.Helper()
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 5000)
	if tb.Depth() < 3 {
		t.Fatalf("depth %d", tb.Depth())
	}
	return b, tb
}

// midInterior returns a non-root interior page with at least four cells.
func midInterior(t *testing.T, tb *sqlitetest.Table, data []byte) uint32 {
	t.Helper()
	for _, pg := range tb.Interiors() {
		if pg != tb.Root() && len(cellOffsets(pageAt(data, 512, pg))) >= 4 {
			return pg
		}
	}
	t.Fatal("no suitable interior page")
	return 0
}

// TestScanReportsKeyRangeViolations: the scan delivers rows as the engine walk
// does, but checks BOTH bounds of every interior key range (the left separator
// below, the right separator above, inherited down the tree) and flags the
// rows outside them: a btree-order warning and Row.KeyRangeViolation.
func TestScanReportsKeyRangeViolations(t *testing.T) {
	type tcase struct {
		name string
		// apply patches data and returns the rowids that must be flagged.
		apply func(t *testing.T, tb *sqlitetest.Table, data []byte) (flagged func(int64) bool, page uint32)
	}
	rootKey := func(idx func(n int) int, delta int64) func(*testing.T, *sqlitetest.Table, []byte) (func(int64) bool, uint32) {
		return func(t *testing.T, tb *sqlitetest.Table, data []byte) (func(int64) bool, uint32) {
			root := tb.Root()
			i := idx(len(cellOffsets(pageAt(data, 512, root))))
			old := cellKey(data, 512, root, i)
			setCellKey(t, data, 512, root, i, old+delta)
			lo, hi := old, old+delta
			if lo > hi {
				lo, hi = hi, lo
			}
			return func(id int64) bool { return id > lo && id <= hi }, root
		}
	}
	first := func(int) int { return 0 }
	last := func(n int) int { return n - 1 }
	for _, c := range []tcase{
		{"first root key raised (the reviewer's 630 to 635)", rootKey(first, +5)},
		{"first root key lowered", rootKey(first, -5)},
		{"last root key raised (the right-most child's lower bound)", rootKey(last, +5)},
		{"last root key lowered", rootKey(last, -5)},
		{"a key of a middle interior page raised (bounds are inherited and checked at every level)", func(t *testing.T, tb *sqlitetest.Table, data []byte) (func(int64) bool, uint32) {
			pg := midInterior(t, tb, data)
			old := cellKey(data, 512, pg, 1)
			setCellKey(t, data, 512, pg, 1, old+5)
			return func(id int64) bool { return id > old && id <= old+5 }, pg
		}},
		{"a key of a middle interior page lowered", func(t *testing.T, tb *sqlitetest.Table, data []byte) (func(int64) bool, uint32) {
			pg := midInterior(t, tb, data)
			old := cellKey(data, 512, pg, 1)
			setCellKey(t, data, 512, pg, 1, old-5)
			return func(id int64) bool { return id > old-5 && id <= old }, pg
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, tb := bigTable(t)
			data := b.Bytes()
			flagged, page := c.apply(t, tb, data)
			_, v := openLive(t, data, sqlitefile.Options{})
			defer v.Release()
			rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
			if len(rows) != 5000 {
				t.Fatalf("%d rows: the walk delivers every row, as the engine does", len(rows))
			}
			var got, want []int64
			for _, r := range rows {
				if r.KeyRangeViolation {
					got = append(got, r.Rowid)
				}
				if flagged(r.Rowid) {
					want = append(want, r.Rowid)
				}
			}
			if !slices.Equal(got, want) || len(want) != 5 {
				t.Errorf("flagged rows %v, want %v", got, want)
			}
			if !viewWarns(v, sqlitefile.WarnBTreeOrder, page) {
				t.Errorf("no btree-order warning on page %d: %v", page, v.Warnings())
			}
		})
	}
	t.Run("a clean tree flags nothing", func(t *testing.T) {
		b, tb := bigTable(t)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
		defer v.Release()
		for _, r := range scanRows(t, v, tb.Root(), sqlitefile.TableTree) {
			if r.KeyRangeViolation {
				t.Fatalf("rowid %d flagged in a clean tree", r.Rowid)
			}
		}
		if viewWarns(v, sqlitefile.WarnBTreeOrder, 0) {
			t.Errorf("warnings %v", v.Warnings())
		}
	})
	t.Run("interior keys that are not increasing", func(t *testing.T) {
		b, tb := bigTable(t)
		data := b.Bytes()
		root := tb.Root()
		setCellKey(t, data, 512, root, 1, cellKey(data, 512, root, 0)-3)
		_, v := openLive(t, data, sqlitefile.Options{})
		defer v.Release()
		scanRows(t, v, root, sqlitefile.TableTree)
		if !viewWarns(v, sqlitefile.WarnBTreeOrder, root) {
			t.Errorf("warnings %v", v.Warnings())
		}
	})
}

// lookupOutcome returns "row", "absent" or "corrupt" for Get of id.
func lookupOutcome(t *testing.T, data []byte, root uint32, id int64) (string, sqlitefile.Row) {
	t.Helper()
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	r, ok, err := v.LookupRowid(context.Background(), root, id)
	switch {
	case err != nil && errors.Is(err, sqlitefile.ErrCorrupt):
		return "corrupt", sqlitefile.Row{}
	case err != nil:
		t.Fatalf("Get(%d): %v", id, err)
	case ok:
		return "row", r
	}
	return "absent", sqlitefile.Row{}
}

// assertLookupAgreesWithScan: for every row the scan delivers, Get returns the
// same row or says the answer is uncertain (a typed error wrapping
// ErrCorrupt); it never answers absent for it.
func assertLookupAgreesWithScan(t *testing.T, data []byte, root uint32, ids []int64) {
	t.Helper()
	_, sv := openLive(t, data, sqlitefile.Options{})
	defer sv.Release()
	delivered := map[int64]sqlitefile.Row{}
	for _, r := range scanRows(t, sv, root, sqlitefile.TableTree) {
		delivered[r.Rowid] = r
	}
	for _, id := range ids {
		want, ok := delivered[id]
		if !ok {
			continue
		}
		switch got, row := lookupOutcome(t, data, root, id); got {
		case "absent":
			t.Errorf("rowid %d is delivered by the scan but Get answers absent", id)
		case "row":
			if sameValues(row.Values, want.Values) != nil || row.Loc.Offset != want.Loc.Offset {
				t.Errorf("rowid %d: Get and the scan return different rows", id)
			}
		}
	}
}

func idRange(lo, hi int64) []int64 {
	var out []int64
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// TestLookupNeverAnswersAbsentOnDamage: an absent answer is given only when the
// search path was clean. Meeting an unparseable cell at a search midpoint, a
// damaged page, an unreadable row or a key-range violation gives a typed error
// wrapping ErrCorrupt.
func TestLookupNeverAnswersAbsentOnDamage(t *testing.T) {
	t.Run("an unparseable cell at a search midpoint (the reviewer's 4 rows)", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "create table t(a)")
		for i := int64(1); i <= 4; i++ {
			tb.Insert(i, fmt.Sprintf("v%d", i))
		}
		data := b.Bytes()
		_, pg, off := tb.CellBytes(3)
		pageAt(data, 512, pg)[off] = 0x7f // claims 127 payload bytes: runs past the page
		_, sv := openLive(t, data, sqlitefile.Options{})
		if got := rowids(scanRows(t, sv, tb.Root(), sqlitefile.TableTree)); !slices.Equal(got, []int64{1, 2, 4}) {
			t.Fatalf("the scan delivers %v", got)
		}
		for _, id := range []int64{3, 4, 5, 0} {
			if got, _ := lookupOutcome(t, data, tb.Root(), id); got != "corrupt" {
				t.Errorf("Get(%d) = %s, want corrupt (the search met the unparseable cell)", id, got)
			}
		}
		assertLookupAgreesWithScan(t, data, tb.Root(), idRange(0, 6))
	})
	t.Run("an unparseable cell inside a 400-row leaf", func(t *testing.T) {
		f := newTreeFixture(t, 400, false)
		data := f.b.Bytes()
		leaf := f.tb.Leaves()[1]
		var victim int64
		var voff int
		for _, id := range f.rowidsOnLeaf(leaf) {
			cell, _, off := f.tb.CellBytes(id)
			if len(cell) < 100 && off > voff {
				victim, voff = id, off
			}
		}
		pageAt(data, f.ps, leaf)[voff] = 0x7f
		ids := f.rowidsOnLeaf(leaf)
		assertLookupAgreesWithScan(t, data, f.tb.Root(), idRange(1, 403))
		if got, _ := lookupOutcome(t, data, f.tb.Root(), victim); got != "corrupt" {
			t.Errorf("Get of the unreadable row = %s", got)
		}
		for _, id := range ids {
			if got, _ := lookupOutcome(t, data, f.tb.Root(), id); got == "absent" {
				t.Errorf("Get(%d) on the damaged leaf answers absent", id)
			}
		}
	})
	t.Run("a damaged page on the path", func(t *testing.T) {
		f := newTreeFixture(t, 400, false)
		data := f.b.Bytes()
		leaf := f.tb.Leaves()[1]
		ids := f.rowidsOnLeaf(leaf)
		pageAt(data, f.ps, leaf)[0] = 0x07
		for _, id := range []int64{ids[0], ids[len(ids)/2], ids[len(ids)-1]} {
			if got, _ := lookupOutcome(t, data, f.tb.Root(), id); got != "corrupt" {
				t.Errorf("Get(%d) = %s, want corrupt", id, got)
			}
		}
		if got, _ := lookupOutcome(t, data, f.tb.Root(), 1); got != "row" { // another leaf: clean
			t.Errorf("Get(1) on a clean path = %s", got)
		}
	})
	t.Run("a child pointer to nowhere", func(t *testing.T) {
		f := newTreeFixture(t, 400, false)
		data := f.b.Bytes()
		key := cellKey(data, f.ps, f.tb.Root(), 0)
		setChild(data, f.ps, f.tb.Root(), 0, 0)
		if got, _ := lookupOutcome(t, data, f.tb.Root(), key-1); got != "corrupt" {
			t.Errorf("Get below the broken child = %s", got)
		}
	})
	t.Run("a row that cannot be read is not absent", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "create table t(a)")
		tb.Insert(1, "one")
		tb.InsertRaw(2, []uint64{1, 13 + 2*40}, []byte{9}) // declares bytes it lacks
		tb.Insert(3, "three")
		if got, _ := lookupOutcome(t, b.Bytes(), tb.Root(), 2); got != "corrupt" {
			t.Errorf("Get of an unreadable row = %s", got)
		}
		if got, _ := lookupOutcome(t, b.Bytes(), tb.Root(), 4); got != "absent" {
			t.Errorf("a clean miss after it = %s, want absent", got)
		}
	})
	t.Run("a clean path still answers absent", func(t *testing.T) {
		b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 300)
		data := b.Bytes()
		for _, id := range []int64{0, -5, 301, 1 << 40} {
			if got, _ := lookupOutcome(t, data, tb.Root(), id); got != "absent" {
				t.Errorf("Get(%d) = %s, want absent", id, got)
			}
		}
	})
}

// TestLookupReportsKeyRangeViolations: after a separator key was doctored the
// scan delivers rows the search cannot reach; Get says uncertain for them (and
// agrees with the scan everywhere else).
func TestLookupReportsKeyRangeViolations(t *testing.T) {
	for _, c := range []struct {
		name   string
		idx    func(n int) int
		delta  int64
		uncert func(old int64) (int64, int64) // the rowids that must be "corrupt"
	}{
		{"first root key raised", func(int) int { return 0 }, +5, func(old int64) (int64, int64) { return old + 1, old + 5 }},
		{"first root key lowered", func(int) int { return 0 }, -5, func(old int64) (int64, int64) { return old - 4, old }},
		{"last root key raised", func(n int) int { return n - 1 }, +5, func(old int64) (int64, int64) { return old + 1, old + 5 }},
		{"last root key lowered", func(n int) int { return n - 1 }, -5, func(old int64) (int64, int64) { return old - 4, old }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, tb := bigTable(t)
			data := b.Bytes()
			root := tb.Root()
			i := c.idx(len(cellOffsets(pageAt(data, 512, root))))
			old := cellKey(data, 512, root, i)
			setCellKey(t, data, 512, root, i, old+c.delta)
			lo, hi := c.uncert(old)
			for id := lo; id <= hi; id++ {
				if got, _ := lookupOutcome(t, data, root, id); got == "absent" {
					t.Errorf("Get(%d) answers absent for a row the scan delivers", id)
				}
			}
			assertLookupAgreesWithScan(t, data, root, idRange(old-12, old+12))
		})
	}
}
