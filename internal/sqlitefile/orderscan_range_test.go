package sqlitefile_test

// The ancestor-range check of the ordered scan (LX-4 U63): an entry outside the (lo, hi)
// its ancestors' separators give its subtree is flagged, whatever its neighbours are.

import (
	"context"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// overwriteLeafKeys overwrites the keys of the last len(keys) cells of the first leaf under the
// first mid-level page with the given 6-byte keys, in cell order.
func overwriteLeafKeys(t testing.TB, tb *sqlitetest.Table, data []byte, keys ...string) {
	t.Helper()
	_, l0, _ := pathPages(t, tb, data)
	p := pageAt(data, 512, l0)
	cells := len(cellOffsets(p))
	for i, k := range keys {
		off := keyByteOffset(p, cells-len(keys)+i)
		copy(p[off:off+6], k)
	}
}

func flaggedKeys(rows []sqlitefile.Row) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.KeyRangeViolation {
			out[string(r.Values[0].Bytes)] = true
		}
	}
	return out
}

// An entry misplaced too high is delivered unflagged by a predecessor-only check (its successor is
// the one that descends); the range check flags it, and so GetByKey is not the only witness.
func TestOrderScanFlagsAnEntryMisplacedTooHigh(t *testing.T) {
	b, tb := keyTable(t, 2000)
	data := b.Bytes()
	overwriteLeafKeys(t, tb, data, "k09999")
	v, tbl := openKeyTable(t, data, "wt")
	defer v.Release()
	scan := scanTableRows(t, v, "wt")
	if len(scan.rows) != 2000 {
		t.Fatalf("%d rows", len(scan.rows))
	}
	if fl := flaggedKeys(scan.rows); !fl["k09999"] {
		t.Fatalf("the misplaced entry is not flagged; flagged: %v", fl)
	}
	// the lookup of the misplaced key is not a clean absent: the scan flags it (a lookup proves its path)
	if got := getKey(t, tbl, txtKey("k09999")); got.what == "absent" && !flaggedKeys(scan.rows)["k09999"] {
		t.Error("a clean absent for a row the scan delivers unflagged")
	}
}

func TestOrderScanFlagsAWholeRunAboveTheSeparator(t *testing.T) {
	b, tb := keyTable(t, 2000)
	data := b.Bytes()
	overwriteLeafKeys(t, tb, data, "k09997", "k09998", "k09999")
	v, _ := openKeyTable(t, data, "wt")
	defer v.Release()
	fl := flaggedKeys(scanTableRows(t, v, "wt").rows)
	for _, k := range []string{"k09997", "k09998", "k09999"} {
		if !fl[k] {
			t.Errorf("%s is not flagged; flagged: %v", k, fl)
		}
	}
}

// A leaf entry equal to the separator that bounds it repeats a primary key: flagged, though its
// predecessor is below it.
func TestOrderScanFlagsAnEntryEqualToItsSeparatorInAUniqueKey(t *testing.T) {
	b, tb := keyTable(t, 2000)
	data := b.Bytes()
	_, l0, _ := pathPages(t, tb, data)
	p := pageAt(data, 512, l0)
	cells := len(cellOffsets(p))
	off := keyByteOffset(p, cells-1)
	var sep int
	if n, err := sscanKey(string(p[off : off+6])); err != nil {
		t.Fatal(err)
	} else {
		sep = n + 2 // the separator is the next key
	}
	copy(p[off:off+6], keyName(sep))
	v, _ := openKeyTable(t, data, "wt")
	defer v.Release()
	hit := false
	for _, r := range scanTableRows(t, v, "wt").rows {
		if r.Loc.Page == l0 && r.KeyRangeViolation {
			hit = true
		}
	}
	if !hit {
		t.Errorf("the leaf entry equal to its separator is not flagged")
	}
}

func TestOrderScanUndecidedBoundIsCountedNotFlagged(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTableWithoutRowid("w", "create table w(k text collate rot13 primary key, v) without rowid", 1)
	for i := range 300 {
		tb.Insert(int64(i+1), keyName(i), "x")
	}
	v, _ := openKeyTable(t, b.Bytes(), "w")
	defer v.Release()
	got := scanTableRows(t, v, "w")
	if len(got.rows) != 300 || len(flaggedIdx(got.rows)) != 0 {
		t.Fatalf("%d rows, flagged %v", len(got.rows), flaggedIdx(got.rows))
	}
	n, found := uncheckedCount(t, v)
	if !found || n < 299 {
		t.Errorf("unchecked count %d (found %v), want every entry counted", n, found)
	}
}

func TestOrderScanBoundsAreReleased(t *testing.T) {
	b, _ := keyTable(t, 2000)
	rb := newRecBudget(256 << 20)
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
	tbl, err := v.Table(context.Background(), "wt")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if _, err := tbl.RowsLoss(context.Background(), func(sqlitefile.Row) bool { n++; return n < 700 }); err != nil { // stopped mid-tree
		t.Fatal(err)
	}
	if _, err := tbl.RowsLoss(context.Background(), func(sqlitefile.Row) bool { return true }); err != nil {
		t.Fatal(err)
	}
	v.Release()
	rb.check(t)
}

// A run misplaced too LOW: only its first entry descends from its predecessor, the others are found
// by the lower bound.
func TestOrderScanFlagsAWholeRunBelowTheSeparator(t *testing.T) {
	b, tb := keyTable(t, 2000)
	data := b.Bytes()
	_, _, l1 := pathPages(t, tb, data)
	p := pageAt(data, 512, l1)
	for i, k := range []string{"k00001", "k00002", "k00003"} {
		off := keyByteOffset(p, i)
		copy(p[off:off+6], k)
	}
	v, _ := openKeyTable(t, data, "wt")
	defer v.Release()
	fl := flaggedKeys(scanTableRows(t, v, "wt").rows)
	for _, k := range []string{"k00001", "k00002", "k00003"} {
		if !fl[k] {
			t.Errorf("%s is not flagged; flagged: %v", k, fl)
		}
	}
}
