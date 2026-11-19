package androidcalendar

import (
	"strings"
	"testing"
)

// Spec 5.3, 9 and F01: the strict subset of the provider's duration grammar, bounded in length (64 bytes), digit
// runs (9 digits) and total (the Java int the provider sums in).
func TestAndroidCalendarDurationGrammarIsBounded(t *testing.T) {
	cases := []struct {
		in    string
		secs  int64
		state string
	}{
		{"PT1H", 3600, ""},
		{"P3600S", 3600, ""},
		{"P1D", 86400, ""},
		{"P2W", 1209600, ""},
		{"-PT30M", -1800, ""},
		{"P1DT2H", 93600, ""},
		{"+P1W", 604800, ""},
		{"P1H1M", 3660, ""},
		{"PT0S", 0, ""},
		{"P", 0, "unparsed"},
		{"PT", 0, "unparsed"},
		{"P1Y", 0, "unparsed"},
		{"P1.5H", 0, "unparsed"},
		{"1H", 0, "unparsed"},
		{"P1H2", 0, "unparsed"},
		{"PT1H ", 0, "unparsed"},
		{"", 0, "unparsed"},
		{"P1H1H", 0, "unparsed"},   // a repeated unit: the provider keeps the last, the strict subset does not choose
		{"PT1HT2M", 0, "unparsed"}, // two T
		{"P1DT", 0, "unparsed"},    // a T after the last run is not between runs
		{"P-1H", 0, "unparsed"},
		{"p1h", 0, "unparsed"},
		{"+-P1H", 0, "unparsed"},
		{"P9999999999S", 0, "out-of-range"},
		{"P2147483648S", 0, "out-of-range"},
		{"P2147483647S", 0, "out-of-range"}, // 10 digits: the digit-run rule of spec 5.3 (F01)
		{"P999999999S", 999999999, ""},      // the largest accepted run
		{"P3550W", 2147040000, ""},
		{"P3551W", 0, "out-of-range"}, // over the int
		{"-P3551W", 0, "out-of-range"},
		{"P9999999999S1", 0, "unparsed"},                         // a grammar failure wins over the digit-run rule
		{"P" + strings.Repeat("1", 62) + "S", 0, "out-of-range"}, // 64 bytes: read, then refused by the digit-run rule
		{"P" + strings.Repeat("1", 63) + "S", 0, "too-long"},     // 65 bytes
	}
	for _, c := range cases {
		secs, state := parseDuration([]byte(c.in))
		if secs != c.secs || state != c.state {
			t.Errorf("parseDuration(%q) = %d, %q; want %d, %q", c.in, secs, state, c.secs, c.state)
		}
	}
	if len("P"+strings.Repeat("1", 63)+"S") != maxDurationLen+1 {
		t.Fatal("the too-long case is not 65 bytes")
	}
}
