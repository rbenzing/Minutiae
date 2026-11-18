package ioswacalls_test

import (
	"math"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// parsedVariants are the variants Parse accepts: the typed pass maps their rows. The other one (otherapp) is refused by Parse,
// and a refused database is never typed-recovered.
var parsedVariants = []string{"ios-callhistory", "ios-callhistory-extract", "ios-callhistory-deleted"}

// recoverVariants hold history in ZWACDCALLEVENT and have a recover oracle (PF2).
var recoverVariants = []string{"ios-callhistory-deleted"}

// 4I2-R10: the claims are exactly the claimable tables, which are ZWACDCALLEVENT only: ZWAAGGREGATECALLEVENT is a join of its
// mapping (a recovered aggregate alone is no call) and ZWACDCALLEVENTPARTICIPANT is not read.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := ioswacalls.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := ioswacalls.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"ZWACDCALLEVENT"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [ZWACDCALLEVENT]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// Spec 14, D8 with PF13: every recovered ZWACDCALLEVENT row is a typed record, never a silent drop, checked against an oracle
// computed by an independent page walker from the committed files (a lower bound). A variant where nothing was deleted has no
// oracle file: it is checked against the empty set, which is a real claim (a typed record there would be invented).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			want := parsertest.RecoverExpect{Variant: v, Claimed: ioswacalls.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, ioswacalls.Parser{}, b, want)
		})
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind (a WAL frame that deleted rows), and every
// want has a digest.
func TestRecoverOracleShape(t *testing.T) {
	for _, v := range recoverVariants {
		x := parsertest.LoadRecoverExpect(t, "testdata", v)
		if len(x.Records) < 5 || len(x.Digests) < len(x.Records) {
			t.Errorf("%s lists %d records and %d digests: the check proves nothing", v, len(x.Records), len(x.Digests))
		}
		inWAL := 0
		for _, r := range x.Records {
			if r.Table != "ZWACDCALLEVENT" || r.Record.Type != "call" {
				t.Fatalf("%s lists %+v: a typed record of another table or type", v, r)
			}
			if r.Method == "sqlite-wal-prior" {
				inWAL++
			}
		}
		if inWAL == 0 {
			t.Errorf("%s lists no version the WAL deleted", v)
		}
	}
}

// 4I2-P8 and PF5: a recovered row has no Parse context. At a backup domain its place names the app; at an app group the
// metadata plist is never evaluated for it, so it states app unknown and app_state not-evaluated, never a guessed label. The
// deleted fixture bundled at an app group is the same database, so only the place differs.
func TestIOSWhatsAppCallsRecoveredAppGroupRowIsNotEvaluated(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios-callhistory-deleted")
	p := ioswacalls.Parser{}
	for _, c := range []struct {
		name, app, basis, state string
		bundle                  func(h *parsertest.Harness) parsertest.Bundle
	}{
		{"domain", "whatsapp", "backup_domain", "", func(h *parsertest.Harness) parsertest.Bundle { return bundleOf(h, fx, "ios-callhistory-deleted") }},
		{"app group", "unknown", "", "not-evaluated", func(h *parsertest.Harness) parsertest.Bundle {
			return parsertest.Fixture{Name: "group", Files: fx.Files}.BundleIOS(h, extractRemote, extractLogical)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := c.bundle(h)
			rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "ZWACDCALLEVENT", nil)
			if !ok {
				t.Fatal("no recovered ZWACDCALLEVENT row: the check proves nothing")
			}
			res := parsertest.MapRecoveredRow(t, h, p, b, rr)
			if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
				t.Fatalf("err %v, unmapped %q, %d records", res.Err, res.Unmapped, len(res.Records))
			}
			raw := rawOf(res.Records[0])
			if raw["app"] != c.app || raw["app_basis"] != orAbsent(c.basis) || raw["app_state"] != orAbsent(c.state) {
				t.Errorf("app %v basis %v state %v, want %q %q %q (\"\" = absent)", raw["app"], raw["app_basis"], raw["app_state"], c.app, c.basis, c.state)
			}
		})
	}
}

