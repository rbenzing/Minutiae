package androidcalendar

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// durationGrammar is spec 5.3's strict subset as a regular expression: sign, P, then runs of 1 to 9 digits each
// carrying a unit, with an optional T before any run. The count of T and the distinct units are checked beside it.
var durationGrammar = regexp.MustCompile(`^[+-]?P(?:T?[0-9]{1,9}[WDHMS])+$`)

// durationRun is one run of the grammar, for the independent sum.
var durationRun = regexp.MustCompile(`T?([0-9]{1,9})([WDHMS])`)

// FuzzAndroidCalendarDuration: parseDuration never panics, and a parsed text matches the grammar, holds at most one T
// and each unit at most once, fits 64 bytes and sums within the Java int (spec 5.3).
func FuzzAndroidCalendarDuration(f *testing.F) {
	for _, s := range []string{
		"PT1H", "P3600S", "P1D", "P2W", "-PT30M", "P1DT2H", "+P1W", "P", "PT", "P1Y", "P1.5H", "1H", "P1H2",
		"PT1H ", "P9999999999S", "P2147483648S", "P999999999S", "P3550W", "P3551W", "P1H1H", "PT1HT2M", strings.Repeat("P1S", 30),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		secs, state := parseDuration(b)
		// the converse (fix round 1, M5): a grammatical text parses, to the sum computed here independently
		if wantSecs, wantState, ok := grammatical(b); ok && (secs != wantSecs || state != wantState) {
			t.Fatalf("parseDuration(%q) = %d, %q; the grammar gives %d, %q", b, secs, state, wantSecs, wantState)
		}
		switch state {
		case "":
			s := string(b)
			if len(b) > maxDurationLen || !durationGrammar.MatchString(s) || strings.Count(s, "T") > 1 {
				t.Fatalf("parseDuration(%q) parsed a text outside the grammar", b)
			}
			seen := map[rune]bool{}
			for _, r := range s {
				if strings.ContainsRune("WDHMS", r) {
					if seen[r] {
						t.Fatalf("parseDuration(%q) parsed a repeated unit %c", b, r)
					}
					seen[r] = true
				}
			}
			if secs > 2147483647 || secs < -2147483647 {
				t.Fatalf("parseDuration(%q) = %d, beyond the Java int", b, secs)
			}
		case "unparsed", "out-of-range", "too-long":
			if secs != 0 {
				t.Fatalf("parseDuration(%q) = %d with state %q", b, secs, state)
			}
			if state == "too-long" && len(b) <= maxDurationLen {
				t.Fatalf("parseDuration(%q) too-long at %d bytes", b, len(b))
			}
		default:
			t.Fatalf("parseDuration(%q): state %q outside the vocabulary", b, state)
		}
	})
}

// grammatical says whether b is in spec 5.3's strict subset (64 bytes at most, the grammar, at most one T, each unit
// once) and, if so, the result parseDuration must give: the signed sum, or "out-of-range" beyond the Java int. It
// shares no code with parseDuration.
func grammatical(b []byte) (secs int64, state string, ok bool) {
	s := string(b)
	if len(b) > maxDurationLen || !durationGrammar.MatchString(s) || strings.Count(s, "T") > 1 {
		return 0, "", false
	}
	unit := map[string]int64{"W": 604800, "D": 86400, "H": 3600, "M": 60, "S": 1}
	seen := map[string]bool{}
	var total int64
	for _, m := range durationRun.FindAllStringSubmatch(s, -1) {
		if seen[m[2]] {
			return 0, "", false
		}
		seen[m[2]] = true
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, "", false
		}
		total += n * unit[m[2]]
	}
	if total > 2147483647 {
		return 0, "out-of-range", true
	}
	if strings.HasPrefix(s, "-") {
		total = -total
	}
	return total, "", true
}
