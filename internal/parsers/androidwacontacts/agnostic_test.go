package androidwacontacts_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidwacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same row gives the same record whether it arrives from Live() or rebuilt as a recovered row. This is
// what makes the mapping reusable by the typed recovery pass.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range []string{"wa-2.11", "wa-current", "wa-business", "wa-deleted"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			parsertest.AssertMapperSourceAgnostic(t, h, androidwacontacts.Parser{}, b, 200)
		})
	}
}
