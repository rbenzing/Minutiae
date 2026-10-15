package sqlitefile_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// fuzzValue builds a Value from the fuzz inputs: kind picks the storage class
// (and, for text, the encoding), the rest are the payloads.
func fuzzValue(kind byte, n int64, fbits uint64, b []byte) sqlitefile.Value {
	switch kind % 5 {
	case 1:
		return evInt(n)
	case 2:
		return evFloat(math.Float64frombits(fbits))
	case 3:
		v := sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Enc: sqlitefile.Encoding(1 + (kind/5)%3)}
		return v
	case 4:
		return sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: b, Len: int64(len(b))}
	}
	return sqlitefile.Value{}
}

func sameValueBits(a, b sqlitefile.Value) bool {
	return a.Kind == b.Kind && a.Int == b.Int && math.Float64bits(a.Float) == math.Float64bits(b.Float) &&
		bytes.Equal(a.Bytes, b.Bytes) && a.Enc == b.Enc && a.Omitted == b.Omitted
}

// FuzzConvertForComparison: the conversion never panics, is deterministic and
// idempotent, produces only int, real or text, never a non-finite real from
// text, and allocates in proportion to its input at most.
func FuzzConvertForComparison(f *testing.F) {
	for _, s := range []string{
		"7", " 7 ", "\t7\n", "+7", "-0", "007", "7.0", "7.", ".5", "5e0", "1e3", "-3",
		"9223372036854775807", "-9223372036854775808", "abc", "", "  ", "7x", "0x10", "1e", "1e+", ".", "+", "--7", "7..0",
		"9223372036854775808", "12345678901234567890", "3.141592653589793238", "1e23", "1e400", "inf", "NaN", "7\x00abc",
	} {
		for own := byte(0); own < 5; own += 2 {
			f.Add(byte(3), int64(0), uint64(0), []byte(s), own, byte(1))
			f.Add(byte(3), int64(0), uint64(0), []byte(s), own, byte(3))
		}
	}
	f.Add(byte(1), int64(math.MinInt64), uint64(0), []byte(nil), byte(0), byte(1))
	f.Add(byte(2), int64(0), math.Float64bits(7.5), []byte(nil), byte(0), byte(1))
	f.Add(byte(2), int64(0), math.Float64bits(math.NaN()), []byte(nil), byte(0), byte(1))
	f.Add(byte(8), int64(0), uint64(0), []byte{'7', 0, '7', 0}, byte(0), byte(4)) // UTF-16LE "7"
	big := utf16Of(false, strings.Repeat(" ", 1<<18)+"7").Bytes                   // a large UTF-16 value, so an allocation that grows with length is seen
	f.Add(byte(8), int64(0), uint64(0), big, byte(0), byte(4))
	f.Fuzz(func(t *testing.T, kind byte, n int64, fbits uint64, b []byte, own, other byte) {
		v := fuzzValue(kind, n, fbits, b)
		ao, at := sqlitefile.Affinity(own%5), sqlitefile.Affinity(other%5)
		got, st := sqlitefile.ConvertForComparison(v, ao, at)
		// The cost is a small constant whatever the length and encoding: a regression
		// to decoding or copying the whole value fails here on any large input.
		if a := minAlloc(func() { sqlitefile.ConvertForComparison(v, ao, at) }); a > convAllocLimit {
			t.Fatalf("allocated %d bytes for %d bytes of input; want at most %d", a, len(b), convAllocLimit)
		}
		// Bounded-prefix classification of UTF-16 text equals classifying the fully decoded text.
		if v.Kind == sqlitefile.KindText && (v.Enc == sqlitefile.EncUTF16LE || v.Enc == sqlitefile.EncUTF16BE) &&
			(at == sqlitefile.AffInteger || at == sqlitefile.AffReal || at == sqlitefile.AffNumeric) && (ao == sqlitefile.AffText || ao == sqlitefile.AffBlob) {
			ref, rst := fullDecodeReference(v, ao, at)
			if !sameOutcome(got, st, ref, rst) {
				t.Fatalf("prefix outcome (kind %d status %d) differs from full decode (kind %d status %d) for %x", got.Kind, st, ref.Kind, rst, b)
			}
		}
		got2, st2 := sqlitefile.ConvertForComparison(v, ao, at)
		if st != st2 || !sameValueBits(got, got2) {
			t.Fatal("not deterministic")
		}
		if st == sqlitefile.ConvUndecided {
			if !sameValueBits(got, v) {
				t.Fatal("an undecided conversion changed the value")
			}
			return
		}
		switch v.Kind {
		case sqlitefile.KindText, sqlitefile.KindInt, sqlitefile.KindFloat:
			if got.Kind != sqlitefile.KindText && got.Kind != sqlitefile.KindInt && got.Kind != sqlitefile.KindFloat {
				t.Fatalf("kind %d became kind %d", v.Kind, got.Kind)
			}
		default:
			if !sameValueBits(got, v) {
				t.Fatalf("a blob or NULL was converted: %+v", got)
			}
		}
		if v.Kind == sqlitefile.KindText && got.Kind == sqlitefile.KindFloat && (math.IsNaN(got.Float) || math.IsInf(got.Float, 0)) {
			t.Fatalf("text %q became a non-finite real", v.Bytes)
		}
		again, st3 := sqlitefile.ConvertForComparison(got, ao, at)
		if st3 != sqlitefile.ConvOK || !sameValueBits(again, got) {
			t.Fatalf("converting a converted value changed it: %+v -> %+v (status %d)", got, again, st3)
		}
	})
}
