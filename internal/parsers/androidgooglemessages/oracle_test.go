package androidgooglemessages_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 10, 4I5 Tasks 4 to 6: every variant with records parses to the records, warnings and notes of its literal oracle
// (tools/fixtures/google_messages.py), the whole of it. No variant is dropped.
func TestAndroidGoogleMessagesMatchesOracle(t *testing.T) {
	ran := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue // refused (no signature): the probe tests state those
		}
		ran++
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteApp)
			p := androidgooglemessages.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect, "messages", "parts")
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}
