package sqlitefile_test

// GetByKey of a WITHOUT ROWID table (LX-4 Task 9, L105): builder-written trees. The
// engine-written files and the oracle comparison are in lookupkey_engine_test.go.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func txtKey(s string) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(s), Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
}

func intKey(i int64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindInt, Int: i} }

// kout is the outcome of one GetByKey, as a word the tests compare.
type kout struct {
	what string // row, absent, corrupt, undecided, badkey
	row  sqlitefile.Row
	err  error
}

func getKey(t testing.TB, tbl *sqlitefile.Table, key ...sqlitefile.Value) kout {
	t.Helper()
	r, ok, err := tbl.GetByKey(context.Background(), key)
	switch {
	case err == nil && ok:
		return kout{what: "row", row: r}
	case err == nil:
		return kout{what: "absent"}
	case errors.Is(err, sqlitefile.ErrCorrupt):
		return kout{what: "corrupt", err: err}
	case errors.Is(err, sqlitefile.ErrKeyUndecided):
		return kout{what: "undecided", err: err}
	case errors.Is(err, sqlitefile.ErrBadKey):
		return kout{what: "badkey", err: err}
	}
	t.Fatalf("GetByKey(%v): unexpected error %v", key, err)
	return kout{}
}

func openKeyTable(t testing.TB, data []byte, name string) (*sqlitefile.View, *sqlitefile.Table) {
	t.Helper()
	_, v := openLive(t, data, sqlitefile.Options{})
	tbl, err := v.Table(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return v, tbl
}

func agree(a, b []sqlitefile.Value) bool { return sameValues(a, b) == nil }

// keyTable is a WITHOUT ROWID table wt(k text primary key, v) with n rows whose keys are
// "k%05d" of 10, 12, 14, ...: the odd numbers are the gaps. With n = 2000 on 512-byte pages
// it is three levels deep.
func keyTable(t testing.TB, n int) (*sqlitetest.Builder, *sqlitetest.Table) {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTableWithoutRowid("wt", "create table wt(k text primary key, v) without rowid", 1)
	for i := range n {
		tb.Insert(int64(i+1), keyName(10+2*i), "x")
	}
	return b, tb
}

func keyName(n int) string { return fmt.Sprintf("k%05d", n) }

func TestGetByKeyFindsAndMissesOnBuilderTables(t *testing.T) {
	b, tb := keyTable(t, 2000)
	if tb.Depth() < 3 {
		t.Fatalf("depth %d, want at least 3", tb.Depth())
	}
	v, tbl := openKeyTable(t, b.Bytes(), "wt")
	defer v.Release()
	scan := scanTableRows(t, v, "wt")
	if len(scan.rows) != 2000 {
		t.Fatalf("%d rows", len(scan.rows))
	}
	interior := map[uint32]bool{}
	for _, pg := range tb.Interiors() {
		interior[pg] = true
	}
	hitsOnInterior := 0
	for i, want := range scan.rows {
		got := getKey(t, tbl, want.Values[0])
		if got.what != "row" {
			t.Fatalf("key %d: %s %v", i, got.what, got.err)
		}
		if !agree(got.row.Values, want.Values) || got.row.Loc.Page != want.Loc.Page || got.row.Loc.Cell != want.Loc.Cell {
			t.Fatalf("key %d: row %+v differs from the scan's %+v", i, got.row, want)
		}
		if got.row.KeyRangeViolation {
			t.Fatalf("key %d: a clean tree flags the row", i)
		}
		if interior[got.row.Loc.Page] {
			hitsOnInterior++
		}
	}
	if hitsOnInterior == 0 {
		t.Error("no key was found in an interior entry")
	}
	for _, n := range []int{0, 9, 11, 13, 1001, 4009, 4010, 4011, 99999} {
		if got := getKey(t, tbl, txtKey(keyName(n))); got.what != "absent" {
			t.Errorf("absent key %d: %s %v", n, got.what, got.err)
		}
	}
	for n := 11; n < 4010; n += 2 { // every gap
		if got := getKey(t, tbl, txtKey(keyName(n))); got.what != "absent" {
			t.Fatalf("gap %d: %s %v", n, got.what, got.err)
		}
	}
	if got := getKey(t, tbl, txtKey("")); got.what != "absent" {
		t.Errorf("empty key: %s", got.what)
	}
}

// damageCase patches a depth-3 key table; the returned word names the damage.
type damageCase struct {
	name  string
	patch func(t testing.TB, tb *sqlitetest.Table, data []byte)
}

func childAt(data []byte, pgno uint32, idx int) uint32 {
	p := pageAt(data, 512, pgno)
	return binary.BigEndian.Uint32(p[cellOffsets(p)[idx]:])
}

// pathPages returns the first mid-level interior page (root cell 0's child) and the first leaf
// under it, and a leaf in the middle of the table (not the first of its parent).
func pathPages(t testing.TB, tb *sqlitetest.Table, data []byte) (mid, leaf0, leaf1 uint32) {
	t.Helper()
	mid = childAt(data, tb.Root(), 0)
	if p := pageAt(data, 512, mid); p[0] != 0x02 {
		t.Fatalf("page %d is not a mid-level interior page", mid)
	}
	return mid, childAt(data, mid, 0), childAt(data, mid, 1)
}

func damageCases() []damageCase {
	return []damageCase{
		{"leaf cells out of order", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			_, l0, _ := pathPages(t, tb, data)
			swapPointers(data, 512, l0, 1, 2)
		}},
		{"mid-level cells out of order", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			mid, _, _ := pathPages(t, tb, data)
			swapPointers(data, 512, mid, 1, 2)
		}},
		{"root cells out of order", func(_ testing.TB, tb *sqlitetest.Table, data []byte) {
			swapPointers(data, 512, tb.Root(), 0, 1)
		}},
		{"an empty leaf", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			_, l0, _ := pathPages(t, tb, data)
			p := pageAt(data, 512, l0)
			p[3], p[4] = 0, 0
		}},
		{"an unreadable leaf", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			_, l0, _ := pathPages(t, tb, data)
			pageAt(data, 512, l0)[0] = 0x07
		}},
		{"a mid-level entry above the key range its ancestors give it", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			mid, _, _ := pathPages(t, tb, data)
			p := pageAt(data, 512, mid)
			last := len(cellOffsets(p)) - 1
			p[keyByteOffset(p, last)+1]++ // the ten-thousands digit: +10000, past root cell 0
		}},
		{"a root key below the entries of its left subtree", func(_ testing.TB, tb *sqlitetest.Table, data []byte) {
			p := pageAt(data, 512, tb.Root())
			p[keyByteOffset(p, 0)+2] = '0' // the thousands digit of the first separator
			p[keyByteOffset(p, 0)+3] = '0'
		}},
		{"the first entry of a sibling leaf below the bound", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			_, _, l1 := pathPages(t, tb, data)
			p := pageAt(data, 512, l1)
			p[keyByteOffset(p, 0)+2] = '0'
			p[keyByteOffset(p, 0)+3] = '0'
			p[keyByteOffset(p, 0)+4] = '0'
		}},
		{"a child pointer to nowhere", func(t testing.TB, tb *sqlitetest.Table, data []byte) {
			mid, _, _ := pathPages(t, tb, data)
			setChild(data, 512, mid, 1, 0)
		}},
	}
}

