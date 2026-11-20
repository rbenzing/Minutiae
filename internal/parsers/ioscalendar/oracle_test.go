package ioscalendar_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// oracleOmits says a note key the literal oracle of tools/fixtures/calendar_ios.py does not state: rowrun's
// schema.CalendarItem (its order is the mapping's, Task 7 left it to the parser) and the unmapped_columns.<table>
// notes. Only for these two forms is the oracle's set of notes read as a subset of the parser's; every other note
// must be exactly the oracle's, no more and no fewer.
func oracleOmits(key string) bool {
	return key == "schema.CalendarItem" || strings.HasPrefix(key, "unmapped_columns.")
}

// Parent spec 10 and plan 4I3 Task 9: every variant with records parses to exactly the CalendarItem records (their
// joins included), the warnings and the notes its literal oracle states, with no contract violation.
func TestIOSCalendarMatchesOracle(t *testing.T) {
	for _, v := range recordedVariants() {
		t.Run(v, func(t *testing.T) {
			fx := parsertest.LoadFixture(t, "testdata", v)
			h := parsertest.NewHarness(t)
			b := bundleOf(h, fx, v)
			p := ioscalendar.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
			var extra []string
			for k := range res.Notes {
				if _, stated := fx.Expect.Notes[k]; !stated && !oracleOmits(k) {
					extra = append(extra, k+"="+res.Notes[k])
				}
			}
			sort.Strings(extra)
			if len(extra) > 0 {
				t.Errorf("notes the oracle does not state: %v", extra)
			}
		})
	}
}
