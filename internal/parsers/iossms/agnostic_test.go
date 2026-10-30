package iossms_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1: the same message row gives the same record whether it arrives from Live() or rebuilt as a
// recovered row, which is what lets plan 4G reuse the mapping. Every variant is checked; ios17-nanos carries
// the attachments, the reactions and the plists, so its joins (handle, chat, members, attachments, recoverable)
// all go through the context.
func TestMapperIsSourceAgnostic(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v))
			parsertest.AssertMapperSourceAgnostic(t, h, iossms.Parser{}, b, 200)
		})
	}
}
