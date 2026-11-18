package ioswacalls_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// runLenient is Harness.RunLenient plus the checks a lenient run would otherwise skip (review M5): no contract violation,
// and every warning is one of the expected kinds (by default only the WAL frames the engine does not apply, which the
// fixtures with a -wal legitimately raise; a test whose store lacks a table passes the counted join warning explicitly). A test
// that expects no warning passes "-" (no warning contains it).
func runLenient(t *testing.T, h *parsertest.Harness, p ioswacalls.Parser, in *parse.Input, allowed ...string) parsertest.Result {
	t.Helper()
	return runLenientWith(t, h, p, in, allowed...)
}

func runLenientWith(t *testing.T, h *parsertest.Harness, p parse.Parser, in *parse.Input, allowed ...string) parsertest.Result {
	t.Helper()
	if len(allowed) == 0 {
		allowed = []string{"wal-frames-not-applied"}
	}
	res := h.RunLenient(p, in)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
	for _, w := range res.Warnings {
		ok := false
		for _, a := range allowed {
			ok = ok || strings.Contains(w.Reason, a)
		}
		if !ok {
			t.Errorf("unexpected warning %s: %s", w.Locator, w.Reason)
		}
	}
	return res
}

// fixtureCalls parses a fixture variant and returns its records by Z_PK.
func fixtureCalls(t *testing.T, v string) (map[int64]records.Record, parsertest.Result) {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
	p := ioswacalls.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied")
	if res.Err != nil {
		t.Fatalf("%s: %v", v, res.Err)
	}
	return byRowid(res), res
}

// Spec 11, 4I2-R4, PF4 (the plan's "single-call aggregate" rule): a call takes its direction only from an aggregate that links
// exactly this one call. Calls 1, 2 and 11 have one; 3 to 5 share an aggregate (multi-call); 6's aggregate says ZINCOMING 7;
// 7 names none; 8 names one that has no row. A store with no Z1CALLEVENTS column states it for every call.
func TestIOSWhatsAppCallDirectionOnlyFromSingleCallAggregate(t *testing.T) {
	recs, _ := fixtureCalls(t, "ios-callhistory")
	dir := func(n int64) any { return recs[n].Payload["direction"] }
	for n, want := range map[int64]string{1: "in", 2: "out", 11: "in"} {
		if dir(n) != want || rawOf(recs[n])["direction_state"] != nil {
			t.Errorf("call %d: direction %v state %v, want %s", n, dir(n), rawOf(recs[n])["direction_state"], want)
		}
	}
	for _, n := range []int64{3, 4, 5} {
		raw := rawOf(recs[n])
		if dir(n) != "unknown" || raw["direction_state"] != "multi-call-aggregate" || raw["aggregate_calls"] != int64(3) {
			t.Errorf("call %d: %v", n, recs[n].Payload)
		}
	}
	if raw := rawOf(recs[6]); dir(6) != "unknown" || raw["direction_state"] != "incoming-unknown" || raw["aggregate"].(map[string]any)["ZINCOMING"] != int64(7) {
		t.Errorf("call 6: %v", recs[6].Payload)
	}
	if raw := rawOf(recs[7]); dir(7) != "unknown" || raw["direction_state"] != "no-aggregate" {
		t.Errorf("call 7: %v", recs[7].Payload)
	}
	if raw := rawOf(recs[8]); dir(8) != "unknown" || raw["direction_state"] != "no-ZWAAGGREGATECALLEVENT-row" {
		t.Errorf("call 8: %v", recs[8].Payload)
	}
	// a direction is never taken from an aggregate that a second call joins later: call 1's aggregate has one call, so the
	// same store with a second call on it states multi-call for both (the count decides, not the aggregate's own columns)
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
	})
	if one, _ := runCalls(t, db, nil); one[1].Payload["direction"] != "in" {
		t.Errorf("control: one call on the aggregate: %v", one[1].Payload)
	}
	db, _ = buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
		tb["ZWACDCALLEVENT"].put(2, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
	})
	for n, r := range func() map[int64]records.Record { m, _ := runCalls(t, db, nil); return m }() {
		if r.Payload["direction"] != "unknown" || rawOf(r)["direction_state"] != "multi-call-aggregate" {
			t.Errorf("two calls on one aggregate, call %d: %v", n, r.Payload)
		}
	}
}

// 4I2-R4: no record has an address; no payload names a participant of the (unjoined) participant table; the note says that
// table exists and is unmapped. The JIDs on the call row itself are raw only.
func TestIOSWhatsAppCallsHaveNoCounterparty(t *testing.T) {
	recs, res := fixtureCalls(t, "ios-callhistory")
	if len(recs) != 16 {
		t.Fatalf("%d records", len(recs))
	}
	for n, r := range recs {
		if _, has := r.Payload["address"]; has {
			t.Errorf("call %d has an address %v", n, r.Payload["address"])
		}
		b, err := json.Marshal(r.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "15550000009@s.whatsapp.net") || strings.Contains(string(b), "15550000001@s.whatsapp.net") {
			t.Errorf("call %d names a participant of the participant table: %s", n, b)
		}
		if r.Payload["service"] != "whatsapp" || r.Payload["kind"] != "unknown" {
			t.Errorf("call %d: service %v kind %v", n, r.Payload["service"], r.Payload["kind"])
		}
	}
	if got := res.Notes["unmapped_tables"]; got != "ZWACDCALLEVENTPARTICIPANT" {
		t.Errorf("note unmapped_tables = %q", got)
	}
	// the call row's own JIDs are raw, as stored
	if raw := rawOf(recs[12]); raw["ZGROUPJIDSTRING"] != "120363000000000001@g.us" || raw["ZGROUPCALLCREATORUSERJIDSTRING"] != "15550000002@s.whatsapp.net" {
		t.Errorf("call 12 raw %v", raw)
	}
	// a Go-built store with the participant table: still no address, the table is named, none of its rows is read
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1)}))
		tb["ZWACDCALLEVENTPARTICIPANT"].put(1, map[string]any{"Z1PARTICIPANTS": int64(1), "ZJIDSTRING": "15550000077@s.whatsapp.net"})
	}, "ZWACDCALLEVENTPARTICIPANT")
	got, res2 := runCalls(t, db, nil)
	b, _ := json.Marshal(got[1].Payload)
	if _, has := got[1].Payload["address"]; has || strings.Contains(string(b), "15550000077") || res2.Notes["unmapped_tables"] != "ZWACDCALLEVENTPARTICIPANT" {
		t.Errorf("participant table: payload %s notes %v", b, res2.Notes)
	}
}

// The oracle of every variant (scenario rows, notes, warnings): every record the generator states is what Parse produces
// (2020-01-06 call 1 and the rest of the scenario of tools/fixtures/whatsapp_ios_calls.py).
func TestIOSWhatsAppCallsMatchesOracle(t *testing.T) {
	compared := 0
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		if len(fx.Expect.Records) == 0 {
			continue
		}
		compared++
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, fx, v)
			p := ioswacalls.Parser{}
			res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied")
			parsertest.AssertMatchesOracle(t, res, b, fx.Expect)
		})
	}
	if compared != 3 {
		t.Errorf("%d variants with records compared, want 3 (every variant but otherapp)", compared)
	}
}
