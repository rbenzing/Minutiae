package sqlitefile_test

import (
	"bytes"
	"cmp"
	"context"
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func TestScanTableMultiLevelOrderAndValues(t *testing.T) {
	const n = 5000
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, n)
	data := b.Bytes()
	if tb.Depth() < 3 {
		t.Fatalf("depth %d: the table must be multi-level", tb.Depth())
	}
	_, v := openLive(t, data, sqlitefile.Options{})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if len(rows) != n {
		t.Fatalf("%d rows, want %d", len(rows), n)
	}
	overflowRows := 0
	for i, r := range rows {
		want := int64(i + 1)
		if r.Rowid != want || !r.HasRowid {
			t.Fatalf("row %d has rowid %d (HasRowid %v)", i, r.Rowid, r.HasRowid)
		}
		if err := rowIs(r, genRow(want)); err != nil {
			t.Fatalf("rowid %d: %v", want, err)
		}
		if r.Loc.OverflowTotal > 0 {
			overflowRows++
		}
	}
	if overflowRows != n/97 {
		t.Errorf("%d rows with overflow pages, want %d", overflowRows, n/97)
	}
	if w := v.Warnings(); len(w) != 0 {
		t.Errorf("a clean tree raised warnings: %v", w)
	}
	if st := v.Stats(); st.PagesSkipped != 0 || st.CellsParsed < n {
		t.Errorf("stats %+v", st)
	}
}

// indexKey orders index entries as the engine does for these values: NULL
// first, then numbers, then text, then the rowid.
func indexCompare(a, b []any) int {
	class := func(v any) int {
		switch v.(type) {
		case nil:
			return 0
		case int64:
			return 1
		}
		return 2
	}
	for i := range a {
		ca, cb := class(a[i]), class(b[i])
		if ca != cb {
			return cmp.Compare(ca, cb)
		}
		switch x := a[i].(type) {
		case int64:
			if c := cmp.Compare(x, b[i].(int64)); c != 0 {
				return c
			}
		case string:
			if c := cmp.Compare(x, b[i].(string)); c != 0 {
				return c
			}
		}
	}
	return 0
}

func TestScanIndexInOrder(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a, b)")
	b.CreateIndex("ia", "t", "create index ia on t(a)", 0)
	const n = 700
	var want [][]any
	for i := int64(1); i <= n; i++ {
		var a any
		switch {
		case i%13 == 0:
			a = nil
		case i%5 == 0:
			a = "k" + string(rune('a'+i%7))
		default:
			a = i % 11 // duplicates
		}
		tb.Insert(i, a, i*2)
		want = append(want, []any{a, i})
	}
	slices.SortStableFunc(want, indexCompare)
	ix := b.Object("ia")
	if ix.Depth() < 2 {
		t.Fatalf("index depth %d: it needs interior entries", ix.Depth())
	}
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	rows := scanRows(t, v, ix.Root(), sqlitefile.IndexTree)
	if len(rows) != n {
		t.Fatalf("%d entries, want %d", len(rows), n)
	}
	interior := map[uint32]bool{}
	for _, p := range ix.Interiors() {
		interior[p] = true
	}
	fromInterior := 0
	for i, r := range rows {
		if r.HasRowid {
			t.Fatalf("entry %d claims a rowid", i)
		}
		if err := rowIs(r, want[i]); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if interior[r.Loc.Page] {
			fromInterior++
		}
	}
	if fromInterior == 0 {
		t.Error("no entry came from an interior page")
	}
	if w := v.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

