package androidcalendar_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same Events row gives the same record whether it arrives from Live() or rebuilt as a recovered row.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range recordedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteProvider)
			parsertest.AssertMapperSourceAgnostic(t, h, androidcalendar.Parser{}, b, 200)
		})
	}
}
