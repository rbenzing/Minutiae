package sqlitefile_test

import (
	"database/sql"
	"errors"
	"math"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only)

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func evInt(n int64) sqlitefile.Value { return sqlitefile.Value{Kind: sqlitefile.KindInt, Int: n} }
func evFloat(f float64) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: f}
}

func evText(s string) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte(s), Len: int64(len(s)), Enc: sqlitefile.EncUTF8}
}

func evBlob(s string) sqlitefile.Value {
	return sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: []byte(s), Len: int64(len(s))}
}

func evUTF16(le bool, s string) sqlitefile.Value {
	var b []byte
	for _, r := range s { // BMP only in these tests
		if le {
			b = append(b, byte(r), byte(r>>8))
		} else {
			b = append(b, byte(r>>8), byte(r))
		}
	}
	enc := sqlitefile.EncUTF16BE
	if le {
		enc = sqlitefile.EncUTF16LE
	}
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Enc: enc}
}

func eqKey(t *testing.T, v sqlitefile.Value, coll string) ([32]byte, sqlitefile.KeyStatus) {
	t.Helper()
	k, st, err := sqlitefile.EqualityKey(v, coll)
	if err != nil {
		t.Fatalf("EqualityKey(%+v, %q): %v", v, coll, err)
	}
	return k, st
}

func TestEqualityKeyTable(t *testing.T) {
	type pair struct {
		name string
		a, b sqlitefile.Value
		coll string
		same bool
	}
	pairs := []pair{
		{"1 == 1.0", evInt(1), evFloat(1), "", true},
		{"-0.0 == 0", evFloat(math.Copysign(0, -1)), evInt(0), "", true},
		{"0.0 == 0", evFloat(0), evInt(0), "", true},
		{"2^63-1 vs 2^63 float", evInt(math.MaxInt64), evFloat(9.223372036854775807e18), "", false},
		{"1 != 1.5", evInt(1), evFloat(1.5), "", false},
		{"1 != '1'", evInt(1), evText("1"), "", false},
		{"'1' != blob 1", evText("1"), evBlob("1"), "", false},
		{"BINARY A != a", evText("A"), evText("a"), "BINARY", false},
		{"empty collation is BINARY", evText("A"), evText("a"), "", false},
		{"NOCASE A == a", evText("A"), evText("a"), "NOCASE", true},
		{"NOCASE lower-case name", evText("A"), evText("a"), "nocase", true},
		{"NOCASE E-acute != e-acute", evText("É"), evText("é"), "NOCASE", false},
		{"RTRIM trailing spaces", evText("a  "), evText("a"), "RTRIM", true},
		{"RTRIM mixed case name", evText("a  "), evText("a"), "RtRiM", true},
		{"RTRIM leading space", evText(" a"), evText("a"), "RTRIM", false},
		{"RTRIM is case-sensitive", evText("A "), evText("a"), "RTRIM", false},
		{"UTF-16 NOCASE", evUTF16(true, "Ab"), evUTF16(false, "aB"), "NOCASE", true},
		{"UTF-16 equals UTF-8 under NOCASE", evUTF16(true, "Ab"), evText("aB"), "NOCASE", true},
		{"UTF-16 BINARY keys the stored bytes", evUTF16(true, "a"), evText("a"), "BINARY", false},
	}
	for _, p := range pairs {
		ka, sa := eqKey(t, p.a, p.coll)
		kb, sb := eqKey(t, p.b, p.coll)
		if sa != sqlitefile.KeyOK || sb != sqlitefile.KeyOK {
			t.Errorf("%s: status %v %v, want KeyOK", p.name, sa, sb)
			continue
		}
		if (ka == kb) != p.same {
			t.Errorf("%s: keys equal = %v, want %v", p.name, ka == kb, p.same)
		}
	}

	odd := evUTF16(true, "ab")
	odd.Bytes = odd.Bytes[:3] // an odd byte count: undecodable
	status := []struct {
		name string
		v    sqlitefile.Value
		coll string
		want sqlitefile.KeyStatus
	}{
		{"NaN", evFloat(math.NaN()), "", sqlitefile.KeyNever},
		{"NULL", sqlitefile.Value{}, "NOCASE", sqlitefile.KeyNever},
		{"omitted", sqlitefile.Value{Kind: sqlitefile.KindText, Omitted: true}, "", sqlitefile.KeyUnknown},
		{"unread", sqlitefile.Value{Kind: sqlitefile.KindInt, Unread: true}, "", sqlitefile.KeyUnknown},
		{"clipped", sqlitefile.Value{Kind: sqlitefile.KindBlob, Clipped: true, Bytes: []byte("x")}, "", sqlitefile.KeyUnknown},
		{"invalid UTF-8 under NOCASE", evText("\xff"), "NOCASE", sqlitefile.KeyUnknown},
		{"invalid UTF-8 under RTRIM", evText("\xff "), "RTRIM", sqlitefile.KeyUnknown},
		{"invalid UTF-8 under BINARY", evText("\xff"), "BINARY", sqlitefile.KeyOK},
		{"undecodable UTF-16", odd, "NOCASE", sqlitefile.KeyUnknown},
	}
	for _, s := range status {
		if _, got := eqKey(t, s.v, s.coll); got != s.want {
			t.Errorf("%s: status %v, want %v", s.name, got, s.want)
		}
	}
	// An unsupported collation is checked first, for every value kind.
	for _, v := range []sqlitefile.Value{
		{},
		evInt(1), evFloat(math.NaN()), evText("a"), evBlob("a"),
		{Kind: sqlitefile.KindInt, Omitted: true},
	} {
		if _, _, err := sqlitefile.EqualityKey(v, "foo"); !errors.Is(err, sqlitefile.ErrUnsupportedCollation) {
			t.Errorf("EqualityKey(%+v, foo) err = %v, want ErrUnsupportedCollation", v, err)
		}
	}
}

