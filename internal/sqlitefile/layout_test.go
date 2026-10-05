package sqlitefile_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func layoutOf(t *testing.T, data []byte, o sqlitefile.Options) (*sqlitefile.Layout, *sqlitefile.View) {
	t.Helper()
	_, v := openLive(t, data, o)
	t.Cleanup(v.Release)
	l, err := v.Layout(context.Background())
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	return l, v
}

// TestLayoutClassifiesEveryPage: tables, an index, overflow, a freelist (dropped
// table included) and auto-vacuum: every page 1..Addressable has exactly one
// class, none is an orphan, and the owners follow the encoding 0 (none),
// 1 (the schema tree), k+2 (Schema().Objects[k]).
func TestLayoutClassifiesEveryPage(t *testing.T) {
	b, _ := freeScenario(sqlitetest.Options{PageSize: 512, AutoVacuum: 1}, 12)
	b.WritePtrmap()
	data := b.Bytes()
	l, v := layoutOf(t, data, sqlitefile.Options{})
	s, err := v.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.Addressable != l.PageCount || l.Addressable != uint32(len(data)/512) || len(l.Class) != int(l.Addressable)+1 || len(l.Owner) != len(l.Class) {
		t.Fatalf("geometry: count %d addressable %d classes %d owners %d", l.PageCount, l.Addressable, len(l.Class), len(l.Owner))
	}
	if len(l.Orphans) != 0 || l.OrphansTotal != 0 || len(l.Problems) != 0 {
		t.Errorf("orphans %v problems %v", l.Orphans, l.Problems)
	}
	if w := v.Warnings(); len(w) != 0 {
		t.Errorf("a consistent database warned: %v", w)
	}
	objIdx := map[string]uint32{}
	for k, o := range s.Objects {
		objIdx[o.Name] = uint32(k) + 2
	}
	// Page 1 is the schema tree.
	if l.Owner[1] != 1 || (l.Class[1] != sqlitefile.ClassBTreeLeaf && l.Class[1] != sqlitefile.ClassBTreeInterior) {
		t.Errorf("page 1: class %d owner %d", l.Class[1], l.Owner[1])
	}
	// The pointer-map pages.
	var wantPtr []uint32
	for pg := uint32(2); pg <= l.Addressable; pg += 103 {
		wantPtr = append(wantPtr, pg)
		if l.Class[pg] != sqlitefile.ClassPtrmap || l.Owner[pg] != 0 {
			t.Errorf("page %d: class %d owner %d, want a pointer-map page", pg, l.Class[pg], l.Owner[pg])
		}
	}
	if !slices.Equal(l.PtrmapPages, wantPtr) {
		t.Errorf("PtrmapPages %v, want %v", l.PtrmapPages, wantPtr)
	}
	// The trees, from the builder's own bookkeeping, for the table and its index.
	for name, ot := range map[string]*sqlitetest.Table{"t1": b.Object("t1"), "i1": b.Object("i1")} {
		own := objIdx[name]
		isLeaf := map[uint32]bool{}
		for _, p := range ot.Leaves() {
			isLeaf[p] = true
		}
		isInt := map[uint32]bool{}
		for _, p := range ot.Interiors() {
			isInt[p] = true
		}
		for _, p := range ot.Pages() {
			if l.Owner[p] != own {
				t.Errorf("%s page %d: owner %d, want %d", name, p, l.Owner[p], own)
			}
			want := sqlitefile.ClassOverflow
			switch {
			case isLeaf[p]:
				want = sqlitefile.ClassBTreeLeaf
			case isInt[p]:
				want = sqlitefile.ClassBTreeInterior
			}
			if l.Class[p] != want {
				t.Errorf("%s page %d: class %d, want %d", name, p, l.Class[p], want)
			}
		}
	}
	// The freelist: trunks and leaves, owner 0.
	fl, err := v.Freelist(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Trunks, fl.Trunks) || !slices.Equal(l.Leaves, fl.Leaves) || len(l.Trunks) == 0 {
		t.Errorf("trunks %v leaves %v, the freelist has %v and %v", l.Trunks, l.Leaves, fl.Trunks, fl.Leaves)
	}
	for _, p := range fl.Trunks {
		if l.Class[p] != sqlitefile.ClassFreelistTrunk || l.Owner[p] != 0 {
			t.Errorf("trunk %d: class %d owner %d", p, l.Class[p], l.Owner[p])
		}
	}
	for _, p := range fl.Leaves {
		if l.Class[p] != sqlitefile.ClassFreelistLeaf || l.Owner[p] != 0 {
			t.Errorf("free leaf %d: class %d owner %d", p, l.Class[p], l.Owner[p])
		}
	}
	for p := uint32(1); p <= l.Addressable; p++ {
		if l.Class[p] == sqlitefile.ClassOrphan {
			t.Errorf("page %d is an orphan", p)
		}
	}
}

