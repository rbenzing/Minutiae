package chromium_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Every record, warning and note of every committed variant (urls, visits, searches, downloads and meta) is the
// oracle's.
func TestChromiumMatchesOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := fx.Bundle(h, remoteFor(v))
			p := chromium.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			noViolations(t, res)
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
			if len(fx.Expect.Notes) == 0 {
				t.Error("the oracle states no note")
			}
		})
	}
}
