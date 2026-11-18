package ioswacalls_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same row gives the same record whether it arrives from Live() or rebuilt as a recovered row, for the
// claimed table. This is what makes the mapping reusable by the typed pass.
func TestMapperIsSourceAgnostic(t *testing.T) {
	covered := false
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			parsertest.AssertMapperSourceAgnostic(t, h, ioswacalls.Parser{}, b, 200)
		})
		covered = covered || len(parsertest.LoadFixture(t, "testdata", v).Expect.Records) > 0
	}
	if !covered {
		t.Error("no variant holds a ZWACDCALLEVENT row: the source-agnostic check covers nothing")
	}
}