// TestGetByKeyAbsentIsNeverAnsweredOnADamagedPath mirrors TestLookupNeverAnswersAbsentOnDamage:
// whatever the damage, a key the scan delivers unflagged is found or the answer is a
// *CorruptError ("the row may exist"), never (false, nil); and the damage is reported somewhere.
func TestGetByKeyAbsentIsNeverAnsweredOnADamagedPath(t *testing.T) {
	for _, c := range damageCases() {
		t.Run(c.name, func(t *testing.T) {
			b, tb := keyTable(t, 2000)
			data := b.Bytes()
			c.patch(t, tb, data)
			v, tbl := openKeyTable(t, data, "wt")
			defer v.Release()
			scan := scanTableRows(t, v, "wt")
			corrupt, row := 0, 0
			for i, r := range scan.rows {
				if r.KeyRangeViolation || (i+1 < len(scan.rows) && scan.rows[i+1].KeyRangeViolation) {
					continue
				}
				switch got := getKey(t, tbl, r.Values[0]); got.what {
				case "absent":
					t.Fatalf("row %d (%q) delivered unflagged by the scan: a clean absent", i, r.Values[0].Bytes)
				case "corrupt":
					corrupt++
				case "row":
					row++
					if !agree(got.row.Values, r.Values) {
						t.Fatalf("row %d: found %+v, the scan delivered %+v", i, got.row.Values, r.Values)
					}
				default:
					t.Fatalf("row %d: %s %v", i, got.what, got.err)
				}
			}
			if corrupt == 0 && !scan.loss.Any() && len(flaggedIdx(scan.rows)) == 0 {
				t.Fatal("the patch damaged nothing the scan can see")
			}
			// the gaps: never a wrong row, and where the damaged page lies on the path to the gap,
			// never a clean absent either
			for n := 11; n < 4010; n += 2 {
				got := getKey(t, tbl, txtKey(keyName(n)))
				if got.what == "row" {
					t.Fatalf("gap %d found a row", n)
				}
				if n <= pathGapsTo[c.name] && got.what != "corrupt" {
					t.Fatalf("gap %d: %s, want corrupt (the damaged page is on its path)", n, got.what)
				}
			}
			t.Logf("%d found, %d corrupt, %d flagged by the scan, scan loss %+v", row, corrupt, len(flaggedIdx(scan.rows)), scan.loss)
		})
	}
}

