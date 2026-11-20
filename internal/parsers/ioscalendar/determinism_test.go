package ioscalendar_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The same input gives the same records, warnings and notes run after run, whatever time.Local is: the zone of the
// host never shows in a record (AssertDeterministic varies time.Local; F16).
func TestParseIsDeterministic(t *testing.T) {
	for _, v := range []string{"calendar-ios", "calendar-ios-deleted", "calendar-ios-extract"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			p := ioscalendar.Parser{}
			parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
		})
	}
}