func TestScanEmptyTree(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a)")
	b.CreateIndex("ia", "t", "create index ia on t(a)", 0)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	for _, c := range []struct {
		root uint32
		kind sqlitefile.BTreeKind
	}{{tb.Root(), sqlitefile.TableTree}, {b.Object("ia").Root(), sqlitefile.IndexTree}} {
		if rows := scanRows(t, v, c.root, c.kind); len(rows) != 0 {
			t.Errorf("root %d: %d rows from an empty tree", c.root, len(rows))
		}
	}
	if w := v.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	// An unusable root is a warning, not an error.
	if rows := scanRows(t, v, 0, sqlitefile.TableTree); len(rows) != 0 || !viewWarns(v, sqlitefile.WarnPageRange, 0) {
		t.Errorf("root 0: %d rows, warnings %v", len(rows), v.Warnings())
	}
}

func TestLookupRowidPageReads(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", stdTableSQL)
	const n = 4000
	for i := int64(1); i <= n; i++ {
		tb.Insert(i*2, genRow(i*2)...) // even rowids only: the odd ones are absent between
	}
	data := b.Bytes()
	depth := tb.Depth()
	if depth < 3 {
		t.Fatalf("depth %d", depth)
	}
	hits := []int64{2, 4, 194, 2 * n, 2 * (n / 2), 388, 2 * 97 * 5}
	for _, id := range hits {
		_, v := openLive(t, data, sqlitefile.Options{})
		row, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
		if err != nil || !ok {
			t.Fatalf("lookup %d: ok %v err %v", id, ok, err)
		}
		if row.Rowid != id || rowIs(row, genRow(id)) != nil {
			t.Errorf("lookup %d returned rowid %d %v", id, row.Rowid, rowIs(row, genRow(id)))
		}
		limit := int64(depth + len(tb.Overflow(id)))
		if got := v.Stats().PageReads; got > limit {
			t.Errorf("lookup %d read %d pages, limit depth %d + overflow %d", id, got, depth, len(tb.Overflow(id)))
		}
	}
	for _, id := range []int64{1, 3, 777, 2*n - 1, 2*n + 1, 2*n + 1000000, 0, -7, math.MinInt64, math.MaxInt64} {
		_, v := openLive(t, data, sqlitefile.Options{})
		_, ok, err := v.LookupRowid(context.Background(), tb.Root(), id)
		if err != nil || ok {
			t.Errorf("absent rowid %d: ok %v err %v", id, ok, err)
		}
		if got := v.Stats().PageReads; got > int64(depth) {
			t.Errorf("miss %d read %d pages, depth %d", id, got, depth)
		}
	}
	// Interior pages are served from the cache: a second lookup in the same
	// view reads at most the leaf (plus its overflow).
	_, v := openLive(t, data, sqlitefile.Options{})
	if _, _, err := v.LookupRowid(context.Background(), tb.Root(), 2000); err != nil {
		t.Fatal(err)
	}
	before := v.Stats()
	if _, ok, err := v.LookupRowid(context.Background(), tb.Root(), 2100); err != nil || !ok {
		t.Fatalf("second lookup: %v %v", ok, err)
	}
	after := v.Stats()
	if reads := after.PageReads - before.PageReads; reads > int64(1+len(tb.Overflow(2100))) {
		t.Errorf("second lookup read %d pages", reads)
	}
	if after.CacheHits-before.CacheHits < int64(depth-1) {
		t.Errorf("only %d cache hits for %d interior pages", after.CacheHits-before.CacheHits, depth-1)
	}
}

