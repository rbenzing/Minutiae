package ioswacalls_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The generator (tools/fixtures/whatsapp_ios_calls.py) is the scenario oracle; these literal facts are written from the spec
// (6.11, 7.4, 7.6), the rulings 4I2-R4, R5, R7, PF4 and PF6, never read back from the generator, so a generator that drifted
// from the rules fails here before any parser exists. The oracle and these facts are two readings of the same spec by one
// author (4I2-R13): they catch a divergence between the text and the implementation, not a reading both share.

type callRecs struct {
	t  *testing.T
	by map[int64]parsertest.ExpRecord
}

func recsOf(t *testing.T, variant string) (callRecs, parsertest.Fixture) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	c := callRecs{t: t, by: map[int64]parsertest.ExpRecord{}}
	for _, r := range fx.Expect.Records {
		c.by[r.Rowid] = r
		if r.Table != "ZWACDCALLEVENT" || r.Type != "call" {
			t.Errorf("%s: record %s/%d of type %q", variant, r.Table, r.Rowid, r.Type)
		}
	}
	return c, fx
}

// rec is one record view.
type rec struct {
	parsertest.ExpRecord
	raw map[string]any
}

func (c callRecs) rec(n int64) rec {
	c.t.Helper()
	r, ok := c.by[n]
	if !ok {
		c.t.Fatalf("no oracle record for ZWACDCALLEVENT %d", n)
	}
	raw, _ := r.Payload["raw"].(map[string]any)
	return rec{ExpRecord: r, raw: raw}
}

func (r rec) direction() any { return r.Payload["direction"] }
func (r rec) outcome() any   { return r.Payload["outcome"] }
func (r rec) kind() any      { return r.Payload["kind"] }

// rawIs compares a raw value with want as printed (JSON numbers decode as json.Number).
func (r rec) rawIs(key string, want any) bool {
	v, has := r.raw[key]
	return has && fmt.Sprint(v) == fmt.Sprint(want)
}

func (r rec) rawAbsent(key string) bool { _, has := r.raw[key]; return !has }

func (r rec) timeIs(want string) bool { return r.Time != nil && *r.Time == want }

func (r rec) timeEndIs(want string) bool { return r.TimeEnd != nil && *r.TimeEnd == want }

func (r rec) aggregate() map[string]any {
	a, _ := r.raw["aggregate"].(map[string]any)
	return a
}

func (r rec) hasDuration() bool { _, has := r.Payload["duration_s"]; return has }

