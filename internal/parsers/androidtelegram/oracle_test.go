package androidtelegram_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 13 (call 8), F4: every variant with records parses to exactly the message, call and contact records, warnings
// and notes of its literal oracle, with no contract violation (the users table included, Task 12). The phone-call
// rows compare as call records (Task 11). No variant is dropped.
func TestAndroidTelegramMatchesOracle(t *testing.T) {
	ran := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue // refused (version band, no message table): the probe and parse tests state those
		}
		ran++
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := fx.Bundle(h, remoteApp)
			p := androidtelegram.Parser{}
			res := h.RunLenient(p, b.Input(h, p, false))
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// locatorRow is "<table>/<rowid>" of a live locator "sqlite:table=<t>;rowid=<n>[;...]".
func locatorRow(loc string) string {
	rest, _ := strings.CutPrefix(loc, "sqlite:table=")
	table, rest, _ := strings.Cut(rest, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}
