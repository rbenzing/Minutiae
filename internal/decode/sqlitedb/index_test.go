package sqlitedb_test

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// joinTable builds a database holding the one table t (sql) and returns its
// resolved table, the DB and the budget.
func joinTable(t testing.TB, opts sqlitetest.Options, sql string, fill func(tt *sqlitetest.Table)) (*sqlitedb.Table, *sqlitedb.DB, *testBudget) {
	t.Helper()
	if opts.PageSize == 0 {
		opts.PageSize = 1024
	}
	data := newBuilderDB(t, opts, func(b *sqlitetest.Builder) { fill(b.CreateTable("t", sql)) })
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tb, d, b
}

// firstRowKey scans tb and returns KeyOf for column col of the first row.
func firstRowKey(t testing.TB, tb *sqlitedb.Table, col int) (parse.JoinKey, bool) {
	t.Helper()
	var k parse.JoinKey
	var ok bool
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		var kerr error
		k, ok, kerr = sqlitedb.KeyOf(r, col)
		if kerr != nil {
			t.Fatalf("KeyOf: %v", kerr)
		}
		return sqlitedb.ErrStop
	}); err != nil {
		t.Fatal(err)
	}
	return k, ok
}

func TestKeyOfCarriesSourceCollationAndClass(t *testing.T) {
	tb, _, _ := joinTable(t, sqlitetest.Options{},
		"create table t(a text collate nocase, b integer, c real, d blob collate rtrim, e numeric collate foo, f, g text)",
		func(tt *sqlitetest.Table) { tt.Insert(1, "Ab", 7, 1.5, []byte("x "), "foo-coll", 3, "plain") })
	want := []parse.JoinKey{
		{Kind: parse.JoinText, S: "Ab", Collation: "NOCASE", Class: parse.ClassText},
		{Kind: parse.JoinInt, I: 7, Collation: "BINARY", Class: parse.ClassNumeric},
		{Kind: parse.JoinFloat, F: math.Float64bits(1.5), Collation: "BINARY", Class: parse.ClassNumeric},
		{Kind: parse.JoinBlob, S: "x ", Collation: "RTRIM", Class: parse.ClassBlob},
		{Kind: parse.JoinText, S: "foo-coll", Collation: "foo", Class: parse.ClassNumeric},
		{Kind: parse.JoinInt, I: 3, Collation: "BINARY", Class: parse.ClassBlob},
		{Kind: parse.JoinText, S: "plain", Collation: "BINARY", Class: parse.ClassText},
	}
	for col, w := range want {
		got, ok := firstRowKey(t, tb, col)
		if !ok || got != w {
			t.Errorf("col %d: KeyOf = %+v, %v; want %+v", col, got, ok, w)
		}
	}
}

func TestKeyOfRefusesNullNaNAndUnknownStates(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b, c text, d as (b+1))")
		tt.InsertRaw(1, []uint64{0, 1, 19}, []byte{7, 0x41, 0x00, 0x42}) // NULL, 7, odd-length UTF-16
		tt.Insert(2, nil, math.NaN(), "ok")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	type probe struct {
		row         int64
		col         int
		ok          bool
		undecidable bool
	}
	probes := []probe{
		{1, 0, false, false}, // NULL: the engine matches it with nothing
		{1, 1, true, false},
		{1, 2, false, true}, // undecodable UTF-16
		{1, 3, false, true}, // virtual generated column
		{1, 9, false, true}, // no such column
		{1, -1, false, true},
		{2, 0, false, false},
		{2, 1, false, false}, // NaN: the engine reads it as NULL (TestEngineReadsAStoredNaNAsNull)
		{2, 2, true, false},
	}
	for _, p := range probes {
		seen := false
		err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
			if id, _ := r.Rowid(); id != p.row {
				return nil
			}
			seen = true
			k, ok, err := sqlitedb.KeyOf(r, p.col)
			if undec := errors.Is(err, sqlitedb.ErrKeyUndecidable); undec != p.undecidable || (err != nil && !undec) {
				t.Errorf("row %d col %d: err = %v, want undecidable %v", p.row, p.col, err, p.undecidable)
			}
			if ok != p.ok || (!ok && k != (parse.JoinKey{})) {
				t.Errorf("row %d col %d: KeyOf = %+v, %v; want ok %v", p.row, p.col, k, ok, p.ok)
			}
			return nil
		})
		if err != nil || !seen {
			t.Fatalf("scan: %v, seen %v", err, seen)
		}
	}
	if _, ok, err := sqlitedb.KeyOf(sqlitedb.Row{}, 0); ok || !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Errorf("KeyOf(zero Row) = %v, %v; want undecidable", ok, err)
	}
}

