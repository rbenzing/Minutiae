package sqlitefile_test

// Row identity of a WITHOUT ROWID table uses the engine's key comparison
// (ruling C48): 1.0 equals 1, the declared collation applies, and a collation
// the library cannot apply makes the relation unknown, never absent. Value
// change detection stays exact: the same key with another stored kind or text
// is a changed row.

import (
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

type keyEqRow struct {
	vals []any
	want sqlitefile.Relation
	note string
}

// runKeyEq builds a WITHOUT ROWID table, writes old at v0 (ids 1..), then
// deletes them all and inserts live (ids 100..) in a later commit: every old
// row is a history row to relate to the live set.
func runKeyEq(t *testing.T, sql string, keyCols int, old [][]any, live [][]any, want []keyEqRow) {
	t.Helper()
	runKeyEqOpts(t, sqlitetest.Options{PageSize: hps}, sql, keyCols, old, live, want)
}

func runKeyEqOpts(t *testing.T, opts sqlitetest.Options, sql string, keyCols int, old [][]any, live [][]any, want []keyEqRow) {
	t.Helper()
	b := sqlitetest.New(opts)
	wr := b.CreateTableWithoutRowid("wr", sql, keyCols)
	for i, r := range old {
		wr.Insert(int64(i+1), r...)
	}
	v0 := b.Snapshot()
	db := walMode(withCount(b.Bytes(), v0.Pages()))
	for i := range old {
		wr.Delete(int64(i + 1))
	}
	for i, r := range live {
		wr.Insert(int64(100+i), r...)
	}
	w := b.NewWAL(false, 0x1000, 0x1001, 0)
	b.CommitTo(w, v0)
	_, h := openAll(t, db, w.Bytes(), nil)
	rows, st := collectRows(t, h)
	if len(rows) != len(want) || st.DuplicateOfLive != 0 {
		t.Fatalf("%d rows (want %d), stats %+v", len(rows), len(want), st)
	}
	for _, r := range rows {
		matched := false
		for _, w := range want {
			if !sameVals(t, r.Values, w.vals) {
				continue
			}
			matched = true
			if r.Relation != w.want {
				t.Errorf("row %v: relation %s, want %s (notes %v)", w.vals, r.Relation, w.want, r.Notes)
			}
			if w.note != "" && !hasNote(r, w.note) {
				t.Errorf("row %v: notes %v, want %s", w.vals, r.Notes, w.note)
			}
		}
		if !matched {
			t.Errorf("unexpected row %v", r.Values)
		}
	}
}

func sameVals(t *testing.T, got []sqlitefile.Value, want []any) bool {
	t.Helper()
	if len(got) != len(want) {
		return false
	}
	for i, w := range want {
		switch x := w.(type) {
		case string:
			if got[i].Kind != sqlitefile.KindText || txtOf(t, got[i]) != x {
				return false
			}
		case int64:
			if got[i].Kind != sqlitefile.KindInt || got[i].Int != x {
				return false
			}
		case float64:
			if got[i].Kind != sqlitefile.KindFloat || got[i].Float != x {
				return false
			}
		}
	}
	return true
}

const (
	sup = sqlitefile.RelSupersededVersion
	abs = sqlitefile.RelAbsentFromLive
	unk = sqlitefile.RelUnknown
)

// The real key 1.0 and the integer key 1 are one key: the history row is a
// changed version of the live row, not an absent one.
func TestWithoutRowidNumericKeysAreEqualAcrossKinds(t *testing.T) {
	runKeyEq(t, "create table wr(k primary key, v) without rowid", 1,
		[][]any{{1.0, "old-real"}, {int64(7), "gone"}},
		[][]any{{int64(1), "live-int"}},
		[]keyEqRow{{vals: []any{1.0, "old-real"}, want: sup}, {vals: []any{int64(7), "gone"}, want: abs}})
}

// The key kind itself changed, the rest equal: still the same row, changed.
func TestWithoutRowidSameRowWithChangedKeyKindIsNotDuplicate(t *testing.T) {
	runKeyEq(t, "create table wr(k primary key, v) without rowid", 1,
		[][]any{{1.0, "same"}},
		[][]any{{int64(1), "same"}},
		[]keyEqRow{{vals: []any{1.0, "same"}, want: sup}})
}

func TestWithoutRowidNocaseKeysAreEqual(t *testing.T) {
	runKeyEq(t, "create table wr(k text collate nocase primary key, v) without rowid", 1,
		[][]any{{"ABC", "old"}, {"zzz", "gone"}},
		[][]any{{"abc", "live"}},
		[]keyEqRow{{vals: []any{"ABC", "old"}, want: sup}, {vals: []any{"zzz", "gone"}, want: abs}})
}

func TestWithoutRowidRtrimKeysAreEqual(t *testing.T) {
	runKeyEq(t, "create table wr(k text collate rtrim primary key, v) without rowid", 1,
		[][]any{{"ab  ", "old"}, {"zz", "gone"}},
		[][]any{{"ab", "live"}},
		[]keyEqRow{{vals: []any{"ab  ", "old"}, want: sup}, {vals: []any{"zz", "gone"}, want: abs}})
}

// A collation the library cannot apply: never absent.
func TestWithoutRowidUnknownCollationIsUnknownNeverAbsent(t *testing.T) {
	runKeyEq(t, "create table wr(k text collate mycoll primary key, v) without rowid", 1,
		[][]any{{"a", "old"}, {"b", "gone"}},
		[][]any{{"a", "new"}},
		[]keyEqRow{
			{vals: []any{"a", "old"}, want: unk, note: sqlitefile.NoteLiveUncertain},
			{vals: []any{"b", "gone"}, want: unk, note: sqlitefile.NoteLiveUncertain},
		})
}

// Composite key in declared order with a descending component: 2.0 equals 2.
func TestWithoutRowidCompositeDescendingKey(t *testing.T) {
	// stored key-first: b, a, v
	runKeyEq(t, "create table wr(a, b, v, primary key (b desc, a)) without rowid", 2,
		[][]any{{2.0, int64(1), "old"}, {2.0, int64(9), "gone"}},
		[][]any{{int64(2), int64(1), "live"}},
		[]keyEqRow{{vals: []any{2.0, int64(1), "old"}, want: sup}, {vals: []any{2.0, int64(9), "gone"}, want: abs}})
}

func TestKeyDigestEquality(t *testing.T) {
	i := func(n int64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindInt, Int: n} }
	f := func(x float64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: x} }
	tx := func(s string) sqlitefile.Value {
		return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(s), Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
	}
	bl := func(s string) sqlitefile.Value {
		return sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte(s), Len: int64(len(s))}
	}
	type c struct {
		name string
		a, b sqlitefile.Value
		coll string
		same bool
	}
	cases := []c{
		{"1 == 1.0", i(1), f(1), "BINARY", true},
		{"0 == -0.0", i(0), f(math.Copysign(0, -1)), "BINARY", true},
		{"2^53+1 != 2^53 real", i(1<<53 + 1), f(1 << 53), "BINARY", false},
		{"2^53 == 2^53 real", i(1 << 53), f(1 << 53), "BINARY", true},
		{"2^63 real is not an int", i(math.MaxInt64), f(1 << 63), "BINARY", false},
		{"-2^63 real == min int", i(math.MinInt64), f(-(1 << 63)), "BINARY", true},
		{"1.5 != 1", i(1), f(1.5), "BINARY", false},
		{"text 1 != int 1", i(1), tx("1"), "BINARY", false},
		{"text != blob", tx("a"), bl("a"), "BINARY", false},
		{"binary case", tx("A"), tx("a"), "BINARY", false},
		{"nocase case", tx("Ab"), tx("aB"), "NOCASE", true},
		{"nocase ascii only", tx("É"), tx("é"), "NOCASE", false},
		{"nocase no trim", tx("a "), tx("a"), "NOCASE", false},
		{"rtrim spaces", tx("a  "), tx("a"), "RTRIM", true},
		{"rtrim is case sensitive", tx("A"), tx("a"), "RTRIM", false},
		{"rtrim only spaces", tx("a\t"), tx("a"), "RTRIM", false},
	}
	for _, tc := range cases {
		da, oka := sqlitefile.KeyDigest([]sqlitefile.Value{tc.a}, []string{tc.coll})
		db, okb := sqlitefile.KeyDigest([]sqlitefile.Value{tc.b}, []string{tc.coll})
		if !oka || !okb {
			t.Errorf("%s: not ok", tc.name)
			continue
		}
		if (da == db) != tc.same {
			t.Errorf("%s: equal=%v, want %v", tc.name, da == db, tc.same)
		}
	}
	// composite keys are position-wise
	d1, _ := sqlitefile.KeyDigest([]sqlitefile.Value{i(1), i(2)}, []string{"BINARY", "BINARY"})
	d2, _ := sqlitefile.KeyDigest([]sqlitefile.Value{i(2), i(1)}, []string{"BINARY", "BINARY"})
	if d1 == d2 {
		t.Error("(1,2) equals (2,1)")
	}
	// not decidable
	omit := sqlitefile.Value{Kind: sqlitefile.KindText, Omitted: true, Len: 3, Enc: sqlitefile.EncUTF8}
	clip := sqlitefile.Value{Kind: sqlitefile.KindText, Clipped: true, Bytes: []byte("a"), Len: 3, Enc: sqlitefile.EncUTF8}
	u16 := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte{'a', 0}, Len: 2, Enc: sqlitefile.EncUTF16LE}
	for name, v := range map[string]sqlitefile.Value{"omitted": omit, "clipped": clip, "null": {Kind: sqlitefile.KindNull}} {
		if _, ok := sqlitefile.KeyDigest([]sqlitefile.Value{v}, []string{"BINARY"}); ok {
			t.Errorf("%s key is decidable", name)
		}
	}
	if _, ok := sqlitefile.KeyDigest([]sqlitefile.Value{u16}, []string{"NOCASE"}); ok {
		t.Error("UTF-16 NOCASE key is decidable")
	}
	if _, ok := sqlitefile.KeyDigest([]sqlitefile.Value{u16}, []string{"BINARY"}); !ok {
		t.Error("UTF-16 BINARY key is not decidable")
	}
}