// A gap whose only evidence of absence is the neighbouring leaf: the leaf next to the one the
// search ends in holds an entry below the bound (U-rule checkSibling). Without the sibling check
// the answer would be a clean absent.
func TestGetByKeyGapNextToABrokenSiblingIsCorrupt(t *testing.T) {
	b, tb := keyTable(t, 2000)
	data := b.Bytes()
	_, l0, l1 := pathPages(t, tb, data)
	p0, p1 := pageAt(data, 512, l0), pageAt(data, 512, l1)
	lastOff := keyByteOffset(p0, len(cellOffsets(p0))-1)
	lastKey := string(p0[lastOff : lastOff+6])
	p1[keyByteOffset(p1, 0)+2] = '0' // the first entry of the next leaf drops far below the bound
	p1[keyByteOffset(p1, 0)+3] = '0'
	p1[keyByteOffset(p1, 0)+4] = '0'
	v, tbl := openKeyTable(t, data, "wt")
	defer v.Release()
	var n int
	if _, err := fmt.Sscanf(lastKey, "k%d", &n); err != nil {
		t.Fatal(err)
	}
	if got := getKey(t, tbl, txtKey(keyName(n+1))); got.what != "corrupt" {
		t.Errorf("the gap after the last entry of a leaf whose neighbour is broken: %s, want corrupt", got.what)
	}
}

func TestGetByKeyUndecidableCollationIsNotAbsent(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTableWithoutRowid("w", "create table w(a int, b text collate rot13, v, primary key(a, b)) without rowid", 2)
	for i := range 60 {
		tb.Insert(int64(i+1), int64(i/3), fmt.Sprintf("b%d", i%3), "x")
	}
	v, tbl := openKeyTable(t, b.Bytes(), "w")
	defer v.Release()
	// stored entries share their first column, so the order of the page itself needs the collation:
	// nothing about absence can be proven, not even for a key that differs in the first column
	for _, a := range []int64{0, 7, 19, 99} {
		for _, s := range []string{"b0", "b1", "zz"} {
			got := getKey(t, tbl, intKey(a), txtKey(s))
			if got.what != "undecided" {
				t.Errorf("key (%d, %q) under an unreproducible collation: %s, want undecided", a, s, got.what)
			}
		}
	}
	if _, _, err := tbl.GetByKey(context.Background(), []sqlitefile.Value{intKey(7), txtKey("b1")}); errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Error("an undecided lookup is not corruption")
	}

	// stored entries all differ in their first column: a key that differs there is decided
	b2 := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb2 := b2.CreateTableWithoutRowid("w", "create table w(a int, b text collate rot13, v, primary key(a, b)) without rowid", 2)
	for i := range 60 {
		tb2.Insert(int64(i+1), int64(i*2), "b", "x")
	}
	v2, tbl2 := openKeyTable(t, b2.Bytes(), "w")
	defer v2.Release()
	for _, a := range []int64{-1, 1, 33, 999} {
		if got := getKey(t, tbl2, intKey(a), txtKey("b")); got.what != "absent" {
			t.Errorf("(%d, b) differs from every stored first column: %s %v, want absent", a, got.what, got.err)
		}
	}
	for _, a := range []int64{0, 40, 118} {
		if got := getKey(t, tbl2, intKey(a), txtKey("b")); got.what != "undecided" {
			t.Errorf("(%d, b) agrees on the first column and needs the collation: %s, want undecided", a, got.what)
		}
	}
}

