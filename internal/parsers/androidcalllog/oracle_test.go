package androidcalllog_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func TestCallsMatchOracle(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			fx := parsertest.LoadFixture(t, "testdata", v)
			b := fx.Bundle(h, remoteFor(v))
			p := androidcalllog.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
}

// The oracle states the path of the file that holds the bytes (T48, T51): every row of a
// lone-database variant names the variant's own remote path, so a parser that fixed one
// primary path would fail on in-contacts2. A variant with a WAL states none: the path follows
// the role of the file that holds the cell.
func TestOracleStatesTheSourcePathOfEveryRow(t *testing.T) {
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			t.Fatalf("%s: no records", v)
		}
		for _, r := range fx.Expect.Records {
			want := remoteFor(v)
			if len(fx.Files) > 1 {
				want = ""
			}
			if r.SourcePath != want || r.Type != "call" {
				t.Errorf("%s row %d: source_path %q type %q", v, r.Rowid, r.SourcePath, r.Type)
			}
		}
	}
}
