package safari_test

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold history in the claimed table, with a recover oracle.
var recoverVariants = []string{"deleted"}

func bundleOf(t *testing.T, h *parsertest.Harness, variant string) parsertest.Bundle {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", variant).BundleIOS(h, remoteSafari, logicalSafari)
}

// Spec section 14: the claimed table maps its recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant
// where nothing was deleted or edited has no oracle file: it is checked against the empty set, which is a real
// claim (a typed record there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			want := parsertest.RecoverExpect{Variant: v, Claimed: safari.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, safari.Parser{}, bundleOf(t, h, v), want)
		})
	}
}

// 4G-R30 / 4F-R3: the claims are exactly the claimable (total) tables, all of them tables of one mapping.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := safari.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := safari.ClaimableTables(); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind.
func TestRecoverOracleShape(t *testing.T) {
	x := parsertest.LoadRecoverExpect(t, "testdata", "deleted")
	typed := map[int64]bool{}
	for _, r := range x.Records {
		if r.Table != "history_visits" || r.Rowid == nil || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" ||
			r.Confidence != 60 || !slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Record.Type != "web_visit" {
			t.Fatalf("deleted lists %+v, want a deleted visit from the freelist, basis fit, 60", r)
		}
		typed[*r.Rowid] = true
	}
	// the five visits whose items stay are typed (4F-R4), all of them
	for id := int64(41); id <= 45; id++ {
		if !typed[id] {
			t.Errorf("deleted visit %d (its item stays) is not listed as a typed record", id)
		}
	}
	// the visits whose items are gone have no URL to state: unmapped, never typed
	unmappedIDs := []int64{}
	for _, u := range x.Unmapped {
		if u.Table != "history_visits" || u.Rowid == nil || *u.Rowid < 31 || *u.Rowid > 40 || u.Contains != "history_item" {
			t.Errorf("unmapped %+v: want a visit of 31 to 40 whose item is gone", u)
			continue
		}
		unmappedIDs = append(unmappedIDs, *u.Rowid)
		if typed[*u.Rowid] {
			t.Errorf("visit %d is both typed and unmapped", *u.Rowid)
		}
	}
	slices.Sort(unmappedIDs)
	if want := []int64{31, 32, 33, 37, 38, 39, 40}; !slices.Equal(unmappedIDs, want) {
		t.Errorf("unmapped visits %v, want exactly %v", unmappedIDs, want)
	}
	if len(typed) != 5 {
		t.Errorf("%d typed visits, want exactly 5 (41 to 45)", len(typed))
	}
	if len(x.Digests) != len(x.Records) {
		t.Errorf("%d digests for %d records: every want has a digest", len(x.Digests), len(x.Records))
	}
}

func TestEveryRecoverOracleHasAFixture(t *testing.T) {
	for _, v := range recoverVariants {
		if !slices.Contains(variants, v) {
			t.Errorf("%s has a recover oracle but is not a fixture variant", v)
		}
		parsertest.LoadRecoverExpect(t, "testdata", v) // also checks it was computed from the committed files
	}
}

func firstMappableVisit(t *testing.T, h *parsertest.Harness, b parsertest.Bundle) sqlitefile.RecoveredRow {
	t.Helper()
	rr, ok := parsertest.FirstClaimedRow(t, h, safari.Parser{}, b, "history_visits", func(r sqlitefile.RecoveredRow) bool {
		return r.Rowid != nil && *r.Rowid >= 41 // a visit whose item stays
	})
	if !ok {
		t.Fatal("the fixture delivers no mappable history_visits row to the typed pass")
	}
	return rr
}