func TestLiteralKeys(t *testing.T) {
	if k := sqlitedb.LiteralIntKey(5); k != (parse.JoinKey{Kind: parse.JoinInt, I: 5}) {
		t.Errorf("int: %+v", k)
	}
	if k, ok := sqlitedb.LiteralFloatKey(1.5); !ok || k != (parse.JoinKey{Kind: parse.JoinFloat, F: math.Float64bits(1.5)}) {
		t.Errorf("float: %+v %v", k, ok)
	}
	if _, ok := sqlitedb.LiteralFloatKey(math.NaN()); ok {
		t.Error("NaN literal has a key")
	}
	if k := sqlitedb.LiteralTextKey([]byte("a")); k != (parse.JoinKey{Kind: parse.JoinText, S: "a"}) {
		t.Errorf("text: %+v", k)
	}
	if k := sqlitedb.LiteralBlobKey([]byte("a")); k != (parse.JoinKey{Kind: parse.JoinBlob, S: "a"}) {
		t.Errorf("blob: %+v", k)
	}
}

func rowidsOf(t testing.TB, ix *sqlitedb.Index, k parse.JoinKey) ([]int64, sqlitedb.JoinFlags) {
	t.Helper()
	ids, fl, err := ix.Rowids(k)
	if err != nil {
		t.Fatalf("Rowids(%+v): %v", k, err)
	}
	return ids, fl
}

func TestIndexGroupsRowidsAscending(t *testing.T) {
	tb, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		for _, id := range []int64{9, 3, 7, 1, 5} {
			tt.Insert(id, id%2) // odd rowids all hold 1
		}
	})
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralIntKey(1)); !slices.Equal(ids, []int64{1, 3, 5, 7, 9}) {
		t.Errorf("ids = %v", ids)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralIntKey(2)); len(ids) != 0 {
		t.Errorf("miss = %v", ids)
	}
	if ids, fl, err := ix.Rowids(parse.JoinKey{}); ids != nil || fl != 0 || err != nil {
		t.Errorf("JoinNone: %v %v %v", ids, fl, err)
	}
	// The result is the caller's: changing it changes nothing in the index.
	ids, _ := rowidsOf(t, ix, sqlitedb.LiteralIntKey(1))
	ids[0] = 99
	if again, _ := rowidsOf(t, ix, sqlitedb.LiteralIntKey(1)); again[0] != 1 {
		t.Error("Rowids returned the index's own slice")
	}
}

func TestIndexUsesTheTargetColumnCollation(t *testing.T) {
	nc, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a text collate nocase)", func(tt *sqlitetest.Table) {
		tt.Insert(1, "abc")
		tt.Insert(2, "ABD")
	})
	bin, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a text)", func(tt *sqlitetest.Table) {
		tt.Insert(1, "abc")
		tt.Insert(2, "ABD")
	})
	ixn, err := nc.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	ixb, err := bin.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := rowidsOf(t, ixn, sqlitedb.LiteralTextKey([]byte("ABC"))); !slices.Equal(ids, []int64{1}) {
		t.Errorf("nocase target: %v", ids)
	}
	if ids, _ := rowidsOf(t, ixn, sqlitedb.LiteralTextKey([]byte("abd"))); !slices.Equal(ids, []int64{2}) {
		t.Errorf("nocase target, stored upper: %v", ids)
	}
	if ids, _ := rowidsOf(t, ixb, sqlitedb.LiteralTextKey([]byte("ABC"))); len(ids) != 0 {
		t.Errorf("binary target found %v", ids)
	}
	if ids, _ := rowidsOf(t, ixb, sqlitedb.LiteralTextKey([]byte("abc"))); !slices.Equal(ids, []int64{1}) {
		t.Errorf("binary target exact: %v", ids)
	}
}

