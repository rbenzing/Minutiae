package androidcontacts_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The mapper agrees with the independent scenario oracle on every row of every pinned fixture:
// accounts through the accounts join, the aggregate time under times.aggregate_last_updated, the
// states of a dangling join, the typed data rows and the caps (T69, T72).
func TestContactsMatchOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := fx.Bundle(h, fixtureRemote)
			p := androidcontacts.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}