// orphanDB frees extra pages and then clears the freelist fields of the header:
// pages that no structure refers to.
func orphanDB(t *testing.T, extra int) ([]byte, []uint32) {
	t.Helper()
	b, pages := freeScenario(sqlitetest.Options{PageSize: 512}, extra)
	data := b.Bytes()
	clear(data[32:40])
	return data, pages
}

func TestLayoutDetectsOrphanAndDoubleClaim(t *testing.T) {
	t.Run("orphan pages", func(t *testing.T) {
		data, pages := orphanDB(t, 5)
		l, _ := layoutOf(t, data, sqlitefile.Options{})
		// The dropped table's pages and the 5 extra ones are on no list any more.
		for _, p := range pages {
			if l.Class[p] != sqlitefile.ClassOrphan {
				t.Errorf("page %d: class %d, want orphan", p, l.Class[p])
			}
			if !slices.Contains(l.Orphans, p) {
				t.Errorf("page %d is missing from Orphans %v", p, l.Orphans)
			}
		}
		if l.OrphansTotal < 5 || len(l.Orphans) != l.OrphansTotal {
			t.Errorf("orphans %d total %d", len(l.Orphans), l.OrphansTotal)
		}
		if !slices.IsSorted(l.Orphans) {
			t.Error("Orphans are in page order")
		}
	})
	t.Run("a leaf with two parents", func(t *testing.T) {
		b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 400)
		data := b.Bytes()
		if tb.Depth() != 2 {
			t.Fatalf("depth %d", tb.Depth())
		}
		first := tb.Leaves()[0]
		setChild(data, 512, tb.Root(), 1, first) // the second child now is the first leaf too
		l, _ := layoutOf(t, data, sqlitefile.Options{})
		if !strings.Contains(strings.Join(l.Problems, "\n"), "claimed twice") {
			t.Errorf("no double claim reported: %v", l.Problems)
		}
		if l.Class[first] != sqlitefile.ClassBTreeLeaf {
			t.Errorf("the shared leaf: class %d", l.Class[first])
		}
	})
	t.Run("a page on the freelist and in a b-tree", func(t *testing.T) {
		b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 400)
		leaf := tb.Leaves()[1]
		extra := uint32(len(b.Bytes())/512 + 1)
		b.SetFreelist([][]uint32{{extra, leaf}})
		l, _ := layoutOf(t, b.Bytes(), sqlitefile.Options{})
		if !strings.Contains(strings.Join(l.Problems, "\n"), "claimed twice") {
			t.Errorf("no double claim reported: %v", l.Problems)
		}
		if l.Class[leaf] != sqlitefile.ClassBTreeLeaf {
			t.Errorf("the page keeps the class of the b-tree that was met first: %d", l.Class[leaf])
		}
	})
	t.Run("an overflow page shared by two rows", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		tb := b.CreateTable("t", "create table t(a)")
		tb.Insert(1, bytes.Repeat([]byte{1}, 2000))
		tb.Insert(2, bytes.Repeat([]byte{2}, 2000))
		data := b.Bytes()
		c1, c2 := tb.Overflow(1), tb.Overflow(2)
		putAt(data, 512, c2[0], 0, c1[1])
		l, _ := layoutOf(t, data, sqlitefile.Options{})
		if !strings.Contains(strings.Join(l.Problems, "\n"), "claimed twice") {
			t.Errorf("no double claim reported: %v", l.Problems)
		}
	})
}