func TestGetByKeyRefusesWrongShapes(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	wo := b.CreateTableWithoutRowid("w", "create table w(a int, b int, v, primary key(a, b)) without rowid", 2)
	for i := range 20 {
		wo.Insert(int64(i+1), int64(i), int64(i), "x")
	}
	rt := b.CreateTable("r", "create table r(a, b)")
	rt.Insert(1, 1, 2)
	b.AddSchemaRow("table", "broken", "broken", int64(wo.Root()), "create table broken(((")
	v, _ := openKeyTable(t, b.Bytes(), "w")
	defer v.Release()
	get := func(name string, key ...sqlitefile.Value) kout {
		tbl, err := v.Table(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		return getKey(t, tbl, key...)
	}
	nan := sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: nanValue()}
	for _, c := range []struct {
		name string
		tbl  string
		key  []sqlitefile.Value
	}{
		{"too few values", "w", []sqlitefile.Value{intKey(1)}},
		{"too many values", "w", []sqlitefile.Value{intKey(1), intKey(1), intKey(1)}},
		{"no values", "w", nil},
		{"a NULL component", "w", []sqlitefile.Value{intKey(1), {Kind: sqlitefile.KindNull}}},
		{"a NaN component", "w", []sqlitefile.Value{intKey(1), nan}},
		{"a rowid table", "r", []sqlitefile.Value{intKey(1)}},
		{"a table whose definition is not parsed", "broken", []sqlitefile.Value{intKey(1), intKey(1)}},
	} {
		if got := get(c.tbl, c.key...); got.what != "badkey" {
			t.Errorf("%s: %s %v, want ErrBadKey", c.name, got.what, got.err)
		}
	}
	if got := get("w", intKey(3), intKey(3)); got.what != "row" {
		t.Errorf("a well-shaped key: %s %v", got.what, got.err)
	}
	rtbl, err := v.Table(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rtbl.Get(context.Background(), 3); !errors.Is(err, sqlitefile.ErrWithoutRowid) {
		t.Errorf("Get of a WITHOUT ROWID table: %v, want ErrWithoutRowid", err)
	}
}

func nanValue() float64 {
	var z float64
	return z / z
}

func TestGetByKeyHostile(t *testing.T) {
	t.Run("a page cycle", func(t *testing.T) {
		b, tb := keyTable(t, 2000)
		data := b.Bytes()
		mid, _, _ := pathPages(t, tb, data)
		setChild(data, 512, mid, 1, tb.Root()) // a child that is the root
		v, tbl := openKeyTable(t, data, "wt")
		defer v.Release()
		bad := 0
		for i := range 2000 { // every key the file was built with, the scan's losses included
			switch got := getKey(t, tbl, txtKey(keyName(10+2*i))); got.what {
			case "absent":
				t.Fatalf("%s: a clean absent through a cycle", keyName(10+2*i))
			case "corrupt":
				bad++
			}
		}
		if bad == 0 {
			t.Error("no lookup met the cycle")
		}
	})
	t.Run("a tree at the depth cap and one level past it", func(t *testing.T) {
		order := sqlitefile.KeyOrder{Cols: []sqlitefile.KeyColumn{{}}, Unique: true}
		look := func(data []byte, k int64) (string, sqlitefile.Row) {
			_, v := openLive(t, data, sqlitefile.Options{})
			defer v.Release()
			r, ok, err := v.LookupKey(context.Background(), 2, order, []sqlitefile.Value{intKey(k)})
			switch {
			case err != nil && errors.Is(err, sqlitefile.ErrCorrupt):
				return "corrupt", r
			case err != nil:
				t.Fatalf("LookupKey(%d): %v", k, err)
			case ok:
				return "row", r
			}
			return "absent", r
		}
		deep := chainIndex(t, 31) // the deepest leaf is at depth 32, the reader's cap
		if got, r := look(deep, 63); got != "row" || !r.BeyondEngineDepth {
			t.Errorf("the deepest entry: %s, beyond-depth %v", got, r.BeyondEngineDepth)
		}
		if got, _ := look(deep, 64); got != "corrupt" {
			t.Errorf("a miss at the end of a tree the engine calls corrupt: %s", got)
		}
		if got, _ := look(deep, 2); got != "row" {
			t.Errorf("an interior entry: %s", got)
		}
		over := chainIndex(t, 32) // one level past the cap
		if got, _ := look(over, 65); got != "corrupt" {
			t.Errorf("an entry below the depth cap: %s, want corrupt (the page is skipped)", got)
		}
		if got, _ := look(over, 1); got != "row" {
			t.Errorf("an entry above the cap: %s", got)
		}
	})
	t.Run("a 4 MiB key is compared within the budget and the charge returns to zero", func(t *testing.T) {
		prefix := strings.Repeat("a", 4<<20-64)
		b := bigKeyTable(prefix+"b", prefix+"c", prefix+"d")
		rb := newRecBudget(256 << 20)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
		tbl, err := v.Table(context.Background(), "w")
		if err != nil {
			t.Fatal(err)
		}
		if got := getKey(t, tbl, txtKey(prefix+"c")); got.what != "row" {
			t.Errorf("the 4 MiB key: %s %v", got.what, got.err)
		}
		if got := getKey(t, tbl, txtKey(prefix+"cc")); got.what != "absent" {
			t.Errorf("a 4 MiB key that is not there: %s %v", got.what, got.err)
		}
		if rb.peak > 160<<20 {
			t.Errorf("peak charge %d MiB for a lookup among three 4 MiB keys", rb.peak>>20)
		}
		v.Release()
		rb.check(t)
	})
	t.Run("a cancelled context is the context error", func(t *testing.T) {
		b, _ := keyTable(t, 100)
		v, tbl := openKeyTable(t, b.Bytes(), "wt")
		defer v.Release()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := tbl.GetByKey(ctx, []sqlitefile.Value{txtKey(keyName(10))}); !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled context: %v", err)
		}
	})
	t.Run("a budget that refuses the clones is a typed error with nothing held", func(t *testing.T) {
		prefix := strings.Repeat("a", 4<<20-64)
		b := bigKeyTable(prefix+"b", prefix+"c", prefix+"d")
		rb := newRecBudget(3 << 20)
		_, v := openLive(t, b.Bytes(), sqlitefile.Options{Budget: rb})
		tbl, err := v.Table(context.Background(), "w")
		if err != nil {
			t.Fatal(err)
		}
		_, ok, err := tbl.GetByKey(context.Background(), []sqlitefile.Value{txtKey(prefix + "c")})
		if ok || !errors.Is(err, sqlitefile.ErrBudget) {
			t.Errorf("a refused charge: found %v, err %v, want ErrBudget", ok, err)
		}
		v.Release()
		rb.check(t)
	})
}

// pathGapsTo: for each damage case, the gap keys up to this number are searched through the
// damaged page, so a clean "absent" for them would be wrong.
var pathGapsTo = map[string]int{
	"leaf cells out of order":      40,
	"mid-level cells out of order": 200,
	"root cells out of order":      200,
	"an empty leaf":                40,
	"an unreadable leaf":           40,
	"a mid-level entry above the key range its ancestors give it": 200,
	"a root key below the entries of its left subtree":            79,
}