func TestIndexNumericEquality(t *testing.T) {
	tb, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.Insert(1, 1)
		tt.Insert(2, 1.0)
		tt.Insert(3, 1.5)
		tt.Insert(4, "1")
		tt.Insert(5, []byte("1"))
	})
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	f1, _ := sqlitedb.LiteralFloatKey(1.0)
	if ids, _ := rowidsOf(t, ix, f1); !slices.Equal(ids, []int64{1, 2}) {
		t.Errorf("1.0 probe = %v, want [1 2]", ids)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralIntKey(1)); !slices.Equal(ids, []int64{1, 2}) {
		t.Errorf("1 probe = %v, want [1 2]", ids)
	}
	f15, _ := sqlitedb.LiteralFloatKey(1.5)
	if ids, _ := rowidsOf(t, ix, f15); !slices.Equal(ids, []int64{3}) {
		t.Errorf("1.5 probe = %v", ids)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralTextKey([]byte("1"))); !slices.Equal(ids, []int64{4}) {
		t.Errorf("text probe = %v", ids)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralBlobKey([]byte("1"))); !slices.Equal(ids, []int64{5}) {
		t.Errorf("blob probe = %v", ids)
	}
}

func TestIndexDoesNotCoerceAffinity(t *testing.T) {
	src, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a text, b integer)", func(tt *sqlitetest.Table) {
		tt.Insert(1, "7", 7)
	})
	tgt, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a integer)", func(tt *sqlitetest.Table) {
		tt.Insert(1, 7)
	})
	ix, err := tgt.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := firstRowKey(t, src, 0)
	ids, fl := rowidsOf(t, ix, text)
	if len(ids) != 0 || fl&sqlitedb.JoinAffinityDiffers == 0 {
		t.Errorf("text 7 vs integer 7: ids %v flags %b; want no match, affinity flag", ids, fl)
	}
	num, _ := firstRowKey(t, src, 1)
	ids, fl = rowidsOf(t, ix, num)
	if !slices.Equal(ids, []int64{1}) || fl&sqlitedb.JoinAffinityDiffers != 0 {
		t.Errorf("integer vs integer: ids %v flags %b", ids, fl)
	}
}

func TestIndexUtf16NocaseWorks(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a text collate nocase)")
		tt.Insert(1, "Straße")
		tt.Insert(2, "ABC")
		tt.InsertRaw(3, []uint64{19}, []byte{0x41, 0x00, 0x42}) // undecodable (odd length)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralTextKey([]byte("abc"))); !slices.Equal(ids, []int64{2}) {
		t.Errorf("abc = %v", ids)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralTextKey([]byte("STRaße"))); !slices.Equal(ids, []int64{1}) {
		t.Errorf("STRaße = %v (ASCII folding only; the rest matches exactly)", ids)
	}
	// A miss is not an absence while an undecodable row exists (B41).
	if ids, _, err := ix.Rowids(sqlitedb.LiteralTextKey([]byte("STRASSE"))); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || len(ids) != 0 {
		t.Errorf("STRASSE = %v, %v; want ErrKeyUndecidable", ids, err)
	}
	if ix.Unkeyed() != 1 {
		t.Errorf("Unkeyed = %d, want 1 (the undecodable UTF-16 value)", ix.Unkeyed())
	}
}

func TestIndexSkipsNullNaNAndUnknownButCountsThem(t *testing.T) {
	tb, d, _ := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.Insert(1, 1)
		tt.Insert(2, nil)
		tt.Insert(3, math.NaN())
		tt.Insert(4, 1)
	})
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Unkeyed() != 2 || d.Stats().IndexUnkeyed != 2 {
		t.Errorf("Unkeyed %d, Stats.IndexUnkeyed %d; want 2, 2", ix.Unkeyed(), d.Stats().IndexUnkeyed)
	}
	if ids, _ := rowidsOf(t, ix, sqlitedb.LiteralIntKey(1)); !slices.Equal(ids, []int64{1, 4}) {
		t.Errorf("ids = %v", ids)
	}
}

func TestIndexCapIsAnErrorNeverAPartialIndex(t *testing.T) {
	tb, _, b := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		for i := int64(1); i <= 1000; i++ {
			tt.Insert(i, i)
		}
	})
	if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil }); err != nil { // warm: the reader keeps its pages
		t.Fatal(err)
	}
	before := b.used
	ix, err := tb.Index(t.Context(), "a", 100)
	if !errors.Is(err, sqlitedb.ErrIndexLimit) || ix != nil {
		t.Fatalf("Index(cap 100) = %v, %v", ix, err)
	}
	if b.used != before {
		t.Errorf("used %d after ErrIndexLimit, want %d", b.used, before)
	}
}