// capBudget refuses a charge that would take the outstanding total above limit
// and remembers the peak.
type capBudget struct {
	limit, used, peak int64
}

func (b *capBudget) Alloc(n int64) error {
	if b.used+n > b.limit {
		return fmt.Errorf("%w: over %d", sqlitefile.ErrBudget, b.limit)
	}
	b.used += n
	b.peak = max(b.peak, b.used)
	return nil
}
func (b *capBudget) Free(n int64) { b.used -= n }

func TestLayoutBudgetAndCap(t *testing.T) {
	t.Run("MaxOrphans cuts the list, OrphansTotal keeps the count", func(t *testing.T) {
		data, _ := orphanDB(t, 20)
		l, _ := layoutOf(t, data, sqlitefile.Options{Limits: sqlitefile.Limits{MaxOrphans: 3}})
		if len(l.Orphans) != 3 || l.OrphansTotal < 20 {
			t.Errorf("orphans %d total %d", len(l.Orphans), l.OrphansTotal)
		}
		all, _ := layoutOf(t, data, sqlitefile.Options{})
		if !slices.Equal(l.Orphans, all.Orphans[:3]) || l.OrphansTotal != all.OrphansTotal {
			t.Errorf("the cut list must be the first three in page order: %v of %v", l.Orphans, all.Orphans)
		}
	})
	t.Run("the arrays are charged to the budget", func(t *testing.T) {
		b, _ := freeScenario(sqlitetest.Options{PageSize: 512}, 10)
		data := b.Bytes()
		pages := int64(len(data) / 512)
		bud := &capBudget{limit: 1 << 30}
		_, v := openLive(t, data, sqlitefile.Options{Budget: bud})
		bud.peak = 0
		if _, err := v.Layout(context.Background()); err != nil {
			t.Fatal(err)
		}
		if bud.peak < 5*pages {
			t.Errorf("peak charge %d for %d pages, want at least 5 bytes a page", bud.peak, pages)
		}
		v.Release()
		if bud.used != 0 {
			t.Errorf("%d bytes still charged after Release", bud.used)
		}
	})
	t.Run("a budget below the arrays refuses", func(t *testing.T) {
		b, _ := freeScenario(sqlitetest.Options{PageSize: 512}, 10)
		data := b.Bytes()
		bud := &capBudget{limit: 1 << 30}
		_, v := openLive(t, data, sqlitefile.Options{Budget: bud})
		defer v.Release()
		bud.limit = bud.used + 5*int64(len(data)/512) - 1 // not enough for the arrays
		if _, err := v.Layout(context.Background()); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Errorf("err %v, want ErrBudget", err)
		}
	})
}