func TestIOSWhatsAppCallsOracleStatesTheSpecRules(t *testing.T) {
	c, fx := recsOf(t, "ios-callhistory")
	want := func(ok bool, format string, args ...any) {
		t.Helper()
		if !ok {
			t.Errorf(format, args...)
		}
	}
	_, has16 := c.by[16]
	want(len(c.by) == 15 && !has16, "ios-callhistory has %d records, want 15 (calls 1-14 in the file, 15 in a committed WAL frame, 16 in an uncommitted one: no record)", len(c.by))

	// call 1: the single-call aggregate A1 says incoming (ZINCOMING 1 = in, S1 and S7); ZOUTCOME 0 = answered; ZVIDEO is not
	// mapped (PF6: no per-value source), so the kind is unknown and the stored value is in raw
	c1 := c.rec(1)
	d1, _ := c1.Payload["duration_s"].(json.Number)
	want(c1.direction() == "in" && c1.outcome() == "answered" && c1.kind() == "unknown" && c1.rawIs("ZVIDEO", 0) && d1.String() == "61.5", "call 1: %v", c1.Payload)
	want(c1.timeIs("2020-01-06T10:43:20Z") && c1.timeEndIs("2020-01-06T10:44:21.5Z") && c1.raw["time_end_basis"] == "start+duration", "call 1 times %v %v raw %v", c1.Time, c1.TimeEnd, c1.raw)
	want(c1.aggregate() != nil && fmt.Sprint(c1.aggregate()["Z_PK"]) == "1" && fmt.Sprint(c1.aggregate()["ZINCOMING"]) == "1", "call 1 aggregate %v", c1.raw["aggregate"])
	// a duration of 0 is a stated duration and derives no end
	want(c.rec(2).hasDuration() && c.rec(2).TimeEnd == nil && c.rec(2).rawAbsent("time_end_basis"), "call 2 duration %v", c.rec(2).Payload)

	// call 2: A2 says outgoing; a missed code on an outgoing call is not "missed" (the label is kept in raw)
	want(c.rec(2).direction() == "out" && c.rec(2).outcome() == "unknown" && c.rec(2).raw["label_ZOUTCOME"] == "missed", "call 2: %v", c.rec(2).Payload)

	// calls 3 to 5 share the aggregate A3: no direction is borrowed from an aggregate that links three calls
	for _, id := range []int64{3, 4, 5} {
		r := c.rec(id)
		want(r.direction() == "unknown" && r.raw["direction_state"] == "multi-call-aggregate" && r.rawIs("aggregate_calls", 3) && r.rawAbsent("aggregate") &&
			r.outcome() == "unknown" && r.raw["label_ZOUTCOME"] == "missed", "call %d: %v", id, r.Payload)
	}

	// call 6: an aggregate whose ZINCOMING is neither 0 nor 1; call 7 names no aggregate; call 8 one that has no row
	c6 := c.rec(6)
	want(c6.direction() == "unknown" && c6.raw["direction_state"] == "incoming-unknown" && fmt.Sprint(c6.aggregate()["ZINCOMING"]) == "7" && c6.outcome() == "answered", "call 6: %v", c6.Payload)
	want(c.rec(7).direction() == "unknown" && c.rec(7).raw["direction_state"] == "no-aggregate" && c.rec(7).rawAbsent("Z1CALLEVENTS"), "call 7: %v", c.rec(7).Payload)
	want(c.rec(8).direction() == "unknown" && c.rec(8).raw["direction_state"] == "no-ZWAAGGREGATECALLEVENT-row" && c.rec(8).rawIs("Z1CALLEVENTS", 99), "call 8: %v", c.rec(8).Payload)

	// ZOUTCOME 1 on an incoming call is missed; the codes 2, 4 and 99 are no outcome and carry no label
	want(c.rec(11).direction() == "in" && c.rec(11).outcome() == "missed" && c.rec(11).rawAbsent("label_ZOUTCOME"), "call 11: %v", c.rec(11).Payload)
	want(c.rec(9).outcome() == "unknown" && c.rec(9).raw["label_ZOUTCOME"] == nil && c.rec(9).rawIs("ZOUTCOME", 2), "call 9: %v", c.rec(9).Payload)
	want(c.rec(10).outcome() == "unknown" && c.rec(10).raw["label_ZOUTCOME"] == nil && c.rec(10).rawIs("ZOUTCOME", 4), "call 10: %v", c.rec(10).Payload)
	want(c.rec(15).outcome() == "unknown" && c.rec(15).raw["label_ZOUTCOME"] == nil && c.rec(15).rawIs("ZOUTCOME", 99) && c.rec(15).direction() == "in", "call 15: %v", c.rec(15).Payload)

	// a group call: the JIDs on the call row are raw only (4I2-R4); an outgoing answered call
	c12 := c.rec(12)
	want(c12.raw["ZGROUPJIDSTRING"] == "120363000000000001@g.us" && c12.raw["ZGROUPCALLCREATORUSERJIDSTRING"] == "15550000002@s.whatsapp.net" && c12.raw["ZCALLIDSTRING"] == "C12",
		"call 12 raw %v", c12.raw)
	want(c12.direction() == "out" && c12.outcome() == "answered" && c12.kind() == "unknown" && c12.rawIs("ZVIDEO", 1) && c12.rawIs("ZBYTESSENT", 1024) && c12.rawIs("ZBYTESRECEIVED", 2048),
		"call 12: %v", c12.Payload)
	// ZVIDEO 2 is no kind either
	want(c.rec(14).kind() == "unknown" && c.rec(14).rawIs("ZVIDEO", 2), "call 14: %v", c.rec(14).Payload)

	// a NULL ZDATE gives no time; a negative ZDURATION gives no duration and no end (stored in raw with its status)
	c13 := c.rec(13)
	want(c13.Time == nil && c13.TimeEnd == nil && !c13.hasDuration() && c13.rawIs("ZDURATION", -5) && c13.raw["ZDURATION_status"] == "negative" && c13.rawAbsent("ZDATE"), "call 13: %v %v", c13.Payload, c13.Time)

	// every record is a WhatsApp call with no counterparty (4I2-R4): the participant table is not joined, so the participant
	// JIDs appear nowhere
	for id, r := range c.by {
		_, hasAddr := r.Payload["address"]
		want(!hasAddr && r.Payload["service"] == "whatsapp", "call %d has address %v service %v", id, r.Payload["address"], r.Payload["service"])
		raw, _ := json.Marshal(r)
		want(!strings.Contains(string(raw), "15550000009") && !strings.Contains(string(raw), "15550000001@"), "call %d names a participant JID: %s", id, raw)
		rr := c.rec(id)
		want(rr.raw["app"] == "whatsapp" && rr.raw["app_basis"] == "backup_domain", "call %d app %v", id, rr.raw)
	}

	// notes and warnings: the participant table is named as unmapped, 12 distinct aggregate keys are read (1, 2, 3, 4, 6 to 12
	// and 99: the NULL key of call 7 is no key), and the one warning is the reader's (the uncommitted WAL frames of call 16)
	want(fx.Expect.Notes["container"] == "backup_domain" && fx.Expect.Notes["app"] == "whatsapp" && fx.Expect.Notes["app_basis"] == "backup_domain", "notes %v", fx.Expect.Notes)
	want(fx.Expect.Notes["unmapped_tables"] == "ZWACDCALLEVENTPARTICIPANT" && fx.Expect.Notes["aggregates"] == "12", "notes %v", fx.Expect.Notes)
	want(len(fx.Expect.Warnings) == 1 && strings.Contains(fx.Expect.Warnings[0].Contains, "wal-frames-not-applied"), "warnings %+v", fx.Expect.Warnings)
	want(len(fx.Files) == 2, "files %d, want the database and its -wal", len(fx.Files))
}

