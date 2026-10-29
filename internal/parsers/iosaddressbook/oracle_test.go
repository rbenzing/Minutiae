package iosaddressbook_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// bundleOf places a fixture as the parser meets it: with-images as a backup (BundleIOS does not place the
// images database), the others as a file acquisition.
func bundleOf(h *parsertest.Harness, fx parsertest.Fixture, variant string) parsertest.Bundle {
	if variant == "with-images" {
		return fx.BundleIOSBackup(h, "HomeDomain", relative)
	}
	return fx.BundleIOS(h, remote, logical)
}

// The parser produces exactly what the committed scenario oracle states, on every variant (the 19 oracle
// choices of Task 13 bind it).
func TestAddressBookMatchesOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := bundleOf(h, fx, v)
			p := iosaddressbook.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			if len(res.Violations) != 0 {
				t.Errorf("contract violations: %v", res.Violations)
			}
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}