func TestCanonicalCollationAndColumnCollation(t *testing.T) {
	for in, want := range map[string]string{
		"": "BINARY", "binary": "BINARY", "BiNaRy": "BINARY",
		"nocase": "NOCASE", "NOCASE": "NOCASE", "rtrim": "RTRIM", "RTRIM": "RTRIM",
	} {
		got, err := sqlitefile.CanonicalCollation(in)
		if err != nil || got != want {
			t.Errorf("CanonicalCollation(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"foo", "nocase ", "unicode", "BINARY\x00"} {
		if _, err := sqlitefile.CanonicalCollation(in); !errors.Is(err, sqlitefile.ErrUnsupportedCollation) {
			t.Errorf("CanonicalCollation(%q) err = %v, want ErrUnsupportedCollation", in, err)
		}
	}
	cases := []struct {
		c    sqlitefile.Column
		want string
	}{
		{sqlitefile.Column{}, ""},
		{sqlitefile.Column{Collation: "nocase"}, "nocase"},
		{sqlitefile.Column{Collation: "nocase", KeyCollation: "RTRIM"}, "RTRIM"},
		{sqlitefile.Column{KeyCollation: "foo"}, "foo"},
	}
	for _, c := range cases {
		if got := sqlitefile.ColumnCollation(c.c); got != c.want {
			t.Errorf("ColumnCollation(%+v) = %q, want %q", c.c, got, c.want)
		}
	}
}

// The two behaviours of keyDigest that differ from the engine stay as they
// were (open question Q13): two NaNs are one key, and UTF-16 text under
// NOCASE or RTRIM has no key.
func TestKeyDigestLegacyQuirksArePinned(t *testing.T) {
	d1, ok1 := sqlitefile.KeyDigest([]sqlitefile.Value{evFloat(math.NaN())}, []string{"BINARY"})
	d2, ok2 := sqlitefile.KeyDigest([]sqlitefile.Value{evFloat(math.NaN())}, []string{"BINARY"})
	if !ok1 || !ok2 || d1 != d2 {
		t.Errorf("two NaNs: ok %v %v, equal %v; the legacy digest treats them as one key", ok1, ok2, d1 == d2)
	}
	for _, c := range []string{"NOCASE", "RTRIM"} {
		if _, ok := sqlitefile.KeyDigest([]sqlitefile.Value{evUTF16(true, "a")}, []string{c}); ok {
			t.Errorf("UTF-16 under %s has a legacy key", c)
		}
	}
	if _, ok := sqlitefile.KeyDigest([]sqlitefile.Value{evUTF16(true, "a")}, []string{"BINARY"}); !ok {
		t.Error("UTF-16 under BINARY has no legacy key")
	}
}

// The two further differences of the legacy digest (ruling B38): invalid
// UTF-8 under NOCASE or RTRIM is hashed bytewise (EqualityKey has no key for
// it), and Unread is ignored (EqualityKey has no key for it).
func TestKeyDigestFurtherLegacyDifferencesArePinned(t *testing.T) {
	for _, c := range []string{"NOCASE", "RTRIM"} {
		if _, ok := sqlitefile.KeyDigest([]sqlitefile.Value{evText("\xffA ")}, []string{c}); !ok {
			t.Errorf("invalid UTF-8 under %s has no legacy key", c)
		}
		if _, st, _ := sqlitefile.EqualityKey(evText("\xffA "), c); st != sqlitefile.KeyUnknown {
			t.Errorf("EqualityKey of invalid UTF-8 under %s: %v, want KeyUnknown", c, st)
		}
	}
	// bytewise: NOCASE folds the ASCII letter after the invalid byte
	da, _ := sqlitefile.KeyDigest([]sqlitefile.Value{evText("\xffA")}, []string{"NOCASE"})
	db, _ := sqlitefile.KeyDigest([]sqlitefile.Value{evText("\xffa")}, []string{"NOCASE"})
	if da != db {
		t.Error("legacy NOCASE digest does not fold bytewise after an invalid byte")
	}
	un := sqlitefile.Value{Kind: sqlitefile.KindInt, Int: 5, Unread: true}
	du, oku := sqlitefile.KeyDigest([]sqlitefile.Value{un}, []string{"BINARY"})
	dr, okr := sqlitefile.KeyDigest([]sqlitefile.Value{evInt(5)}, []string{"BINARY"})
	if !oku || !okr || du != dr {
		t.Errorf("legacy digest must ignore Unread: ok %v %v equal %v", oku, okr, du == dr)
	}
	if _, st, _ := sqlitefile.EqualityKey(un, "BINARY"); st != sqlitefile.KeyUnknown {
		t.Errorf("EqualityKey of an unread value: %v, want KeyUnknown", st)
	}
}

// Only NULL never matches: a Kind the library does not define is undecidable.
func TestEqualityKeyUnknownKindIsUnknownNeverNever(t *testing.T) {
	for _, c := range []string{"", "NOCASE"} {
		v := sqlitefile.Value{Kind: sqlitefile.Kind(200)}
		if _, st, err := sqlitefile.EqualityKey(v, c); err != nil || st != sqlitefile.KeyUnknown {
			t.Errorf("unknown kind under %q: status %v err %v, want KeyUnknown", c, st, err)
		}
	}
}

// Outside the two quirks (no NaN, no UTF-16), EqualityKey and the digest of a
// one-column tuple agree on equality, for every pair and collation.
func TestEqualityKeyAgreesWithKeyDigestOutsideTheQuirks(t *testing.T) {
	vals := []sqlitefile.Value{
		evInt(0), evInt(1), evInt(-1), evInt(math.MaxInt64), evInt(math.MinInt64),
		evFloat(0), evFloat(math.Copysign(0, -1)), evFloat(1), evFloat(1.5), evFloat(-1), evFloat(9.223372036854775807e18),
		evFloat(math.Inf(1)), evFloat(math.Inf(-1)), evFloat(1e300),
		evText(""), evText("a"), evText("A"), evText("a "), evText(" a"), evText("a  "), evText("é"), evText("É"), evText("1"),
		evBlob(""), evBlob("a"), evBlob("A"), evBlob("a "), evBlob("1"),
	}
	for _, coll := range []string{"BINARY", "NOCASE", "RTRIM"} {
		for i, a := range vals {
			for j, b := range vals {
				ka, sa, _ := sqlitefile.EqualityKey(a, coll)
				kb, sb, _ := sqlitefile.EqualityKey(b, coll)
				da, oa := sqlitefile.KeyDigest([]sqlitefile.Value{a}, []string{coll})
				db, ob := sqlitefile.KeyDigest([]sqlitefile.Value{b}, []string{coll})
				if (sa == sqlitefile.KeyOK) != oa || (sb == sqlitefile.KeyOK) != ob {
					t.Fatalf("%s %d,%d: availability differs", coll, i, j)
				}
				if sa == sqlitefile.KeyOK && sb == sqlitefile.KeyOK && (ka == kb) != (da == db) {
					t.Errorf("%s %+v vs %+v: EqualityKey equal=%v, KeyDigest equal=%v", coll, a, b, ka == kb, da == db)
				}
			}
		}
	}
}

// The layer's key equality equals the engine's: columns with BLOB affinity so
// the bound parameters are compared with no affinity conversion.
func TestEqualityKeyMatchesEngine(t *testing.T) {
	checkEqualityAgainstEngine(t, "", evText)
}

// The same comparison in a UTF-16le database: the engine converts the bound
// text to the database encoding, the layer decodes before folding.
func TestEqualityKeyMatchesEngineUTF16(t *testing.T) {
	checkEqualityAgainstEngine(t, "pragma encoding='UTF-16le'", func(s string) sqlitefile.Value { return evUTF16(true, s) })
}

func checkEqualityAgainstEngine(t *testing.T, pragma string, evText func(string) sqlitefile.Value) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if pragma != "" {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`create table k(a blob collate binary, b blob collate nocase, c blob collate rtrim)`); err != nil {
		t.Fatal(err)
	}
	type gv struct {
		v sqlitefile.Value
		g any
	}
	vs := []gv{
		{evInt(1), int64(1)},
		{evFloat(1), float64(1)},
		{evFloat(1.5), 1.5},
		{evInt(0), int64(0)},
		{evFloat(math.Copysign(0, -1)), math.Copysign(0, -1)},
		{evInt(-7), int64(-7)},
		{evFloat(-7), float64(-7)},
		{evInt(math.MaxInt64), int64(math.MaxInt64)},
		{evFloat(9.223372036854775807e18), 9.223372036854775807e18},
		{evFloat(math.Inf(1)), math.Inf(1)},
		{evFloat(math.Inf(-1)), math.Inf(-1)},
		{evText("a\x00b"), "a\x00b"},
		{evText("a\x00B"), "a\x00B"},
		{evText("a\x00"), "a\x00"},
		{evText("a"), "a"},
		{evText("A"), "A"},
		{evText("a "), "a "},
		{evText(" a"), " a"},
		{evText("a  "), "a  "},
		{evText("A "), "A "},
		{evText("é"), "é"},
		{evText("É"), "É"},
		{evText("1"), "1"},
		{evText(""), ""},
		{evBlob("a"), []byte("a")},
		{evBlob("A"), []byte("A")},
		{evBlob("a "), []byte("a ")},
		{evBlob("1"), []byte("1")},
		{evBlob(""), []byte{}},
		{sqlitefile.Value{}, nil},
	}
	type stmts struct{ ins, sel string }
	cols := map[string]stmts{
		"BINARY": {`insert into k(a) values (?)`, `select a = ? from k`},
		"NOCASE": {`insert into k(b) values (?)`, `select b = ? from k`},
		"RTRIM":  {`insert into k(c) values (?)`, `select c = ? from k`},
	}
	for coll, st := range cols {
		for _, x := range vs {
			if _, err := db.Exec(`delete from k`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(st.ins, x.g); err != nil {
				t.Fatal(err)
			}
			for _, y := range vs {
				var eq sql.NullBool
				if err := db.QueryRow(st.sel, y.g).Scan(&eq); err != nil {
					t.Fatal(err)
				}
				kx, sx, _ := sqlitefile.EqualityKey(x.v, coll)
				ky, sy, _ := sqlitefile.EqualityKey(y.v, coll)
				got := sx == sqlitefile.KeyOK && sy == sqlitefile.KeyOK && kx == ky
				want := eq.Valid && eq.Bool
				if got != want {
					t.Errorf("%s: %+v vs %+v: layer equal=%v, engine equal=%v", coll, x.v, y.v, got, want)
				}
			}
		}
	}
}
