package androidtelegram_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// Spec 13 (call 8), F4: every variant with records parses to exactly the message records, warnings and notes of its
// literal oracle (the message tables; users is Task 12's), with no contract violation, once pendingOracle has taken
// out what later tasks map. No variant is dropped.
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
			res.Records = dropPendingCalls(t, v, res.Records)
			parsertest.AssertMatchesOracle(t, res, b, pendingOracle(fx.Expect), "messages", "messages_v2")
		})
	}
	if ran != 9 {
		t.Errorf("%d variants with records, want 9", ran)
	}
}

// pendingCallRows are the phone-call service rows (R11-R17, P6, and the layers' service rows that carry a phone-call
// action), which become call records in Task 11: until then they are dropped from both sides (F4), by variant and
// "<table>/<rowid>".
func pendingCallRows() map[string][]string {
	return map[string][]string{
		"telegram-v181": {
			"messages_v2/11", "messages_v2/12", "messages_v2/13", "messages_v2/14", "messages_v2/15", "messages_v2/16",
			"messages_v2/17",
		},
		"telegram-v159": {"messages_v2/11"},
		"telegram-v64":  {"messages/107"},
		"telegram-layers": {
			"messages_v2/1003", "messages_v2/1007", "messages_v2/1008", "messages_v2/1022", "messages_v2/1033",
			"messages_v2/1034", "messages_v2/1042", "messages_v2/1046",
			"messages_v2/1601", // a phone-call reason carrying a secret-kind encrypted_key (Task 10 fix 1, M7)
		},
	}
}

// dropPendingCalls removes the records of the pending call rows from a run (the other side of pendingOracle's drop).
func dropPendingCalls(t *testing.T, variant string, recs []records.Record) []records.Record {
	t.Helper()
	rows := pendingCallRows()[variant]
	var out []records.Record
	dropped := 0
	for _, r := range recs {
		if slices.Contains(rows, locatorRow(r.Locator)) {
			dropped++
			continue
		}
		out = append(out, r)
	}
	if dropped != len(rows) {
		t.Errorf("%s: %d records of the %d pending call rows", variant, dropped, len(rows))
	}
	return out
}

// locatorRow is "<table>/<rowid>" of a live locator "sqlite:table=<t>;rowid=<n>[;...]".
func locatorRow(loc string) string {
	rest, _ := strings.CutPrefix(loc, "sqlite:table=")
	table, rest, _ := strings.Cut(rest, ";rowid=")
	id, _, _ := strings.Cut(rest, ";")
	return table + "/" + id
}

// pendingOracle is the interim filter of TestAndroidTelegramMatchesOracle (F4): a copy of exp without what Tasks 11
// and 12 map. The call records of the pending call rows are dropped (Task 11), as dropPendingCalls drops them from the
// run; the users records and the users.data warning are Task 12's (AssertMatchesOracle is given the message tables
// only). Participants, threads, joins, secret chats and Summary are compared in full (Task 10). No variant is dropped.
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
	calls := pendingCallRows()[exp.Variant]
	recs := out.Records[:0]
	for _, r := range out.Records {
		if r.Table != "users" && slices.Contains(calls, r.Table+"/"+itoa(r.Rowid)) {
			continue
		}
		recs = append(recs, r)
	}
	out.Records = recs
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
