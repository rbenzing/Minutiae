package androidcalendar_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Parent spec 10 and plan 4I3 Tasks 4 and 5: every variant with records parses to exactly the Events records (their
// child joins included), the warnings and the notes its literal oracle states, with no contract violation.
func TestAndroidCalendarMatchesOracle(t *testing.T) {
	for _, v := range recordedVariants {
		t.Run(v, func(t *testing.T) {
			fx := parsertest.LoadFixture(t, "testdata", v)
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteProvider)
			p := androidcalendar.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}