func TestIOSWhatsAppCallsOracleStatesTheApp(t *testing.T) {
	c, fx := recsOf(t, "ios-callhistory-extract")
	r1, r2 := c.rec(1), c.rec(2)
	// an extraction names no app: the random container name is no evidence, and no container metadata is held (4I2-R5)
	if len(c.by) != 2 || r1.raw["app"] != "unknown" || r1.raw["app_state"] != "no-container-metadata" || !r1.rawAbsent("app_basis") || fx.Expect.Notes["container"] != "app_group" {
		t.Errorf("extract: %d records, raw %v, notes %v", len(c.by), r1.raw, fx.Expect.Notes)
	}
	// the single-call aggregates decide the direction in an app group too
	if r1.direction() != "in" || r1.outcome() != "answered" || r2.direction() != "out" || r2.outcome() != "unknown" || fx.Expect.Notes["aggregates"] != "2" {
		t.Errorf("extract directions: %v %v notes %v", r1.Payload, r2.Payload, fx.Expect.Notes)
	}
	if len(fx.Expect.ExtraFiles) != 0 || len(fx.Expect.Warnings) != 0 {
		t.Errorf("an extraction variant has extras %v and warnings %v", fx.Expect.ExtraFiles, fx.Expect.Warnings)
	}
	c, fx = recsOf(t, "ios-callhistory-otherapp")
	if len(c.by) != 0 || len(fx.Expect.Notes) != 0 {
		t.Errorf("otherapp states %d records and notes %v; the parser refuses it", len(c.by), fx.Expect.Notes)
	}
}

// TestIOSWhatsAppCallsDeletedOracleStatesTheHistory (plan 4I2 PF2, Review Focus 4): deleted calls are found through the WAL
// (the file holds 90 and 91, a first WAL frame 92 and 93, one committed WAL transaction deletes them with the even ones of
// 100 to 159); call 90 is on aggregate A1, whose one live call is call 1, and no recovered call borrows a direction from it.
func TestIOSWhatsAppCallsDeletedOracleStatesTheHistory(t *testing.T) {
	c, fx := recsOf(t, "ios-callhistory-deleted")
	if len(c.by) != 32 || fx.Expect.Notes["aggregates"] != "2" {
		t.Errorf("%d live records, notes %v, want 32 (calls 1, 2 and the 30 odd ones of 100-159) and two aggregate keys", len(c.by), fx.Expect.Notes)
	}
	if c.rec(1).direction() != "in" || c.rec(2).direction() != "out" {
		t.Errorf("live calls 1 and 2: %v %v", c.rec(1).Payload, c.rec(2).Payload)
	}
	raw, err := os.ReadFile("testdata/ios-callhistory-deleted.recover.expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Claimed []string `json:"claimed"`
		Records []struct {
			Table, Method, Relation, File string
			Rowid                         int64
			Record                        struct {
				Deleted bool
				Payload map[string]any
			}
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	if len(exp.Claimed) != 1 || exp.Claimed[0] != "ZWACDCALLEVENT" {
		t.Errorf("claimed %v", exp.Claimed)
	}
	rows := map[int64]bool{}
	files := map[string]bool{}
	for _, r := range exp.Records {
		rows[r.Rowid] = true
		files[r.File] = true
		if r.Table != "ZWACDCALLEVENT" || r.Method != "sqlite-wal-prior" || r.Relation != "absent-from-live" {
			t.Errorf("row %d: %+v", r.Rowid, r)
		}
		// a recovered call states no direction, whatever its aggregate says (Review Focus 4), and no counterparty
		rawp, _ := r.Record.Payload["raw"].(map[string]any)
		if r.Record.Payload["direction"] != "unknown" || rawp["direction_state"] != "recovered-row" || rawp["aggregate"] != nil || r.Record.Payload["address"] != nil || r.Record.Deleted {
			t.Errorf("recovered call %d: %v deleted %v", r.Rowid, r.Record.Payload, r.Record.Deleted)
		}
		if r.Rowid == 90 && (fmt.Sprint(rawp["Z1CALLEVENTS"]) != "1" || r.Record.Payload["outcome"] != "answered") {
			t.Errorf("recovered call 90 is on aggregate A1 and answered: %v", r.Record.Payload)
		}
	}
	for n := int64(1); n <= 200; n++ {
		want := n == 90 || n == 91 || n == 92 || n == 93 || (n >= 100 && n <= 159 && n%2 == 0)
		if rows[n] != want {
			t.Errorf("row %d recovered = %v, want %v", n, rows[n], want)
		}
	}
	if !files["db"] || !files["wal"] {
		t.Errorf("history in %v, want both the main file and the WAL", files)
	}
}
