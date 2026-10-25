package chromium_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every urls and visits record, warning and note of every committed variant is the oracle's (Task 3 compares these
// three tables; Task 4 adds the other two).
func TestVisitsAndURLsMatchOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := fx.Bundle(h, remoteFor(v))
			p := chromium.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect, "urls", "visits", "meta")
			// the meta table gives notes and no record; the comparer keys notes by table, so compare them here
			n := 0
			for k, want := range fx.Expect.Notes {
				if strings.HasPrefix(k, "meta.") {
					n++
					if res.Notes[k] != want {
						t.Errorf("note %s = %q, the oracle expects %q", k, res.Notes[k], want)
					}
				}
			}
			if n == 0 {
				t.Error("the oracle states no meta note")
			}
			for _, r := range res.Records {
				if !strings.HasPrefix(r.Locator, "sqlite:table=urls;") && !strings.HasPrefix(r.Locator, "sqlite:table=visits;") {
					t.Errorf("record %s: Task 3 maps only urls and visits", r.Locator)
				}
			}
		})
	}
}