func recRaw(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// Review Focus 3: a recovered visit that lost its rowid is mapped without an invented visit id, and says so
// (unknown is never absent).
func TestRecoveredVisitWithLostRowidHasNoVisitIDAndSaysSo(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(t, h, "deleted")
	rr := firstMappableVisit(t, h, b)
	whole := parsertest.MapRecoveredRow(t, h, safari.Parser{}, b, rr)
	if whole.Err != nil || whole.Unmapped != "" || len(whole.Records) != 1 || whole.Records[0].Payload["visit_id"] == nil {
		t.Fatalf("the intact row: %+v, want one record with its visit id", whole)
	}
	rr.Rowid = nil
	lost := parsertest.MapRecoveredRow(t, h, safari.Parser{}, b, rr)
	if lost.Err != nil || lost.Unmapped != "" || len(lost.Records) != 1 {
		t.Fatalf("the row without a rowid: %+v, want one record", lost)
	}
	if v, has := lost.Records[0].Payload["visit_id"]; has {
		t.Errorf("a visit without a rowid states a visit id: %v", v)
	}
	unk, _ := recRaw(lost.Records[0])["unknown_columns"].(map[string]any)
	if unk["id"] != "rowid-lost" {
		t.Errorf("raw.unknown_columns = %v, want id marked rowid-lost", unk)
	}
}

// Program ruling P-4-1: a non-finite REAL in a recovered row is stated by its IEEE bits and an invalid status,
// never as a number (the payload accepts finite numbers only) and never as a time.
func TestRecoveredNonFiniteVisitTimeIsStatedByItsBits(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    float64
		bits string
	}{
		{"+Inf", math.Inf(1), "7ff0000000000000"},
		{"-Inf", math.Inf(-1), "fff0000000000000"},
		{"NaN", math.Float64frombits(0x7ff8000000000001), "7ff8000000000001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(t, h, "deleted")
			isReal := false
			rr := firstMappableVisit(t, h, b)
			for i := range rr.Values {
				if rr.Values[i].Kind == sqlitefile.KindFloat { // visit_time is the table's only REAL
					rr.Values[i].Float, isReal = tc.v, true
				}
			}
			if !isReal {
				t.Fatal("the recovered visit holds no REAL cell")
			}
			res := parsertest.MapRecoveredRow(t, h, safari.Parser{}, b, rr)
			if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
				t.Fatalf("%+v, want one record", res)
			}
			raw := recRaw(res.Records[0])
			if raw["visit_time_ieee"] != tc.bits || raw["visit_time_status"] != "invalid" {
				t.Errorf("raw = %v, want visit_time_ieee %s and visit_time_status invalid", raw, tc.bits)
			}
			if _, has := raw["visit_time"]; has || res.Records[0].Time != nil {
				t.Errorf("a non-finite REAL became a number or a time: raw %v, time %v", raw["visit_time"], res.Records[0].Time)
			}
		})
	}
}

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	odd := func(row parse.Row) bool { id, ok := row.Rowid(); return ok && id%2 == 1 }
	for _, tc := range []struct {
		fault, want string
		f           func(parse.Row, []records.Record) []records.Record
	}{
		{"drops-url", "payload differs", func(_ parse.Row, r []records.Record) []records.Record { delete(r[0].Payload, "url"); return r }},
		{"adds-record", "typed records for the cell", func(_ parse.Row, r []records.Record) []records.Record { return append(r, r[0]) }},
		{"sets-recovery", "only the host stamps", func(_ parse.Row, r []records.Record) []records.Record { r[0].Recovery = "sqlite-freelist"; return r }},
		// a fault that corrupts only some records (odd rowids) must be reported too, whatever the oracle lists in full
		{"corrupts-odd-body", "digest", func(row parse.Row, r []records.Record) []records.Record {
			if odd(row) {
				r[0].Body += " (corrupted)"
			}
			return r
		}},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			rec := &parsertest.FailureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: safari.Parser{}, Fault: tc.f}, bundleOf(t, h, "deleted"),
				parsertest.LoadRecoverExpect(t, "testdata", "deleted"))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: safari.Parser{}}, bundleOf(t, h, "deleted"), parsertest.LoadRecoverExpect(t, "testdata", "deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
