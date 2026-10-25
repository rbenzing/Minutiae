package chromium_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same row gives the same record whether it arrives from Live() or rebuilt as a recovered row,
// which is what lets plan 4G reuse the mappings.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			parsertest.AssertMapperSourceAgnostic(t, h, chromium.Parser{}, b, 200)
		})
	}
}