// A collation written in the table-level key clause governs the key (ruling
// C53): it is the index's collation and overrides the column's own.
func TestWithoutRowidKeyClauseNocaseKeysAreEqual(t *testing.T) {
	runKeyEq(t, "create table wr(k text, v, primary key (k collate nocase)) without rowid", 1,
		[][]any{{"ABC", "old"}, {"zzz", "gone"}},
		[][]any{{"abc", "live"}},
		[]keyEqRow{{vals: []any{"ABC", "old"}, want: sup}, {vals: []any{"zzz", "gone"}, want: abs}})
}

func TestWithoutRowidKeyClauseRtrimKeysAreEqual(t *testing.T) {
	runKeyEq(t, "create table wr(k text, v, primary key (k collate rtrim)) without rowid", 1,
		[][]any{{"ab  ", "old"}, {"zz", "gone"}},
		[][]any{{"ab", "live"}},
		[]keyEqRow{{vals: []any{"ab  ", "old"}, want: sup}, {vals: []any{"zz", "gone"}, want: abs}})
}

// The key clause overrides the column's own collation, in both directions.
func TestWithoutRowidKeyClauseOverridesColumnCollation(t *testing.T) {
	runKeyEq(t, "create table wr(k text collate binary, v, primary key (k collate nocase)) without rowid", 1,
		[][]any{{"ABC", "old"}},
		[][]any{{"abc", "live"}},
		[]keyEqRow{{vals: []any{"ABC", "old"}, want: sup}})
	runKeyEq(t, "create table wr(k text collate nocase, v, primary key (k collate binary)) without rowid", 1,
		[][]any{{"ABC", "old"}},
		[][]any{{"abc", "live"}},
		[]keyEqRow{{vals: []any{"ABC", "old"}, want: abs}})
}

