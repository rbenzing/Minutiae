package sqlitefile_test

import (
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// fuzzOrderValue draws a Value from a selector and the fuzz inputs. Floats include
// NaN, so reflexivity holds only because a NaN orders as NULL.
func fuzzOrderValue(sel byte, n int64, fbits uint64, b []byte) sqlitefile.Value {
	switch sel % 9 {
	case 0:
		return sqlitefile.Value{}
	case 1:
		return evInt(n)
	case 2:
		return evFloat(math.Float64frombits(fbits))
	case 3:
		return evText(string(b))
	case 4:
		return evBlob(string(b))
	case 5:
		return orderText(sqlitefile.EncUTF16LE, string(b))
	case 6:
		return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Enc: sqlitefile.EncUTF16BE}
	case 7:
		return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Omitted: len(b)%2 == 0}
	}
	return evFloat(float64(n))
}

func FuzzOrderValues(f *testing.F) {
	f.Add(byte(1), int64(1), uint64(0), []byte("a"), byte(2), int64(1), uint64(0), []byte("b"), byte(3), int64(0), uint64(0), []byte("A"), byte(1))
	f.Add(byte(2), int64(0), math.Float64bits(math.NaN()), []byte(nil), byte(0), int64(0), uint64(0), []byte(nil), byte(2), int64(0), math.Float64bits(1), []byte(nil), byte(0))
	f.Add(byte(3), int64(0), uint64(0), []byte("a "), byte(3), int64(0), uint64(0), []byte("a"), byte(4), int64(0), uint64(0), []byte("a"), byte(2))
	f.Add(byte(1), int64(math.MaxInt64), uint64(0), []byte(nil), byte(2), int64(0), math.Float64bits(float64(math.MaxInt64)), []byte(nil), byte(1), int64(math.MinInt64), uint64(0), []byte(nil), byte(0))
	// Embedded NULs under NOCASE (U55): equal by the engine, different bytes after the NUL.
	f.Add(byte(3), int64(0), uint64(0), []byte("a\x00b"), byte(3), int64(0), uint64(0), []byte("a\x00c"), byte(3), int64(0), uint64(0), []byte("A\x00b"), byte(2))
	f.Add(byte(3), int64(0), uint64(0), []byte("\x00"), byte(3), int64(0), uint64(0), []byte("a\x00"), byte(3), int64(0), uint64(0), []byte("a\x00\x00"), byte(2))
	f.Fuzz(func(t *testing.T, sa byte, na int64, fa uint64, ba []byte, sb byte, nb int64, fb uint64, bb []byte, sc byte, nc int64, fc uint64, bc []byte, collSel byte) {
		a, b, c := fuzzOrderValue(sa, na, fa, ba), fuzzOrderValue(sb, nb, fb, bb), fuzzOrderValue(sc, nc, fc, bc)
		coll := []string{"", "BINARY", "NOCASE", "RTRIM", "nocase"}[int(collSel)%5]
		ab, sab := sqlitefile.OrderValues(a, b, coll)
		ba2, sba := sqlitefile.OrderValues(b, a, coll)
		if sab != sba {
			t.Fatalf("status %v one way, %v the other", sab, sba)
		}
		if again, sagain := sqlitefile.OrderValues(a, b, coll); again != ab || sagain != sab {
			t.Fatalf("not deterministic")
		}
		if sab == sqlitefile.OrderOK {
			if ab < -1 || ab > 1 || ab != -ba2 {
				t.Fatalf("antisymmetry: %d and %d", ab, ba2)
			}
		} else if ab != 0 {
			t.Fatalf("an undecided result carries %d", ab)
		}
		if aa, st := sqlitefile.OrderValues(a, a, coll); st == sqlitefile.OrderOK && aa != 0 {
			t.Fatalf("reflexivity: OrderValues(a,a) = %d", aa)
		}
		bc2, sbc := sqlitefile.OrderValues(b, c, coll)
		ac, sac := sqlitefile.OrderValues(a, c, coll)
		if sab == sqlitefile.OrderOK && sbc == sqlitefile.OrderOK && sac == sqlitefile.OrderOK {
			if ab <= 0 && bc2 <= 0 && ac > 0 {
				t.Fatalf("transitivity: a<=b, b<=c, a>c")
			}
			if ab >= 0 && bc2 >= 0 && ac < 0 {
				t.Fatalf("transitivity: a>=b, b>=c, a<c")
			}
		}
	})
}
