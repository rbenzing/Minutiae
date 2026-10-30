package ioscalls_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same row gives the same record whether it arrives from Live() or rebuilt as a recovered
// row, which is what lets plan 4G reuse the mappings. coredata-* exercise the ZCALLRECORD mapping and
// legacy-call-history the call mapping.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			remote, logical := remotesFor(v)
			b := parsertest.LoadFixture(t, "testdata", v).BundleIOS(h, remote, logical)
			parsertest.AssertMapperSourceAgnostic(t, h, ioscalls.Parser{}, b, 200)
		})
	}
}