// A composite key clause: each component keeps its own collation.
func TestWithoutRowidKeyClauseCompositeCollations(t *testing.T) {
	// stored key-first: a, b, v
	runKeyEq(t, "create table wr(a text, b text, v, primary key (a collate nocase, b)) without rowid", 2,
		[][]any{{"AB", "x", "old"}, {"AB", "X", "other"}},
		[][]any{{"ab", "x", "live"}},
		[]keyEqRow{{vals: []any{"AB", "x", "old"}, want: sup}, {vals: []any{"AB", "X", "other"}, want: abs}})
}

// A collation the library cannot apply in the key clause: never absent.
func TestWithoutRowidKeyClauseUnknownCollationIsUnknownNeverAbsent(t *testing.T) {
	runKeyEq(t, "create table wr(k text, v, primary key (k collate mycoll)) without rowid", 1,
		[][]any{{"a", "old"}, {"b", "gone"}},
		[][]any{{"a", "new"}},
		[]keyEqRow{
			{vals: []any{"a", "old"}, want: unk, note: sqlitefile.NoteLiveUncertain},
			{vals: []any{"b", "gone"}, want: unk, note: sqlitefile.NoteLiveUncertain},
		})
}

// Boundaries of the key equality that mutations of keyDigest would move.
func TestKeyDigestBoundaries(t *testing.T) {
	i := func(n int64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindInt, Int: n} }
	f := func(x float64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: x} }
	tx := func(s string) sqlitefile.Value {
		return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(s), Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
	}
	one := func(v sqlitefile.Value, coll string) [32]byte {
		d, ok := sqlitefile.KeyDigest([]sqlitefile.Value{v}, []string{coll})
		if !ok {
			t.Fatalf("not decidable: %+v", v)
		}
		return d
	}
	// 2^63 is not an int64: it never equals the smallest integer
	if one(i(math.MinInt64), "BINARY") == one(f(1<<63), "BINARY") {
		t.Error("min int equals the real 2^63")
	}
	if one(i(math.MinInt64), "BINARY") != one(f(-(1<<63)), "BINARY") {
		t.Error("min int differs from the real -2^63")
	}
	// the whole ASCII letter range folds under NOCASE, 'Z' and 'A' included
	for _, c := range []string{"A", "Z", "M"} {
		if one(tx(c), "NOCASE") != one(tx(strings.ToLower(c)), "NOCASE") {
			t.Errorf("NOCASE %q differs from its lower case", c)
		}
	}
	if one(tx("@"), "NOCASE") == one(tx("`"), "NOCASE") || one(tx("["), "NOCASE") == one(tx("{"), "NOCASE") {
		t.Error("NOCASE folds a character outside A-Z")
	}
	// composite text keys are length prefixed: a value holding the kind byte
	// (3, text) cannot move the boundary between two components.
	kind := string(rune(3))
	d1, _ := sqlitefile.KeyDigest([]sqlitefile.Value{tx("p" + kind + "r"), tx("s")}, []string{"BINARY", "BINARY"})
	d2, _ := sqlitefile.KeyDigest([]sqlitefile.Value{tx("p"), tx("r" + kind + "s")}, []string{"BINARY", "BINARY"})
	if d1 == d2 {
		t.Error("composite keys with the kind byte inside a component collide")
	}
}

// Two live rows that share a key (damage, or a key canonicalized wrongly)
// make the live set uncertain: a history row that matches neither is unknown,
// never absent.
func TestWithoutRowidDuplicateLiveKeyTaintsAbsence(t *testing.T) {
	runKeyEq(t, "create table wr(k text collate nocase primary key, v) without rowid", 1,
		[][]any{{"gone", "old"}},
		[][]any{{"abc", "one"}, {"ABC", "two"}},
		[]keyEqRow{{vals: []any{"gone", "old"}, want: unk, note: sqlitefile.NoteLiveUncertain}})
}

// A live row whose key the library cannot compare (UTF-16 text under NOCASE)
// is found by its row digest only: a history row with an integer key matches
// nothing, and the live set is uncertain, so it is unknown, never absent.
func TestWithoutRowidUndecidableLiveKeyTaintsAbsence(t *testing.T) {
	runKeyEqOpts(t, sqlitetest.Options{PageSize: hps, Encoding: 2}, "create table wr(k collate nocase primary key, v) without rowid", 1,
		[][]any{{int64(5), "old"}},
		[][]any{{"abc", "live"}},
		[]keyEqRow{{vals: []any{int64(5), "old"}, want: unk, note: sqlitefile.NoteLiveUncertain}})
}
