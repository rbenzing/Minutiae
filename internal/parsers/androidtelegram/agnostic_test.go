package androidtelegram_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 11 (4I4-P6): the typed pass's mappings give the same records for a row whether it arrives from the live view
// or rebuilt as a recovered row (a mapper depends only on its row and its context). Every variant with records, the
// WAL-backed ones included.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue // refused before any mapping runs (TestParserCoversEveryVariant)
		}
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteApp)
			parsertest.AssertMapperSourceAgnostic(t, h, androidtelegram.Parser{}, b, 200)
		})
	}
}