// orAbsent is the value a missing map key reads as for want: nil for "".
func orAbsent(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Review Focus 4, 4I2-P8: a recovered call whose aggregate links exactly one LIVE call that is another row never borrows that
// direction (a live link says nothing about a deleted row). Call 90 of ios-callhistory-deleted is deleted and names aggregate A1,
// whose only live call is call 1 (a "in" call when parsed live): the recovered row states direction unknown and
// direction_state recovered-row, and no aggregate row.
func TestIOSWhatsAppCallsRecoveredRowNeverBorrowsALiveAggregate(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "ios-callhistory-deleted")
	b := bundleOf(h, fx, "ios-callhistory-deleted")
	p := ioswacalls.Parser{}
	// the live control: call 1 is the one live call of A1 and has a direction
	live, _ := fixtureCalls(t, "ios-callhistory-deleted")
	if live[1].Payload["direction"] != "in" {
		t.Fatalf("control: live call 1: %v", live[1].Payload)
	}
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "ZWACDCALLEVENT", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 90 })
	if !ok {
		t.Fatal("recovered call 90 not found")
	}
	res := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
		t.Fatalf("err %v, unmapped %q, %d records", res.Err, res.Unmapped, len(res.Records))
	}
	pl, raw := res.Records[0].Payload, rawOf(res.Records[0])
	if pl["direction"] != "unknown" || raw["direction_state"] != "recovered-row" || raw["aggregate"] != nil || raw["Z1CALLEVENTS"] != int64(1) || pl["outcome"] != "answered" {
		t.Errorf("recovered call 90: payload %v", pl)
	}
	if _, has := pl["address"]; has {
		t.Errorf("recovered call 90 has an address: %v", pl)
	}
}

// the Task 7 README note: the oracle's page walker types a whole-number REAL as an integer, so its deleted rows carry fractions;
// a recovered row whose ZDURATION is a whole-number REAL is covered here, on a copy of a recovered row with the value replaced.
func TestIOSWhatsAppCallsRecoveredWholeNumberRealDuration(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-callhistory-deleted"), "ios-callhistory-deleted")
	p := ioswacalls.Parser{}
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "ZWACDCALLEVENT", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 90 })
	if !ok {
		t.Fatal("recovered call 90 not found")
	}
	// the column order of tools/fixtures/schemas/whatsapp-ios/callhistory.sql: Z_PK, Z_ENT, Z_OPT, Z1CALLEVENTS, ZBYTESRECEIVED,
	// ZBYTESSENT, ZOUTCOME, ZVIDEO, ZDATE, ZDURATION
	const di = 9
	if len(rr.Values) <= di || rr.Values[di].Kind != sqlitefile.KindFloat || rr.Values[di].Float != 30.5 {
		t.Fatalf("the recovered call 90 does not hold ZDURATION 30.5 in column %d: %+v", di, rr.Values)
	}
	realVal := func(f float64) sqlitefile.Value {
		return sqlitefile.Value{Kind: sqlitefile.KindFloat, Float: f, Serial: 7}
	}
	for name, c := range map[string]struct {
		v    sqlitefile.Value
		want any
		end  bool
	}{
		"whole REAL": {realVal(30), 30.0, true},
		"integer":    {sqlitefile.Value{Kind: sqlitefile.KindInt, Int: 30, Serial: 1}, 30.0, true},
		"fractional": {realVal(30.5), 30.5, true},
		"zero":       {realVal(0), 0.0, false},
		"huge":       {realVal(1e300), 1e300, false},
		"not finite": {realVal(math.Inf(1)), nil, false},
		"negative":   {realVal(-2), nil, false},
	} {
		row := rr
		row.Values = slices.Clone(rr.Values)
		row.Values[di] = c.v
		res := parsertest.MapRecoveredRow(t, h, p, b, row)
		if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
			t.Errorf("%s: err %v, unmapped %q, %d records", name, res.Err, res.Unmapped, len(res.Records))
			continue
		}
		rec := res.Records[0]
		if rec.Payload["duration_s"] != c.want || (rec.TimeEnd != nil) != c.end {
			t.Errorf("%s: duration_s %v (want %v), time end %v (want one: %v)", name, rec.Payload["duration_s"], c.want, rec.TimeEnd, c.end)
		}
	}
}
