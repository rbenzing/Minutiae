package iosaddressbook_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same ABPerson row gives the same record whether it arrives from Live() or rebuilt as a
// recovered row, which is what lets plan 4G reuse the mapping. Every variant is checked: the joins (multi-values,
// labels, entries, store, groups) go through the context, and the photo presence is outside the mapper.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			parsertest.AssertMapperSourceAgnostic(t, h, iosaddressbook.Parser{}, b, 200)
		})
	}
}
