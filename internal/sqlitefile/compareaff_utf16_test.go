package sqlitefile_test

import (
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// utf16Units encodes 16-bit units as UTF-16 text bytes.
func utf16Units(be bool, units []uint16) sqlitefile.Value {
	b := make([]byte, 0, 2*len(units))
	for _, u := range units {
		if be {
			b = append(b, byte(u>>8), byte(u))
		} else {
			b = append(b, byte(u), byte(u>>8))
		}
	}
	enc := sqlitefile.EncUTF16LE
	if be {
		enc = sqlitefile.EncUTF16BE
	}
	return sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: b, Len: int64(len(b)), Enc: enc}
}

// utf16Of encodes s (any code point) as UTF-16 text.
func utf16Of(be bool, s string) sqlitefile.Value {
	var units []uint16
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			units = append(units, uint16(0xd800+(r>>10)), uint16(0xdc00+(r&0x3ff)))
		} else {
			units = append(units, uint16(r))
		}
	}
	return utf16Units(be, units)
}

// fullDecodeReference is the specification of the UTF-16 path: decode the whole
// text, then classify the UTF-8 result. Text that does not decode is undecided.
func fullDecodeReference(v sqlitefile.Value, own, other sqlitefile.Affinity) (sqlitefile.Value, sqlitefile.ConvStatus) {
	s, ok := v.Text()
	if !ok {
		return v, sqlitefile.ConvUndecided
	}
	return sqlitefile.ConvertForComparison(evText(s), own, other)
}

// sameOutcome compares a UTF-16 outcome with the reference one: a text left as
// text may differ in its encoding, a number must be the same number.
func sameOutcome(got sqlitefile.Value, gst sqlitefile.ConvStatus, ref sqlitefile.Value, rst sqlitefile.ConvStatus) bool {
	if gst != rst {
		return false
	}
	if gst == sqlitefile.ConvUndecided {
		return true
	}
	if got.Kind != ref.Kind {
		return false
	}
	switch got.Kind {
	case sqlitefile.KindInt:
		return got.Int == ref.Int
	case sqlitefile.KindFloat:
		return math.Float64bits(got.Float) == math.Float64bits(ref.Float)
	}
	return true
}

// minAlloc is the smallest number of heap bytes one call of f allocates over a
// few trials (noise from other goroutines can only add to a trial).
func minAlloc(f func()) uint64 {
	f() // warm up
	best := uint64(math.MaxUint64)
	for range 5 {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		f()
		runtime.ReadMemStats(&m1)
		best = min(best, m1.TotalAlloc-m0.TotalAlloc)
	}
	return best
}

