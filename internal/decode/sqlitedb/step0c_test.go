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

// B44: a failed index build is remembered for the life of the context.
func TestFailedIndexBuildIsCachedPerColumn(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, n integer)")
		for i := int64(1); i <= 500; i++ {
			g.Insert(i, i, i)
		}
	})
	b := bigBudget()
	d := openBytes(t, data, nil, nil, b)
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	if _, err := c.Match("g", "id", sqlitedb.LiteralIntKey(1)); err != nil { // warm: resolves the table
		t.Fatal(err)
	}
	b.limit = b.used + 2000 // room for a few index entries only
	_, err1 := c.Match("g", "n", sqlitedb.LiteralIntKey(1))
	if !errors.Is(err1, parse.ErrBudget) {
		t.Fatalf("first Match = %v, want parse.ErrBudget", err1)
	}
	scans := d.Stats().Scans
	b.limit = 1 << 40 // even with room again, the failure stays
	for range 5 {
		_, err := c.Match("g", "n", sqlitedb.LiteralIntKey(2))
		if err == nil || err.Error() != err1.Error() || !errors.Is(err, parse.ErrBudget) {
			t.Fatalf("repeat Match = %v, want the first error %v", err, err1)
		}
	}
	if got := d.Stats().Scans; got != scans {
		t.Errorf("Scans %d -> %d: the failed build was retried", scans, got)
	}
}

// B43: two different stored UTF-16 values never match through U+FFFD.
func TestUtf16LoneSurrogatesNeverMatchEachOther(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, k text)")
		g.InsertRaw(1, []uint64{0, 21}, []byte{'a', 0x00, 0x00, 0xD8}) // "a" + lone high surrogate D800
		g.InsertRaw(2, []uint64{0, 21}, []byte{'a', 0x00, 0x01, 0xD8}) // "a" + lone high surrogate D801
		g.Insert(3, 3, "a�")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := tb.Index(t.Context(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	ids, _, err := ix.Rowids(sqlitedb.LiteralTextKey([]byte("a�")))
	if err != nil {
		t.Fatalf("a hit is returned normally: %v", err)
	}
	if len(ids) != 1 || ids[0] != 3 {
		t.Errorf("ids = %v; only the exact row 3 may match", ids)
	}
	if ix.Unkeyed() != 2 {
		t.Errorf("Unkeyed = %d, want the two undecodable rows", ix.Unkeyed())
	}
}

// B41 (a): a probe key that cannot be compared is an error, never an empty
// answer. NULL and NaN (which the engine matches with nothing) are decided.
func TestRowidsUndecidableProbeIsAnError(t *testing.T) {
	tb, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a text collate nocase)", func(tt *sqlitetest.Table) {
		tt.Insert(1, "abc")
	})
	ix, err := tb.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	bad := []parse.JoinKey{
		{Kind: parse.JoinText, S: "\xff\xfe"},   // invalid UTF-8 under NOCASE
		{Kind: parse.JoinKeyKind(99), S: "abc"}, // a kind this layer does not define
	}
	for _, k := range bad {
		if ids, _, err := ix.Rowids(k); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ids != nil {
			t.Errorf("Rowids(%+v) = %v, %v; want ErrKeyUndecidable", k, ids, err)
		}
	}
	nan := parse.JoinKey{Kind: parse.JoinFloat, F: math.Float64bits(math.NaN())}
	if ids, _, err := ix.Rowids(nan); err != nil || len(ids) != 0 {
		t.Errorf("Rowids(NaN) = %v, %v; the engine reads NaN as NULL, which matches nothing", ids, err)
	}
}

// B41 (b): a miss is not proof of absence while target rows are unkeyed for
// want of a decidable key; a hit is returned normally; NULL rows are decided.
func TestRowidsMissWithUndecidedTargetRowsIsAnError(t *testing.T) {
	// Row 1 holds an odd-length UTF-16 text (undecodable), row 2 "abc", row 3 NULL.
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, k text)")
		g.InsertRaw(1, []uint64{0, 19}, []byte{0x41, 0x00, 0x42})
		g.Insert(2, 2, "abc")
		g.Insert(3, 3, nil)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "g", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := tb.Index(t.Context(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _, err := ix.Rowids(sqlitedb.LiteralTextKey([]byte("abc"))); err != nil || !slices.Equal(ids, []int64{2}) {
		t.Errorf("hit = %v, %v", ids, err)
	}
	if ids, _, err := ix.Rowids(sqlitedb.LiteralTextKey([]byte("zzz"))); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ids != nil {
		t.Errorf("miss = %v, %v; want ErrKeyUndecidable", ids, err)
	}
	if ix.Unkeyed() != 2 {
		t.Errorf("Unkeyed = %d, want 2 (undecodable and NULL)", ix.Unkeyed())
	}

	// With only a NULL row unkeyed the miss is provable.
	tb2, _, _ := joinTable(t, sqlitetest.Options{}, "create table t(a)", func(tt *sqlitetest.Table) {
		tt.Insert(1, 1)
		tt.Insert(2, nil)
		tt.Insert(3, math.NaN())
	})
	ix2, err := tb2.Index(t.Context(), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _, err := ix2.Rowids(sqlitedb.LiteralIntKey(9)); err != nil || len(ids) != 0 {
		t.Errorf("miss next to NULL and NaN rows = %v, %v; want a clean empty answer", ids, err)
	}
}

func TestContextMatchPropagatesUndecidable(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024, Encoding: 2}, func(b *sqlitetest.Builder) {
		g := b.CreateTable("g", "create table g(id integer primary key, k text)")
		g.InsertRaw(1, []uint64{0, 19}, []byte{0x41, 0x00, 0x42})
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	c := sqlitedb.NewContext(t.Context(), d, &parse.Input{}, nil)
	t.Cleanup(c.Close)
	if rows, err := c.Match("g", "k", sqlitedb.LiteralTextKey([]byte("x"))); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || rows != nil {
		t.Errorf("Match = %v, %v; want ErrKeyUndecidable", rows, err)
	}
}

// B45: KeyOf says "no key" (and no error) only for NULL and NaN, the values the
// engine never matches; the undecidable states are pinned by index_test.go.
func TestKeyOfNullAndNaNHaveNoKeyAndNoError(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		tt := b.CreateTable("t", "create table t(a, b)")
		tt.Insert(1, nil, 5)
		tt.Insert(2, math.NaN(), 5)
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		k, ok, err := sqlitedb.KeyOf(r, 0)
		if ok || err != nil || k != (parse.JoinKey{}) {
			t.Errorf("NULL or NaN: %+v, %v, %v; want no key and no error", k, ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
