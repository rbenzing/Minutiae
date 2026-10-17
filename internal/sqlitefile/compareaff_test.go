package sqlitefile_test

import (
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// convWant is the expected outcome of one conversion: the kind and payload of
// the value, and the status.
type convWant struct {
	kind sqlitefile.Kind
	i    int64
	f    float64
	s    string
	st   sqlitefile.ConvStatus
}

func wantInt(n int64) convWant     { return convWant{kind: sqlitefile.KindInt, i: n} }
func wantFloat(f float64) convWant { return convWant{kind: sqlitefile.KindFloat, f: f} }
func wantText(s string) convWant   { return convWant{kind: sqlitefile.KindText, s: s} }
func wantUndecided() convWant      { return convWant{st: sqlitefile.ConvUndecided} }

// sameConv reports whether got (a result of ConvertForComparison) is the wanted
// outcome. An undecided outcome carries no value to compare.
func sameConv(got sqlitefile.Value, st sqlitefile.ConvStatus, w convWant) bool {
	if st != w.st {
		return false
	}
	if st == sqlitefile.ConvUndecided {
		return true
	}
	if got.Kind != w.kind {
		return false
	}
	switch w.kind {
	case sqlitefile.KindInt:
		return got.Int == w.i
	case sqlitefile.KindFloat:
		return got.Float == w.f && math.Signbit(got.Float) == math.Signbit(w.f)
	case sqlitefile.KindText:
		return string(got.Bytes) == w.s
	}
	return true
}

// A text under a numeric comparison either converts, is provably text (and stays
// the same text), or is undecided.
func TestConvertForComparisonTable(t *testing.T) {
	type row struct {
		name  string
		v     sqlitefile.Value
		own   sqlitefile.Affinity
		other sqlitefile.Affinity
		want  convWant
	}
	var rows []row
	add := func(name string, v sqlitefile.Value, own, other sqlitefile.Affinity, w convWant) {
		rows = append(rows, row{name, v, own, other, w})
	}
	// The classifier: a text with TEXT or no affinity of its own against each numeric affinity.
	conv := map[string]convWant{
		"7": wantInt(7), " 7 ": wantInt(7), "\t7\n": wantInt(7), "+7": wantInt(7), "-0": wantInt(0), "007": wantInt(7),
		"7.0": wantInt(7), "7.": wantInt(7), ".5": wantFloat(0.5), "5e0": wantInt(5), "1e3": wantInt(1000), "-3": wantInt(-3),
		"9223372036854775807": wantInt(math.MaxInt64), "-9223372036854775808": wantInt(math.MinInt64),
		"7.5": wantFloat(7.5), "19.99": wantFloat(19.99), "0.1": wantFloat(0.1),
		"7\x00": wantInt(7), "7\x00abc": wantInt(7), "7 \x00 junk": wantInt(7), "\v\f\r7\r\f\v": wantInt(7),
	}
	for s, w := range conv {
		for _, other := range []sqlitefile.Affinity{sqlitefile.AffInteger, sqlitefile.AffReal, sqlitefile.AffNumeric} {
			for _, own := range []sqlitefile.Affinity{sqlitefile.AffBlob, sqlitefile.AffText} {
				add("convert "+s, evText(s), own, other, w)
			}
		}
	}
	for _, s := range []string{
		"abc", "", "  ", "7x", "x7", "7 7", "0x10", "1e", "1e+", ".", "+", "--7", "7..0", "1_0", "inf", "-Infinity", "NaN",
		"\xc2\xa07", "\xd9\xa7", "\x007", "ab\x007", "1e\x00", "nokia", "e5", "7e5e", "1.2.3", "7-", "+-7",
	} {
		add("text "+s, evText(s), sqlitefile.AffBlob, sqlitefile.AffNumeric, wantText(s))
	}
	for _, s := range []string{
		"9223372036854775808", "-9223372036854775809", "12345678901234567890", "3.141592653589793238", "1e23",
		"1e400", "1.5e-30", "0.1234567890123456", strings.Repeat("1", 65), "1" + strings.Repeat("0", 64),
	} {
		add("undecided "+s, evText(s), sqlitefile.AffBlob, sqlitefile.AffInteger, wantUndecided())
	}
	// A TEXT target: an integer becomes its decimal text, a real is undecided.
	add("int to text", evInt(7), sqlitefile.AffBlob, sqlitefile.AffText, wantText("7"))
	add("minint to text", evInt(math.MinInt64), sqlitefile.AffBlob, sqlitefile.AffText, wantText("-9223372036854775808"))
	add("float to text", evFloat(7.5), sqlitefile.AffBlob, sqlitefile.AffText, wantUndecided())
	add("integral float to text", evFloat(7), sqlitefile.AffBlob, sqlitefile.AffText, wantUndecided())
	add("blob to text", evBlob("7"), sqlitefile.AffBlob, sqlitefile.AffText, convWant{kind: sqlitefile.KindBlob})
	add("null to text", sqlitefile.Value{}, sqlitefile.AffBlob, sqlitefile.AffText, convWant{kind: sqlitefile.KindNull})
	add("text to text", evText("7"), sqlitefile.AffBlob, sqlitefile.AffText, wantText("7"))
	// A numeric target: numbers, blobs and NULL are not touched.
	add("int to numeric", evInt(7), sqlitefile.AffBlob, sqlitefile.AffInteger, wantInt(7))
	add("float to numeric", evFloat(7.5), sqlitefile.AffBlob, sqlitefile.AffReal, wantFloat(7.5))
	add("blob to numeric", evBlob("7"), sqlitefile.AffBlob, sqlitefile.AffNumeric, convWant{kind: sqlitefile.KindBlob})
	add("null to numeric", sqlitefile.Value{}, sqlitefile.AffBlob, sqlitefile.AffNumeric, convWant{kind: sqlitefile.KindNull})
	// UTF-16 text decodes before it is classified; one that does not decode is undecided.
	add("utf16le 7", evUTF16(true, "7"), sqlitefile.AffBlob, sqlitefile.AffInteger, wantInt(7))
	add("utf16be 7.5", evUTF16(false, "7.5"), sqlitefile.AffBlob, sqlitefile.AffInteger, wantFloat(7.5))
	odd := evUTF16(true, "7")
	odd.Bytes = append(odd.Bytes, 0)
	add("odd utf16", odd, sqlitefile.AffBlob, sqlitefile.AffInteger, wantUndecided())
	// Text that is not valid UTF-8 is not repaired: undecided when a conversion applies.
	add("invalid utf8", sqlitefile.Value{Kind: sqlitefile.KindText, Bytes: []byte{'7', 0xff}, Len: 2, Enc: sqlitefile.EncUTF8},
		sqlitefile.AffBlob, sqlitefile.AffInteger, wantUndecided())
	// A value that is not materialised is undecided when a conversion applies.
	for _, m := range []struct {
		name string
		mod  func(*sqlitefile.Value)
	}{
		{"omitted", func(v *sqlitefile.Value) { v.Omitted, v.Bytes = true, nil }},
		{"unread", func(v *sqlitefile.Value) { v.Unread = true }},
		{"clipped", func(v *sqlitefile.Value) { v.Clipped, v.Len = true, 9 }},
	} {
		v := evText("7")
		m.mod(&v)
		add(m.name+" under numeric", v, sqlitefile.AffBlob, sqlitefile.AffInteger, wantUndecided())
		add(m.name+" under text own", v, sqlitefile.AffText, sqlitefile.AffInteger, wantUndecided())
		iv := evInt(7)
		m.mod(&iv)
		add(m.name+" int under text", iv, sqlitefile.AffBlob, sqlitefile.AffText, wantUndecided())
	}
	for _, r := range rows {
		got, st := sqlitefile.ConvertForComparison(r.v, r.own, r.other)
		if !sameConv(got, st, r.want) {
			t.Errorf("%s (own %d, other %d): got kind %d int %d float %v text %q status %d; want %+v", r.name, r.own, r.other, got.Kind, got.Int, got.Float, got.Bytes, st, r.want)
		}
	}
	// A converted integer has the UTF-8 encoding the index keys.
	if got, _ := sqlitefile.ConvertForComparison(evInt(-5), sqlitefile.AffBlob, sqlitefile.AffText); got.Enc != sqlitefile.EncUTF8 || got.Len != 2 {
		t.Errorf("integer rendered as text: enc %d len %d; want UTF-8 and length 2", got.Enc, got.Len)
	}
	// An unmaterialised value under no conversion is returned as it came.
	om := evText("7")
	om.Omitted, om.Bytes = true, nil
	if got, st := sqlitefile.ConvertForComparison(om, sqlitefile.AffInteger, sqlitefile.AffInteger); st != sqlitefile.ConvOK || !got.Omitted {
		t.Errorf("omitted value, equal classes: %+v %d; want it unchanged", got, st)
	}
}

// PASSES ON ARRIVAL against the stub (which converts nothing): the operand that
// is not weaker keeps its value.
func TestConvertForComparisonLeavesTheStrongerOperandAlone(t *testing.T) {
	for _, c := range []struct {
		name  string
		v     sqlitefile.Value
		own   sqlitefile.Affinity
		other sqlitefile.Affinity
	}{
		{"numeric own against TEXT", evInt(7), sqlitefile.AffNumeric, sqlitefile.AffText},
		{"numeric own against none", evInt(7), sqlitefile.AffNumeric, sqlitefile.AffBlob},
		{"TEXT own against TEXT", evText("7"), sqlitefile.AffText, sqlitefile.AffText},
		{"numeric own against numeric", evText("7"), sqlitefile.AffNumeric, sqlitefile.AffNumeric},
		{"TEXT own against none", evText("7"), sqlitefile.AffText, sqlitefile.AffBlob},
		{"none against none", evText("7"), sqlitefile.AffBlob, sqlitefile.AffBlob},
		{"real own against TEXT", evFloat(7.5), sqlitefile.AffReal, sqlitefile.AffText},
	} {
		got, st := sqlitefile.ConvertForComparison(c.v, c.own, c.other)
		if st != sqlitefile.ConvOK || got.Kind != c.v.Kind || got.Int != c.v.Int || got.Float != c.v.Float || string(got.Bytes) != string(c.v.Bytes) {
			t.Errorf("%s: got %+v status %d; want %+v unchanged", c.name, got, st, c.v)
		}
	}
}

// A name is text, never an "undecided" number: the classifier must not make a
// phone brand or a sentence a failed join. PASSES ON ARRIVAL (the stub converts
// nothing); mutation: report a letter-first text as undecided.
func TestConvertForComparisonNamesAreText(t *testing.T) {
	long := "The quick brown fox jumps over the lazy dog and keeps on running far away " + strings.Repeat("x", 30)
	if len(long) < 100 {
		t.Fatal("test string too short")
	}
	for _, s := range []string{
		"nokia", "iPhone", "n/a", "Name", "Infinite loop", "Information", "nanny", "-iPhone", "+name", long,
		"1. one two three four five six seven eight nine ten eleven twelve thirteen fourteen",
	} {
		got, st := sqlitefile.ConvertForComparison(evText(s), sqlitefile.AffBlob, sqlitefile.AffNumeric)
		if st != sqlitefile.ConvOK || got.Kind != sqlitefile.KindText || string(got.Bytes) != s {
			t.Errorf("%q: got %+v status %d; want it decided as the same text", s, got, st)
		}
	}
}

// The classifier reads at most 64 bytes of number-like text and allocates
// nothing in proportion to the input.
func TestConvertForComparisonHugeTextIsBounded(t *testing.T) {
	sevens := evText(strings.Repeat("7", 4<<20))
	blanks := evText(strings.Repeat(" ", 4<<20) + "7")
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	_, st1 := sqlitefile.ConvertForComparison(sevens, sqlitefile.AffBlob, sqlitefile.AffInteger)
	got2, st2 := sqlitefile.ConvertForComparison(blanks, sqlitefile.AffBlob, sqlitefile.AffInteger)
	runtime.ReadMemStats(&m1)
	if st1 != sqlitefile.ConvUndecided {
		t.Errorf("4 MiB of digits: status %d; want ConvUndecided", st1)
	}
	if st2 != sqlitefile.ConvOK || got2.Kind != sqlitefile.KindInt || got2.Int != 7 {
		t.Errorf("4 MiB of blanks then 7: %+v status %d; want integer 7", got2, st2)
	}
	if d := m1.TotalAlloc - m0.TotalAlloc; d > 64<<10 {
		t.Errorf("allocated %d bytes for 8 MiB of input; want at most 64 KiB", d)
	}
}