// A bounded prefix of UTF-16 text is enough: the outcome equals classifying the
// fully decoded text, for both byte orders and for the shapes that make a
// prefix rule go wrong (long blanks, a number then blanks, a bad unit early or
// late, an odd byte count, a pair straddling the 64-byte cut, a NUL).
// PASSES ON ARRIVAL (the whole value was decoded). Mutation: cut the prefix at
// 32 bytes instead of 64.
func TestConvertForComparisonUTF16PrefixEqualsFullDecode(t *testing.T) {
	blanks := func(n int) string { return strings.Repeat(" \t\n\v\f\r", n/6+1)[:n] }
	cases := []string{
		"", blanks(100000), blanks(100000) + "7", "7" + blanks(100000), "7" + blanks(70) + "x", blanks(70) + "7" + blanks(70),
		strings.Repeat("7", 63), strings.Repeat("7", 64), strings.Repeat("7", 65), strings.Repeat("7", 64) + " ", strings.Repeat("7", 64) + "   x",
		"1" + strings.Repeat("0", 62) + "     x", "7\x00abc", "\x007", "  \x00  ", "7 \x00 junk", "7.5", "-3", "1e5", "1e", "19.99", "inf", "-nan",
		strings.Repeat("7", 63) + "\U0001F600", strings.Repeat("7", 62) + "\U0001F600" + "7", strings.Repeat("7", 64) + "\U0001F600",
		"\U0001F600", " \U0001F600 ", "7é", " 7", "nokia", strings.Repeat("The quick brown fox ", 20),
		strings.Repeat("7", 200) + "x", "7" + strings.Repeat("0", 70) + ".5",
	}
	check := func(label string, v sqlitefile.Value) {
		t.Helper()
		for _, other := range []sqlitefile.Affinity{sqlitefile.AffInteger, sqlitefile.AffNumeric} {
			for _, own := range []sqlitefile.Affinity{sqlitefile.AffBlob, sqlitefile.AffText} {
				got, gst := sqlitefile.ConvertForComparison(v, own, other)
				ref, rst := fullDecodeReference(v, own, other)
				if !sameOutcome(got, gst, ref, rst) {
					t.Errorf("%s: prefix outcome (kind %d status %d) differs from full decode (kind %d status %d)", label, got.Kind, gst, ref.Kind, rst)
				}
			}
		}
	}
	for _, be := range []bool{false, true} {
		for i, s := range cases {
			check("case "+string(rune('A'+i%26))+" be="+map[bool]string{false: "false", true: "true"}[be], utf16Of(be, s))
		}
		bad := map[string][]uint16{
			"lone high first":    {0xd800, '7'},
			"lone low first":     {0xdc00, '7'},
			"high at the end":    {'7', 0xd800},
			"high then not low":  {'7', 0xd800, 'x'},
			"blanks then bad":    {' ', ' ', ' ', 0xdc00},
			"nul then bad":       {'7', 0, 0xdc00},
			"number, nul, valid": {'7', 0, 'a'},
		}
		late := make([]uint16, 0, 101)
		for range 100 {
			late = append(late, '7')
		}
		bad["bad late"] = append(late, 0xdc00)
		for name, u := range bad {
			check(name, utf16Units(be, u))
		}
		odd := utf16Of(be, "7")
		odd.Bytes = append(odd.Bytes, 0)
		check("odd byte count", odd)
		longOdd := utf16Of(be, strings.Repeat(" ", 1000)+"7")
		longOdd.Bytes = longOdd.Bytes[:len(longOdd.Bytes)-1]
		check("long odd", longOdd)
		// A random sweep over an alphabet that exercises every rule.
		rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a fixed seed makes the sweep reproducible; no secret is involved
		alpha := []string{"7", "7", "0", ".", "e", "-", "+", " ", " ", "\t", "\x00", "x", "é", "\U0001F600", "5", "E"}
		for range 3000 {
			var sb strings.Builder
			for range rng.IntN(150) {
				sb.WriteString(alpha[rng.IntN(len(alpha))])
			}
			check("random "+sb.String(), utf16Of(be, sb.String()))
		}
	}
}

// The classification of a UTF-16 value allocates a small constant, whatever the
// value's length: a whole-value decode would cost several times its size.
func TestConvertForComparisonUTF16AllocationDoesNotGrowWithLength(t *testing.T) {
	const limit = convAllocLimit
	for _, n := range []int{1 << 10, 1 << 16, 1 << 20} {
		blank := strings.Repeat(" ", n)
		for _, c := range []struct{ name, s string }{
			{"blanks then 7", blank + "7"},
			{"7 then blanks", "7" + blank},
			{"digits", strings.Repeat("7", n)},
			{"decimal then blanks", "19.99" + blank},
		} {
			for _, be := range []bool{false, true} {
				v := utf16Of(be, c.s)
				got := minAlloc(func() { sqlitefile.ConvertForComparison(v, sqlitefile.AffBlob, sqlitefile.AffInteger) })
				if got > limit {
					t.Errorf("%s, %d units, be=%v: allocated %d bytes; want at most %d whatever the length", c.name, n, be, got, limit)
				}
			}
		}
	}
	// The same bound holds for UTF-8 values.
	v := evText(strings.Repeat(" ", 1<<20) + "19.99")
	if got := minAlloc(func() { sqlitefile.ConvertForComparison(v, sqlitefile.AffBlob, sqlitefile.AffInteger) }); got > limit {
		t.Errorf("utf-8: allocated %d bytes; want at most %d", got, limit)
	}
}

// convAllocLimit is the most heap bytes one ConvertForComparison call may
// allocate, whatever the length and encoding of the value. Measured: 0 bytes for
// a text, 40 for an integer rendered as text; the limit is a 20x margin.
const convAllocLimit = 1024
