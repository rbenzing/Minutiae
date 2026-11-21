package androidgooglemessages_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12 (4I4-P6): the typed pass's mappings give the same records for a row whether it arrives from the live view or
// rebuilt as a recovered row (a mapper depends only on its row and its context): gm-aosp-v2 and gm-hostile.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range []string{"gm-aosp-v2", "gm-hostile"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteApp)
			parsertest.AssertMapperSourceAgnostic(t, h, androidgooglemessages.Parser{}, b, 2000)
		})
	}
}