func TestIndexBudgetExhaustedIsParseErrBudget(t *testing.T) {
	tb, _, b := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		for i := int64(1); i <= 500; i++ {
			tt.Insert(i, i)
		}
	})
	if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil }); err != nil { // warm
		t.Fatal(err)
	}
	before := b.used
	b.limit = before + 2000 // room for a few entries only
	ix, err := tb.Index(t.Context(), "a", 0)
	if !errors.Is(err, parse.ErrBudget) || ix != nil {
		t.Fatalf("Index = %v, %v; want parse.ErrBudget", ix, err)
	}
	if b.used != before {
		t.Errorf("used %d after the refusal, want %d", b.used, before)
	}
}

func TestJoinsNeverAllocateBeyondTheBudget(t *testing.T) {
	tb, _, b := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		for i := int64(1); i <= 300; i++ {
			tt.Insert(i, i)
		}
	})
	if err := tb.Scan(t.Context(), func(sqlitedb.Row) error { return nil }); err != nil {
		t.Fatal(err)
	}
	base := b.used
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	cost := b.used - base
	if cost < 300*32 {
		t.Errorf("a 300-entry index charged %d bytes, want at least %d", cost, 300*32)
	}
	ix.Release()
	if b.used != base {
		t.Errorf("Release left %d bytes charged", b.used-base)
	}
	b.limit = base + cost - 1
	if ix, err := tb.Index(t.Context(), "a", 0); !errors.Is(err, parse.ErrBudget) || ix != nil {
		t.Errorf("Index under a budget one byte short = %v, %v", ix, err)
	}
	if b.used != base {
		t.Errorf("a refused index left %d bytes charged", b.used-base)
	}
}

func TestIndexOnMissingColumnOrTable(t *testing.T) {
	tb, d, _ := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) { tt.Insert(1, 1) })
	_, err := tb.Index(t.Context(), "nope", 0)
	var us *sqlitedb.UnsupportedSchemaError
	if !errors.As(err, &us) || !slices.Equal(us.Missing, []string{"nope"}) || us.Table != "t" {
		t.Errorf("missing column: %v", err)
	}
	if _, err := d.Table(t.Context(), "zzz", nil, nil); !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("missing table: %v", err)
	}
}

func TestIndexWithoutRowidTable(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTableWithoutRowid("w", "create table w(k, v, primary key(k)) without rowid", 1)
		tt.Insert(0, "a", 1)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "w", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ix, err := tb.Index(t.Context(), "v", 0); !errors.Is(err, sqlitedb.ErrWithoutRowid) || ix != nil {
		t.Errorf("Index = %v, %v", ix, err)
	}
}

func TestUnsupportedCollationRefusesTheJoin(t *testing.T) {
	tgt, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a text collate foo)", func(tt *sqlitetest.Table) { tt.Insert(1, "x") })
	_, err := tgt.Index(t.Context(), "a", 0)
	var uc *sqlitedb.UnsupportedCollationError
	if !errors.As(err, &uc) || !errors.Is(err, sqlitedb.ErrUnsupportedCollation) || uc.Table != "t" || uc.Column != "a" || uc.Collation != "foo" {
		t.Fatalf("target collation foo: %v", err)
	}
	ok, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a text)", func(tt *sqlitetest.Table) { tt.Insert(1, "x") })
	ix, err := ok.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	ids, fl, err := ix.Rowids(parse.JoinKey{Kind: parse.JoinText, S: "x", Collation: "foo"})
	if !errors.Is(err, sqlitedb.ErrUnsupportedCollation) || ids != nil || fl != 0 {
		t.Errorf("source collation foo: %v %v %v (a refusal, not a BINARY match)", ids, fl, err)
	}
}

func TestBudgetReturnsToZeroAfterIndexRelease(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(bd *sqlitetest.Builder) {
		tt := bd.CreateTable("t", "create table t(a)")
		for i := int64(1); i <= 50; i++ {
			tt.Insert(i, i%5)
		}
	})
	b := bigBudget()
	d, err := sqlitedb.Open(t.Context(), filesOf(data, nil, nil), b)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Index(t.Context(), "a", 0); err != nil {
		t.Fatal(err)
	}
	if b.used == 0 {
		t.Fatal("nothing charged")
	}
	d.Release()
	if b.used != 0 {
		t.Errorf("used = %d after DB.Release, want 0", b.used)
	}
}
