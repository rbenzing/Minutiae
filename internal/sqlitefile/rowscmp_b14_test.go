package sqlitefile_test

// B14: the history comparison keys a WITHOUT ROWID row by the engine's equality (sqlitefile.EqualityKey). The
// behaviour changes this brings (ruling J78) are each pinned here: NaN and NULL are KeyNever (decided: never equal),
// UTF-16 under NOCASE or RTRIM has a folded key, and unread or invalid-UTF-8 text is KeyUnknown (undecided).

import (
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func kd(vals ...sqlitefile.Value) func(coll ...string) ([32]byte, sqlitefile.KeyStatus) {
	return func(coll ...string) ([32]byte, sqlitefile.KeyStatus) { return sqlitefile.KeyDigest(vals, coll) }
}

// J78 changes 1 and 4: NaN and NULL were "equal by bits" and "no key"; both are now KeyNever.
func TestKeyDigestNaNAndNullAreNeverNotUnknown(t *testing.T) {
	for name, v := range map[string]sqlitefile.Value{"NaN": evFloat(math.NaN()), "NULL": {Kind: sqlitefile.KindNull}} {
		for _, c := range []string{"BINARY", "NOCASE", "RTRIM"} {
			if _, st := kd(v)(c); st != sqlitefile.KeyNever {
				t.Errorf("%s under %s: status %v, want KeyNever", name, c, st)
			}
		}
	}
}

// J78 change 2: UTF-16 text under NOCASE or RTRIM has a key, equal to the key of the same UTF-8 text.
func TestKeyDigestUTF16FoldedKeyIsAvailable(t *testing.T) {
	for _, le := range []bool{true, false} {
		d16, st := kd(evUTF16(le, "AbC"))("NOCASE")
		d8, st8 := kd(evText("aBc"))("NOCASE")
		if st != sqlitefile.KeyOK || st8 != sqlitefile.KeyOK || d16 != d8 {
			t.Errorf("le=%v NOCASE: status %v %v, equal %v", le, st, st8, d16 == d8)
		}
		r16, st := kd(evUTF16(le, "ab  "))("RTRIM")
		r8, _ := kd(evText("ab"))("RTRIM")
		if st != sqlitefile.KeyOK || r16 != r8 {
			t.Errorf("le=%v RTRIM: status %v, equal %v", le, st, r16 == r8)
		}
	}
}

// J78 change 3: unread values and invalid UTF-8 under NOCASE or RTRIM are KeyUnknown, never a key and never KeyNever.
func TestKeyDigestUnreadAndInvalidTextAreUnknown(t *testing.T) {
	unread := sqlitefile.Value{Kind: sqlitefile.KindInt, Int: 5, Unread: true}
	for _, c := range []string{"BINARY", "NOCASE", "RTRIM"} {
		if _, st := kd(unread)(c); st != sqlitefile.KeyUnknown {
			t.Errorf("unread under %s: status %v, want KeyUnknown", c, st)
		}
	}
	for _, c := range []string{"NOCASE", "RTRIM"} {
		if _, st := kd(evText("\xffA "))(c); st != sqlitefile.KeyUnknown {
			t.Errorf("invalid UTF-8 under %s: status %v, want KeyUnknown", c, st)
		}
	}
	if _, st := kd(evText("\xffA "))("BINARY"); st != sqlitefile.KeyOK {
		t.Errorf("invalid UTF-8 under BINARY: status %v, want KeyOK (bytes compare)", st)
	}
}

// A tuple with a component that never equals anything never equals anything, whatever else is undecided; an
// undecided component with no such component leaves the tuple undecided.
func TestKeyDigestTupleStatusKeepsNeverAndUnknownApart(t *testing.T) {
	never, unk := evFloat(math.NaN()), sqlitefile.Value{Kind: sqlitefile.KindInt, Unread: true}
	for name, tc := range map[string]struct {
		vals []sqlitefile.Value
		want sqlitefile.KeyStatus
	}{
		"never then unknown": {[]sqlitefile.Value{never, unk}, sqlitefile.KeyNever},
		"unknown then never": {[]sqlitefile.Value{unk, never}, sqlitefile.KeyNever},
		"unknown and ok":     {[]sqlitefile.Value{evInt(1), unk}, sqlitefile.KeyUnknown},
		"ok and ok":          {[]sqlitefile.Value{evInt(1), evText("a")}, sqlitefile.KeyOK},
	} {
		if _, st := sqlitefile.KeyDigest(tc.vals, []string{"BINARY", "BINARY"}); st != tc.want {
			t.Errorf("%s: status %v, want %v", name, st, tc.want)
		}
	}
}

// The engine decides: every pair of values and collation, the digest of a one-column tuple is equal exactly when
// the engine compares equal (a KeyNever or KeyUnknown side is never equal): 0 divergences. The oracle checks
// equality only, in a UTF-8, a UTF-16le and a UTF-16be database; the status (KeyNever against KeyUnknown) is pinned
// by the KeyDigest unit tests of this file.
func TestEngineOracleKeyEqualityMatrix(t *testing.T) {
	eq := func(x, y sqlitefile.Value, coll string) bool {
		kx, sx := sqlitefile.KeyDigest([]sqlitefile.Value{x}, []string{coll})
		ky, sy := sqlitefile.KeyDigest([]sqlitefile.Value{y}, []string{coll})
		return sx == sqlitefile.KeyOK && sy == sqlitefile.KeyOK && kx == ky
	}
	checkEqualityAgainstEngine(t, "", evText, eq)
	checkEqualityAgainstEngine(t, "pragma encoding='UTF-16le'", func(s string) sqlitefile.Value { return evUTF16(true, s) }, eq)
	checkEqualityAgainstEngine(t, "pragma encoding='UTF-16be'", func(s string) sqlitefile.Value { return evUTF16(false, s) }, eq)
}

// J78 changes 1 and 4, J103: a NaN key (stored NaN reads as NULL, as the engine does) and a NULL key never equal a
// live row, and equal-to-nothing decides key equality, not presence: the relation is unknown with the reason
// "key-never", never absent, whatever the live set holds.
func TestWithoutRowidNaNNeverEqualsLive(t *testing.T) {
	runKeyEq(t, "create table wr(k primary key, v) without rowid", 1,
		[][]any{{math.NaN(), "old"}},
		[][]any{{math.NaN(), "live"}},
		[]keyEqRow{{vals: []any{nil, "old"}, want: unk, note: "key-never"}})
}

// J103: a recovered row with a NULL key (a stored NaN reads as NULL) and a byte-identical live twin is
// unknown/"key-never", never absent (and not the live row's twin either: the key equals nothing).
func TestWithoutRowidNullKeyWithIdenticalLiveTwinIsUnknownNeverAbsent(t *testing.T) {
	runKeyEq(t, "create table wr(k primary key, v) without rowid", 1,
		[][]any{{math.NaN(), "same"}},
		[][]any{{math.NaN(), "same"}},
		[]keyEqRow{{vals: []any{nil, "same"}, want: unk, note: "key-never"}})
}

// J78 change 2: a UTF-16 database with a NOCASE key relates its history rows, where it was unknown.
func TestWithoutRowidUTF16NocaseKeyIsAvailable(t *testing.T) {
	runKeyEqOpts(t, sqlitetest.Options{PageSize: hps, Encoding: 2}, "create table wr(k collate nocase primary key, v) without rowid", 1,
		[][]any{{"ABC", "old"}, {"zzz", "gone"}},
		[][]any{{"abc", "live"}},
		[]keyEqRow{{vals: []any{"ABC", "old"}, want: sup}, {vals: []any{"zzz", "gone"}, want: abs}})
	runKeyEqOpts(t, sqlitetest.Options{PageSize: hps, Encoding: 2}, "create table wr(k collate rtrim primary key, v) without rowid", 1,
		[][]any{{"ab  ", "old"}, {"zz", "gone"}},
		[][]any{{"ab", "live"}},
		[]keyEqRow{{vals: []any{"ab  ", "old"}, want: sup}, {vals: []any{"zz", "gone"}, want: abs}})
}

// J78 change 3, and the KeyNever/KeyUnknown split: a history row whose key is undecided (invalid UTF-8 under
// NOCASE) has an unknown relation with the reason "key-unknown", and a NaN key beside it is unknown with the
// reason "key-never" (J103): neither is ever absent.
func TestWithoutRowidUnknownKeyIsUnknownNeverAbsent(t *testing.T) {
	runKeyEq(t, "create table wr(k collate nocase primary key, v) without rowid", 1,
		[][]any{{"\xffa", "old"}},
		[][]any{{"\xffA", "live"}},
		[]keyEqRow{{vals: []any{"\xffa", "old"}, want: unk, note: "key-unknown"}})
	runKeyEq(t, "create table wr(k primary key, v) without rowid", 1,
		[][]any{{math.NaN(), "old"}},
		[][]any{{int64(1), "live"}},
		[]keyEqRow{{vals: []any{nil, "old"}, want: unk, note: "key-never"}})
}

// Ordinary keys relate exactly as before the move onto EqualityKey.
func TestRowsRelationsUnchangedOnExistingFixtures(t *testing.T) {
	runKeyEq(t, "create table wr(k primary key, v) without rowid", 1,
		[][]any{{int64(2), "old"}, {"txt", "gone"}, {1.5, "real-gone"}},
		[][]any{{int64(2), "new"}},
		[]keyEqRow{
			{vals: []any{int64(2), "old"}, want: sup},
			{vals: []any{"txt", "gone"}, want: abs},
			{vals: []any{1.5, "real-gone"}, want: abs},
		})
}
