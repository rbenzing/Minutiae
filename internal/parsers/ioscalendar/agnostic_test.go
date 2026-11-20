package ioscalendar_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 10 and parent 12.1: the same CalendarItem row gives the same record whether it arrives from Live() or rebuilt as
// a recovered row (the typed pass's mapping, which joins nothing either way, B2).
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range recordedVariants() {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			parsertest.AssertMapperSourceAgnostic(t, h, ioscalendar.Parser{}, b, 200)
		})
	}
}
