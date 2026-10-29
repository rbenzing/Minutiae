package iossms_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// oracleVariants are the variants this task maps completely: the others hold attachments, recently deleted
// messages and plists, which Task 9 adds (and extends this list to every variant).
var oracleVariants = []string{"ios10-seconds"}

func TestIOSSMSMatchesOracle(t *testing.T) {
	for _, v := range oracleVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := fx.BundleIOSBackup(h, domain, rel)
			p := iossms.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}
