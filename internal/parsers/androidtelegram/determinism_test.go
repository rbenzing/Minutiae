package androidtelegram_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The same input gives the same records, warnings and notes run after run, whatever GOMAXPROCS and time.Local are
// (AssertDeterministic): the counted warnings, constructors_seen (sorted by count then ID) and every raw list come out
// in one order, and no host zone shows in a record. telegram-v181 (with its WAL), telegram-hostile (every blob state)
// and telegram-v64 (the pre-84 conventions).
func TestParseIsDeterministic(t *testing.T) {
	for _, v := range []string{"telegram-v181", "telegram-hostile", "telegram-v64"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteApp)
			p := androidtelegram.Parser{}
			parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
		})
	}
}
