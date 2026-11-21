package androidgooglemessages_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The same input gives the same records, warnings and notes run after run, whatever GOMAXPROCS and time.Local are
// (AssertDeterministic): gm-aosp-v2, gm-hostile (the caps and every cell state) and gm-aosp-v2-wal.
func TestParseIsDeterministic(t *testing.T) {
	for _, v := range []string{"gm-aosp-v2", "gm-hostile", "gm-aosp-v2-wal"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteApp)
			p := androidgooglemessages.Parser{}
			parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
		})
	}
}