func TestCellLocationReproducesBytes(t *testing.T) {
	for _, o := range []sqlitetest.Options{
		{PageSize: 512}, {PageSize: 512, Reserved: 32}, {PageSize: 4096}, {PageSize: 1024, Encoding: 2}, {PageSize: 65536, Reserved: 8},
	} {
		b, tb := stdTable(t, o, 600)
		b.CreateIndex("ib", "t", "create index ib on t(b)", 1)
		data := b.Bytes()
		ps := b.PageSize()
		usable := ps - o.Reserved
		_, v := openLive(t, data, sqlitefile.Options{})
		rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		if len(rows) != 600 {
			t.Fatalf("%+v: %d rows", o, len(rows))
		}
		for _, r := range rows {
			cell, page, off := tb.CellBytes(r.Rowid)
			l := r.Loc
			if l.File != sqlitefile.FileDB || l.Page != page || l.PageOffset != int64(page-1)*int64(ps) || l.Offset != l.PageOffset+int64(off) {
				t.Fatalf("%+v rowid %d: loc %+v, builder says page %d offset %d", o, r.Rowid, l, page, off)
			}
			if l.Length != int64(len(cell)) || !bytes.Equal(data[l.Offset:l.Offset+l.Length], cell) {
				t.Fatalf("%+v rowid %d: bytes at the location differ from the cell the builder wrote", o, r.Rowid)
			}
			if l.PageOffset > l.Offset || l.Offset+l.Length > l.PageOffset+int64(usable) {
				t.Fatalf("%+v rowid %d: cell [%d,%d) outside the usable page [%d,%d)", o, r.Rowid, l.Offset, l.Offset+l.Length, l.PageOffset, l.PageOffset+int64(usable))
			}
		}
		// Index entries: inside their page, and the cell parses back to the entry.
		ix := b.Object("ib")
		pages := map[uint32]bool{}
		for _, p := range ix.Pages() {
			pages[p] = true
		}
		for _, r := range scanRows(t, v, ix.Root(), sqlitefile.IndexTree) {
			l := r.Loc
			if !pages[l.Page] || l.PageOffset != int64(l.Page-1)*int64(ps) || l.Offset < l.PageOffset || l.Offset+l.Length > l.PageOffset+int64(usable) {
				t.Fatalf("%+v index entry: loc %+v", o, l)
			}
		}
	}
	// Page 1 (the schema table): offsets are relative to the start of the page,
	// which is the start of the file.
	b, _ := stdTable(t, sqlitetest.Options{PageSize: 512}, 3)
	data := b.Bytes()
	_, v := openLive(t, data, sqlitefile.Options{})
	rows := scanRows(t, v, 1, sqlitefile.TableTree)
	if len(rows) != 1 {
		t.Fatalf("%d schema rows", len(rows))
	}
	l := rows[0].Loc
	if l.Page != 1 || l.PageOffset != 0 || l.Offset < 108 || l.Offset+l.Length > 512 {
		t.Errorf("schema row loc %+v", l)
	}
	if got := data[l.Offset : l.Offset+l.Length]; len(got) == 0 || !bytes.Contains(got, []byte("create table t")) {
		t.Errorf("the bytes at the schema row's location are not its cell: %q", got)
	}
}

func TestOverflowProvenanceRecorded(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 300)
	long := bytes.Repeat([]byte{7, 8, 9}, 2000) // about 12 overflow pages at 512
	tb.Insert(1000, "x", int64(1), "long", []byte{}, long)
	data := b.Bytes()
	chain := tb.Overflow(1000)
	if len(chain) < 10 {
		t.Fatalf("chain of %d pages", len(chain))
	}
	_, v := openLive(t, data, sqlitefile.Options{})
	row, ok, err := v.LookupRowid(context.Background(), tb.Root(), 1000)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if row.Loc.OverflowTotal != len(chain) || len(row.Loc.Overflow) != len(chain) || row.Loc.OverflowMixed {
		t.Fatalf("total %d listed %d mixed %v, chain %d", row.Loc.OverflowTotal, len(row.Loc.Overflow), row.Loc.OverflowMixed, len(chain))
	}
	for i, p := range row.Loc.Overflow {
		if p.Page != chain[i] || p.At.File != sqlitefile.FileDB || p.At.Offset != int64(chain[i]-1)*512 {
			t.Errorf("overflow part %d = %+v, builder chain page %d", i, p, chain[i])
		}
	}
	// A chain over MaxLocOverflow keeps the count and cuts the list.
	_, v = openLive(t, data, sqlitefile.Options{Limits: sqlitefile.Limits{MaxLocOverflow: 3}})
	row, ok, err = v.LookupRowid(context.Background(), tb.Root(), 1000)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if row.Loc.OverflowTotal != len(chain) || len(row.Loc.Overflow) != 3 || row.Loc.Overflow[2].Page != chain[2] {
		t.Errorf("total %d listed %d (want %d and 3)", row.Loc.OverflowTotal, len(row.Loc.Overflow), len(chain))
	}
	// A row with no overflow lists none.
	if row, _, _ := v.LookupRowid(context.Background(), tb.Root(), 5); row.Loc.OverflowTotal != 0 || row.Loc.Overflow != nil {
		t.Errorf("row 5: %+v", row.Loc)
	}
}

