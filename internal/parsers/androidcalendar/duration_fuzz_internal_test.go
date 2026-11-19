package androidcalendar

import (
	"regexp"
	"strings"
	"testing"
)

// durationGrammar is spec 5.3's strict subset as a regular expression: sign, P, then runs of 1 to 9 digits each
// carrying a unit, with an optional T before any run. The count of T and the distinct units are checked beside it.
var durationGrammar = regexp.MustCompile(`^[+-]?P(?:T?[0-9]{1,9}[WDHMS])+$`)

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