func TestPtrmapMismatchWarns(t *testing.T) {
	b, tb, _ := ptrmapDB(t)
	leaf := tb.Leaves()[0]
	b.SetPtrmapEntry(leaf, 5, 77) // the wrong parent
	ov := tb.Overflow(200)
	b.SetPtrmapEntry(ov[1], 3, 4) // the wrong type
	l, v := layoutOf(t, b.Bytes(), sqlitefile.Options{})
	n := 0
	for _, w := range v.Warnings() {
		if w.Code == sqlitefile.WarnPtrmapMismatch {
			n++
			if w.Page != leaf && w.Page != ov[1] {
				t.Errorf("warning for page %d", w.Page)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d ptrmap-mismatch warnings, want 2: %v", n, v.Warnings())
	}
	if len(l.Problems) != 0 {
		t.Errorf("a pointer-map mismatch is a warning, not a double claim: %v", l.Problems)
	}
}

// sparseDB synthesizes a 1.2 GB database on demand: page 1 is real, every other
// page reads as zeros, and any read that touches the lock-byte page is recorded.
type sparseDB struct {
	head     []byte
	size     int64
	lockFrom int64
	lockTo   int64
	reads    atomic.Int64
	lockRead atomic.Bool
}

func (s *sparseDB) ReadAt(p []byte, off int64) (int, error) {
	s.reads.Add(1)
	if off < s.lockTo && off+int64(len(p)) > s.lockFrom {
		s.lockRead.Store(true)
	}
	if off >= s.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), s.size-off)
	clear(p[:n])
	if off < int64(len(s.head)) {
		copy(p[:n], s.head[off:])
	}
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func TestLockBytePageNeverParsed(t *testing.T) {
	const ps, pages = 4096, 300000
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b.SetHeaderPages(pages, true)
	lock := sqlitefile.LockBytePage(ps)
	if lock != 262145 {
		t.Fatalf("lock-byte page %d", lock)
	}
	s := &sparseDB{head: b.Bytes(), size: int64(pages) * ps, lockFrom: int64(lock-1) * ps, lockTo: int64(lock) * ps}
	db, err := sqlitefile.Open(s, s.size, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := db.Live()
	defer v.Release()
	l, err := v.Layout(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.LockBytePage != lock || l.Class[lock] != sqlitefile.ClassLockByte {
		t.Errorf("lock-byte page %d class %d", l.LockBytePage, l.Class[lock])
	}
	if s.lockRead.Load() {
		t.Error("the lock-byte page was read")
	}
	if got := v.Stats().PageReads; got > 4 {
		t.Errorf("%d page reads for a database whose only structure is page 1", got)
	}
	if l.Addressable != pages || len(l.Class) != pages+1 {
		t.Errorf("addressable %d, class array %d", l.Addressable, len(l.Class))
	}
	if l.OrphansTotal != pages-2 || len(l.Orphans) != sqlitefile.DefaultLimits().MaxOrphans {
		t.Errorf("orphans %d (total %d): every page but page 1 and the lock-byte page is one, the list is cut at MaxOrphans", len(l.Orphans), l.OrphansTotal)
	}
	if len(l.Problems) != 0 {
		t.Errorf("problems: %v", l.Problems)
	}
	// A header that declares far more pages than exist allocates by what exists.
	b2 := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	b2.SetHeaderPages(1<<31, true)
	l2, _ := layoutOf(t, b2.Bytes(), sqlitefile.Options{})
	if l2.PageCount != 1<<31 || l2.Addressable != 1 || len(l2.Class) != 2 {
		t.Errorf("declared %d, addressable %d, %d classes", l2.PageCount, l2.Addressable, len(l2.Class))
	}
}

// TestLayoutSurvivesMutations: single and triple byte damage anywhere in a
// database with trees, overflow, a freelist and a pointer map never panics, and
// the arrays keep their size.
func TestLayoutSurvivesMutations(t *testing.T) {
	b, _ := freeScenario(sqlitetest.Options{PageSize: 512, AutoVacuum: 1}, 30)
	b.WritePtrmap()
	orig := b.Bytes()
	rng := uint64(88172645463325252)
	next := func() uint64 {
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		return rng
	}
	for i := range 2500 {
		data := slices.Clone(orig)
		for k := 0; k < 1+i%3; k++ {
			data[int(next()%uint64(len(data)))] = byte(next())
		}
		db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
		if err != nil {
			continue
		}
		v := db.Live()
		l, err := v.Layout(context.Background())
		if err != nil {
			var pe *sqlitefile.PanicError
			if errors.As(err, &pe) {
				t.Fatalf("mutation %d: %v", i, err)
			}
			v.Release()
			continue
		}
		if len(l.Class) != int(l.Addressable)+1 || len(l.Owner) != len(l.Class) || l.OrphansTotal < len(l.Orphans) {
			t.Fatalf("mutation %d: arrays %d/%d for %d pages, orphans %d of %d", i, len(l.Class), len(l.Owner), l.Addressable, len(l.Orphans), l.OrphansTotal)
		}
		v.Release()
	}
}
