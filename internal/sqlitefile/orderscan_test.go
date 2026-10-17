package sqlitefile_test

// The key-order check of index-shaped trees (LX-4 Task 8, L105): builder-written
// trees with a doctored order. The expected flags are derived from the delivered
// keys by plain integer comparison, not from the library's comparator.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const osPS = 512

// woTable builds a WITHOUT ROWID table w(k, v) of n rows with the keys 10..10+n-1
// (one-byte integers, so a key byte can be patched in place); each cell is about
// 50 bytes so the tree has several leaves under an interior root.
func woTable(n int) (*sqlitetest.Builder, *sqlitetest.Table) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTableWithoutRowid("w", "create table w(k integer primary key, v) without rowid", 1)
	for i := range n {
		k := int64(10 + i)
		tb.Insert(k, k, strings.Repeat("x", 40))
	}
	return b, tb
}

// keyByteOffset is the offset inside page p of the first byte of the key of cell i,
// a one-byte integer stored first in the record.
func keyByteOffset(p []byte, i int) int {
	off := cellOffsets(p)[i]
	if p[0] == 0x02 {
		off += 4 // the left child pointer
	}
	_, n := vtVarint(p[off:]) // the payload length
	off += n
	return off + int(p[off]) // the record header
}

type scanned struct {
	rows []sqlitefile.Row
	loss sqlitefile.ScanLoss
}

