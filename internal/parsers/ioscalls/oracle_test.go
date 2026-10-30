package ioscalls_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func TestIOSCallsMatchOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			remote, logical := remotesFor(v)
			b := fx.BundleIOS(h, remote, logical)
			p := ioscalls.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			if len(res.Violations) != 0 {
				t.Errorf("contract violations: %v", res.Violations)
			}
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}

// The oracle states the path of the file that holds the bytes (T48): the WAL-only rows name the -wal.
func TestOracleStatesTheSourcePathOfEveryRow(t *testing.T) {
	for _, v := range variants {
		remote, _ := remotesFor(v)
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			t.Fatalf("%s: no records", v)
		}
		for _, r := range fx.Expect.Records {
			if r.SourcePath != "" && r.SourcePath != remote && r.SourcePath != remote+"-wal" {
				t.Errorf("%s row %d: source_path %q", v, r.Rowid, r.SourcePath)
			}
		}
	}
}