// TestLocOverflowMixed: a part that lies in another file or frame than the cell
// marks the row mixed (the WAL fixtures of Task 8 assert it end to end).
func TestLocOverflowMixed(t *testing.T) {
	at := sqlitefile.PageLoc{File: sqlitefile.FileDB, Offset: 1024}
	same := []sqlitefile.ChainStep{{Page: 5, At: sqlitefile.PageLoc{File: sqlitefile.FileDB, Offset: 2048}}}
	if l := sqlitefile.LocFor(at, 3, 0, sqlitefile.Cell{Offset: 10, Length: 20}, same, 8); l.OverflowMixed || l.Offset != 1034 || l.Length != 20 {
		t.Errorf("%+v", l)
	}
	for _, other := range []sqlitefile.PageLoc{
		{File: sqlitefile.FileWAL, Offset: 32, Frame: 1},
		{File: sqlitefile.FileJournal, Offset: 28, Record: 2},
	} {
		l := sqlitefile.LocFor(at, 3, 0, sqlitefile.Cell{}, []sqlitefile.ChainStep{{Page: 5, At: other}}, 8)
		if !l.OverflowMixed || l.OverflowTotal != 1 {
			t.Errorf("%+v: not mixed: %+v", other, l)
		}
	}
	walCell := sqlitefile.PageLoc{File: sqlitefile.FileWAL, Offset: 56, Frame: 2}
	l := sqlitefile.LocFor(walCell, 3, 0, sqlitefile.Cell{}, []sqlitefile.ChainStep{{Page: 5, At: sqlitefile.PageLoc{File: sqlitefile.FileWAL, Offset: 4000, Frame: 3}}}, 8)
	if !l.OverflowMixed || l.Frame != 2 {
		t.Errorf("another frame: %+v", l)
	}
}

func TestPtrmapPageno(t *testing.T) {
	// Pages 2, then every U/5+1 pages; the lock-byte page's slot moves to the next page.
	cases := []struct {
		ps, res  int
		pg, want uint32
	}{
		{512, 0, 1, 0},
		{512, 0, 2, 2},
		{512, 0, 3, 2},
		{512, 0, 104, 2},
		{512, 0, 105, 105},
		{512, 0, 106, 105},
		{512, 0, 208, 208},
		{4096, 0, 2, 2},
		{4096, 0, 821, 2},
		{4096, 0, 822, 822},
		{512, 32, 98, 2},
		{512, 32, 99, 99},
	}
	for _, c := range cases {
		if got := sqlitefile.PtrmapPageno(c.ps, c.res, c.pg); got != c.want {
			t.Errorf("ptrmapPageno(%d,%d,%d) = %d, want %d", c.ps, c.res, c.pg, got, c.want)
		}
	}
	// The map page that would be the lock-byte page moves one page on.
	lock := sqlitefile.LockBytePage(1024) // 1048577: (lock-2) is a multiple of 1024/5+1
	n := uint32(1024/5 + 1)
	if (lock-2)%n != 0 {
		t.Skipf("lock-byte page %d is not a pointer-map slot for 1024-byte pages", lock)
	}
	if got := sqlitefile.PtrmapPageno(1024, 0, lock+3); got != lock+1 {
		t.Errorf("map page covering %d = %d, want %d", lock+3, got, lock+1)
	}
}