func scanTableRows(t *testing.T, v *sqlitefile.View, name string) scanned {
	t.Helper()
	tbl, err := v.Table(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out scanned
	out.loss, err = tbl.RowsLoss(context.Background(), func(r sqlitefile.Row) bool {
		out.rows = append(out.rows, r.Clone())
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func scanIndexEntries(t *testing.T, v *sqlitefile.View, name string) []sqlitefile.Row {
	t.Helper()
	ix, err := v.Index(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []sqlitefile.Row
	if err := ix.Entries(context.Background(), func(r sqlitefile.Row) bool {
		out = append(out, r.Clone())
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func warningsOf(v *sqlitefile.View, code string) []sqlitefile.Warning {
	var out []sqlitefile.Warning
	for _, w := range v.Warnings() {
		if w.Code == code {
			out = append(out, w)
		}
	}
	return out
}

// flaggedIdx returns the positions of the rows carrying KeyRangeViolation.
func flaggedIdx(rows []sqlitefile.Row) []int {
	var out []int
	for i, r := range rows {
		if r.KeyRangeViolation {
			out = append(out, i)
		}
	}
	return out
}

// firstKeyInt is the first value of an entry, an integer.
func firstKeyInt(t *testing.T, r sqlitefile.Row) int64 {
	t.Helper()
	if r.Values[0].Kind != sqlitefile.KindInt {
		t.Fatalf("first value is not an integer: %+v", r.Values[0])
	}
	return r.Values[0].Int
}

// nonAscending returns the positions i where the integer key at i is not above the
// key at i-1: the entries a check of a unique key must flag.
func nonAscending(t *testing.T, rows []sqlitefile.Row, strict bool) []int {
	t.Helper()
	var out []int
	for i := 1; i < len(rows); i++ {
		a, b := firstKeyInt(t, rows[i-1]), firstKeyInt(t, rows[i])
		if b < a || (strict && b == a) {
			out = append(out, i)
		}
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWithoutRowidSwappedLeafCellsAreFlagged(t *testing.T) {
	b, tb := woTable(60)
	data := b.Bytes()
	leaves := tb.Leaves()
	if len(leaves) < 3 || len(cellOffsets(pageAt(data, osPS, leaves[1]))) < 5 {
		t.Fatalf("the fixture has %d leaves", len(leaves))
	}
	swapPointers(data, osPS, leaves[1], 2, 3)
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	got := scanTableRows(t, v, "w")
	if len(got.rows) != 60 {
		t.Fatalf("%d rows delivered, want all 60", len(got.rows))
	}
	want := nonAscending(t, got.rows, true)
	if len(want) != 1 {
		t.Fatalf("the swap must make exactly one entry descend, got %v", want)
	}
	if fl := flaggedIdx(got.rows); !equalInts(fl, want) {
		t.Fatalf("flagged entries %v, want %v (the entry that descends)", fl, want)
	}
	ws := warningsOf(v, sqlitefile.WarnBTreeOrder)
	if len(ws) != 1 || ws[0].Page != leaves[1] {
		t.Fatalf("btree-order warnings %+v, want one naming page %d", ws, leaves[1])
	}
	if !strings.Contains(ws[0].Msg, strconv.Itoa(got.rows[want[0]].Loc.Cell)) {
		t.Errorf("the warning %q does not name the cell index %d", ws[0].Msg, got.rows[want[0]].Loc.Cell)
	}
	if got.loss.Any() {
		t.Errorf("loss %+v: an order violation loses nothing", got.loss)
	}
}

func TestWithoutRowidWrongInteriorEntryIsFlagged(t *testing.T) {
	b, tb := woTable(60)
	data := b.Bytes()
	root := tb.Root()
	if len(tb.Interiors()) == 0 || pageAt(data, osPS, root)[0] != 0x02 {
		t.Fatal("the fixture needs an interior root")
	}
	p := pageAt(data, osPS, root)
	p[keyByteOffset(p, 0)] = 125 // above every key of the right subtree
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	got := scanTableRows(t, v, "w")
	if len(got.rows) != 60 {
		t.Fatalf("%d rows delivered, want 60", len(got.rows))
	}
	// The patched separator (key 125, the entry at position pi) bounds its right subtree from below:
	// every entry of that subtree, down to the next root entry, lies outside its range and is
	// flagged, the descending one included. The patched entry itself is inside the root's range.
	pi := -1
	for i, r := range got.rows {
		if r.Loc.Page == root && firstKeyInt(t, r) == 125 {
			pi = i
		}
	}
	if pi < 0 || pi+1 >= len(got.rows) {
		t.Fatal("the patched entry was not delivered")
	}
	var want []int
	for i := pi + 1; i < len(got.rows) && got.rows[i].Loc.Page != root; i++ {
		want = append(want, i)
	}
	if len(nonAscending(t, got.rows, true)) == 0 || len(want) < 2 {
		t.Fatal("the patch must make a whole subtree lie below its separator")
	}
	if fl := flaggedIdx(got.rows); !equalInts(fl, want) {
		t.Fatalf("flagged entries %v, want %v", fl, want)
	}
	if len(warningsOf(v, sqlitefile.WarnBTreeOrder)) == 0 {
		t.Error("no btree-order warning")
	}
}

func TestIndexEntriesOutOfOrderAreFlagged(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 60; i++ {
		tb.Insert(i, i, fmt.Sprintf("key-%04d-%s", i, strings.Repeat("p", 30)))
	}
	b.CreateIndex("i", "t", "create index i on t(b)", 1)
	data := b.Bytes()
	ix := b.Object("i")
	leaves := ix.Leaves()
	if len(leaves) < 2 {
		t.Fatalf("%d index leaves", len(leaves))
	}
	swapPointers(data, osPS, leaves[0], 1, 2)
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	rows := scanIndexEntries(t, v, "i")
	if len(rows) != 60 {
		t.Fatalf("%d entries delivered", len(rows))
	}
	var want []int
	for i := 1; i < len(rows); i++ {
		if string(rows[i].Values[0].Bytes) < string(rows[i-1].Values[0].Bytes) {
			want = append(want, i)
		}
	}
	if len(want) != 1 {
		t.Fatalf("expected exactly one descending entry, got %v", want)
	}
	if fl := flaggedIdx(rows); !equalInts(fl, want) {
		t.Fatalf("flagged %v, want %v", fl, want)
	}
	if ws := warningsOf(v, sqlitefile.WarnBTreeOrder); len(ws) != 1 || ws[0].Page != leaves[0] {
		t.Fatalf("btree-order warnings %+v, want one on page %d", ws, leaves[0])
	}
}

func TestWithoutRowidDuplicateKeyIsFlagged(t *testing.T) {
	b, tb := woTable(60)
	data := b.Bytes()
	leaf := tb.Leaves()[1]
	p := pageAt(data, osPS, leaf)
	p[keyByteOffset(p, 3)] = p[keyByteOffset(p, 2)] // two equal primary keys
	_, v := openLive(t, data, sqlitefile.Options{})
	defer v.Release()
	got := scanTableRows(t, v, "w")
	if len(got.rows) != 60 {
		t.Fatalf("%d rows", len(got.rows))
	}
	want := nonAscending(t, got.rows, true)
	if len(want) != 1 {
		t.Fatalf("expected one repeated key, got %v", want)
	}
	if fl := flaggedIdx(got.rows); !equalInts(fl, want) {
		t.Fatalf("flagged %v, want %v: a repeated primary key is damage", fl, want)
	}
	if len(warningsOf(v, sqlitefile.WarnBTreeOrder)) != 1 {
		t.Errorf("warnings %+v", v.Warnings())
	}
}

// PASSES ON ARRIVAL against the stub (nothing is flagged there); it guards the
// tie rule. Mutation: treat a tie as a violation for a non-unique index.
func TestIndexTiesAreNotJudged(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTable("t", "create table t(a, b)")
	for i := int64(1); i <= 200; i++ {
		tb.Insert(i, i, i%5)
	}
	b.CreateIndex("i", "t", "create index i on t(b)", 1)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	rows := scanIndexEntries(t, v, "i")
	if len(rows) != 200 {
		t.Fatalf("%d entries", len(rows))
	}
	if fl := flaggedIdx(rows); len(fl) != 0 {
		t.Errorf("tied entries flagged: %v", fl)
	}
	if ws := warningsOf(v, sqlitefile.WarnBTreeOrder); len(ws) != 0 {
		t.Errorf("btree-order warnings on ties: %+v", ws)
	}
	if ws := warningsOf(v, sqlitefile.WarnKeyOrderUnchecked); len(ws) != 0 {
		t.Errorf("key-order-unchecked on a decidable index: %+v", ws)
	}
}

// uncheckedCount parses the count of the one key-order-unchecked warning.
func uncheckedCount(t *testing.T, v *sqlitefile.View) (n int, found bool) {
	t.Helper()
	ws := warningsOf(v, sqlitefile.WarnKeyOrderUnchecked)
	if len(ws) == 0 {
		return 0, false
	}
	if len(ws) != 1 {
		t.Fatalf("%d key-order-unchecked warnings, want one per scan: %+v", len(ws), ws)
	}
	if _, err := fmt.Sscanf(ws[0].Msg, "%d entries", &n); err != nil {
		t.Fatalf("warning %q does not start with the count: %v", ws[0].Msg, err)
	}
	return n, true
}

func TestOrderCheckUnsupportedCollationIsCountedNotSilent(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTableWithoutRowid("w", "create table w(k text collate foo primary key, v) without rowid", 1)
	for i := range 40 {
		k := fmt.Sprintf("k%03d", i)
		tb.Insert(int64(i), k, strings.Repeat("v", 30))
	}
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	got := scanTableRows(t, v, "w")
	if len(got.rows) != 40 {
		t.Fatalf("%d rows", len(got.rows))
	}
	if fl := flaggedIdx(got.rows); len(fl) != 0 {
		t.Errorf("rows flagged although the order is undecidable: %v", fl)
	}
	if ws := warningsOf(v, sqlitefile.WarnBTreeOrder); len(ws) != 0 {
		t.Errorf("btree-order warnings for an undecidable order: %+v", ws)
	}
	if n, ok := uncheckedCount(t, v); !ok || n != 40 {
		t.Errorf("unchecked count %d (found %v), want 40: every entry (the first meets a bound)", n, ok)
	}
}

func TestOrderCheckShortRecordsAndUnreadValuesAreCounted(t *testing.T) {
	// A record shorter than the two-column key: the table says (a, b) but the rows
	// hold one value (the builder sorts by the first).
	b := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb := b.CreateTableWithoutRowid("s", "create table s(a, b, primary key(a, b)) without rowid", 1)
	for i := range 30 {
		tb.Insert(int64(i), int64(100+i))
	}
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	got := scanTableRows(t, v, "s")
	if len(got.rows) != 30 || len(flaggedIdx(got.rows)) != 0 {
		t.Fatalf("%d rows, flagged %v", len(got.rows), flaggedIdx(got.rows))
	}
	if n, ok := uncheckedCount(t, v); !ok || n != 29 {
		t.Errorf("short records: unchecked %d (found %v), want 29", n, ok)
	}
	v.Release()

	// An unread key value: a long text key whose overflow chain is cut.
	b2 := sqlitetest.New(sqlitetest.Options{PageSize: osPS})
	tb2 := b2.CreateTableWithoutRowid("u", "create table u(k text primary key, v) without rowid", 1)
	for i := range 5 {
		tb2.Insert(int64(i), fmt.Sprintf("%c%s", 'a'+i, strings.Repeat("z", 3000)), "v")
	}
	data := b2.Bytes()
	chain := tb2.Overflow(2)
	if len(chain) < 3 {
		t.Fatalf("chain of %d pages", len(chain))
	}
	copy(pageAt(data, osPS, chain[1])[:4], []byte{0, 0, 0, 0}) // the chain ends early
	_, v2 := openLive(t, data, sqlitefile.Options{})
	defer v2.Release()
	got2 := scanTableRows(t, v2, "u")
	if len(got2.rows) != 5 {
		t.Fatalf("%d rows", len(got2.rows))
	}
	if fl := flaggedIdx(got2.rows); len(fl) != 0 {
		t.Errorf("flagged %v although a key value is unread", fl)
	}
	n, ok := uncheckedCount(t, v2)
	if !ok || n < 2 {
		t.Errorf("unread key: unchecked %d (found %v), want at least the entry and its successor", n, ok)
	}
}

// ScanTreeOrdered applies DESC: an ascending tree read as descending has every
// entry but the first out of order.
func TestScanTreeOrderedAppliesDescAndUnique(t *testing.T) {
	b, tb := woTable(60)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	defer v.Release()
	run := func(order sqlitefile.KeyOrder) (flagged, total int) {
		_, err := v.ScanTreeOrdered(context.Background(), tb.Root(), order, func(r sqlitefile.Row) bool {
			total++
			if r.KeyRangeViolation {
				flagged++
			}
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		return flagged, total
	}
	if f, n := run(sqlitefile.KeyOrder{Cols: []sqlitefile.KeyColumn{{}}, Unique: true}); f != 0 || n != 60 {
		t.Errorf("ascending: flagged %d of %d", f, n)
	}
	if f, n := run(sqlitefile.KeyOrder{Cols: []sqlitefile.KeyColumn{{Desc: true}}}); f < 59 || n != 60 {
		t.Errorf("descending: flagged %d of %d, want 59 or 60 of 60 (the first entry can lie outside its separator too)", f, n)
	}
}

func TestOrderKeysExprColumnIsDecidedOnlyForNonText(t *testing.T) {
	cols := []sqlitefile.KeyColumn{{Expr: true}}
	if c, _, st := sqlitefile.OrderKeys([]sqlitefile.Value{evInt(1)}, []sqlitefile.Value{evInt(2)}, cols); st != sqlitefile.OrderOK || c != -1 {
		t.Errorf("ints under an expression column: %d, %v", c, st)
	}
	if _, _, st := sqlitefile.OrderKeys([]sqlitefile.Value{evText("a")}, []sqlitefile.Value{evInt(2)}, cols); st != sqlitefile.OrderUndecided {
		t.Errorf("text under an expression column: %v, want undecided", st)
	}
	if c, _, st := sqlitefile.OrderKeys([]sqlitefile.Value{{}}, []sqlitefile.Value{evInt(2)}, cols); st != sqlitefile.OrderOK || c != -1 {
		t.Errorf("NULL under an expression column: %d, %v", c, st)
	}
}
