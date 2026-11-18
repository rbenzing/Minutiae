package androidwhatsapp_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 12.1 for message_add_on: the same reaction row gives the same record whether it arrives from Live() or rebuilt as a
// recovered row (the kit also covers the claimed tables, see TestMapperIsSourceAgnostic). message_add_on is mapped but not
// claimed, so its mapping is checked directly, on the variants that hold reactions.
func TestMapperIsSourceAgnosticForReactions(t *testing.T) {
	var reactions int
	for _, v := range parsedVariants {
		res, _ := runVariant(t, v)
		for _, r := range res.Records {
			if r.Payload["kind"] == "reaction" {
				reactions++
			}
		}
	}
	if reactions == 0 {
		t.Fatal("no variant holds a reaction: the check proves nothing")
	}
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "android-modern-2026").Bundle(h, remoteFor("android-modern-2026"))
	parsertest.AssertMapperSourceAgnostic(t, h, androidwhatsapp.Parser{}, b, 500)
}
