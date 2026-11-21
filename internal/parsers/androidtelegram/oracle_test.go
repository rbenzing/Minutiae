package androidtelegram_test

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 13 (call 8), F4: every variant with records parses to exactly the message records, warnings and notes of its
// literal oracle (the message tables; users is Task 12's), with no contract violation, once pendingOracle has taken
// out what a later task maps. The phone-call rows compare as call records (Task 11). No variant is dropped.
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
			parsertest.AssertMatchesOracle(t, res, b, pendingOracle(fx.Expect), "messages", "messages_v2")
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}

// pendingOracle is the interim filter of TestAndroidTelegramMatchesOracle (F4): a copy of exp without what Task 12
// maps: the users records and the users.data warning (AssertMatchesOracle is given the message tables only).
// Participants, threads, joins, secret chats, calls and Summary are compared in full. No variant is dropped.
func pendingOracle(exp parsertest.Expect) parsertest.Expect {
	var out parsertest.Expect
	bs, err := json.Marshal(exp)
	if err != nil {
		panic(err)
	}
	dec := json.NewDecoder(bytes.NewReader(bs))
	dec.UseNumber() // ids above 2^53 (a pre-84 packed mid) stay exact
	if err := dec.Decode(&out); err != nil {
		panic(err)
	}
	warns := out.Warnings[:0]
	for _, w := range out.Warnings {
		if w.Table == "users" || w.Locator == "sqlite:table=users" {
			continue
		}
		warns = append(warns, w)
	}
	out.Warnings = warns
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// locatorRow is "<table>/<rowid>" of a live locator "sqlite:table=<t>;rowid=<n>[;...]".
func locatorRow(loc string) string {
	rest, _ := strings.CutPrefix(loc, "sqlite:table=")
	table, rest, _ := strings.Cut(rest, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}
