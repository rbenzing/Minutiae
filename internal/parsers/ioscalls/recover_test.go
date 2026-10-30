package ioscalls_test

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold history in a claimed table, with a recover oracle.
var recoverVariants = []string{"coredata-deleted", "legacy-deleted"}

func bundleOf(t *testing.T, h *parsertest.Harness, variant string) parsertest.Bundle {
	t.Helper()
	remote, logical := remotesFor(variant)
	return parsertest.LoadFixture(t, "testdata", variant).BundleIOS(h, remote, logical)
}

// Spec section 14: the claimed tables map their recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant
// where nothing was deleted or edited has no oracle file: it is checked against the empty set, which is a real
// claim (a typed record there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			want := parsertest.RecoverExpect{Variant: v, Claimed: ioscalls.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, ioscalls.Parser{}, bundleOf(t, h, v), want)
		})
	}
}

// 4G-R30 / 4h-R6: the claims are exactly the claimable (total) tables, all of them tables of one mapping each.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := ioscalls.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := ioscalls.ClaimableTables(); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v", got, want)
	}
	if want := []string{"ZCALLRECORD", "call"}; !slices.Equal(ioscalls.ClaimableTables(), want) {
		t.Errorf("ClaimableTables = %v, want %v", ioscalls.ClaimableTables(), want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind. Rows 21 to 50 were deleted from a
// table of 60 rows on 1024-byte pages; the walker finds the rows of the freed whole leaf pages only (the rebalance
// after the delete rewrites the page that keeps live cells, which loses the stale cells at the start of the run), so
// the listed rowids are one run of whole pages ending at the last deleted row. A call joins nothing live, so every
// recovered row is typed and nothing is unmapped.
func TestRecoverOracleShape(t *testing.T) {
	for _, tc := range []struct {
		variant, table string
		first          int64
	}{
		{"coredata-deleted", "ZCALLRECORD", 29},
		{"legacy-deleted", "call", 31},
	} {
		t.Run(tc.variant, func(t *testing.T) {
			x := parsertest.LoadRecoverExpect(t, "testdata", tc.variant)
			var ids []int64
			for _, r := range x.Records {
				if r.Table != tc.table || r.Rowid == nil || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" ||
					r.Confidence != 60 || !slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Record.Type != "call" {
					t.Fatalf("%s lists %+v, want a deleted call from the freelist, basis fit, 60", tc.variant, r)
				}
				ids = append(ids, *r.Rowid)
			}
			slices.Sort(ids)
			var want []int64
			for id := tc.first; id <= 50; id++ {
				want = append(want, id)
			}
			if !slices.Equal(ids, want) {
				t.Errorf("typed %s rowids %v, want exactly %d to 50", tc.table, ids, tc.first)
			}
			if len(x.Unmapped) != 0 {
				t.Errorf("unmapped %+v: a call needs no live join, nothing is unmapped", x.Unmapped)
			}
			if len(x.Digests) != len(x.Records) {
				t.Errorf("%d digests for %d records: every want has a digest", len(x.Digests), len(x.Records))
			}
		})
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

func recRaw(r records.Record) map[string]any {
	m, _ := r.Payload["raw"].(map[string]any)
	return m
}

// Program ruling P-4-1: a non-finite REAL in a recovered row is stated by its IEEE bits and an invalid status,
// never as a number (the payload accepts finite numbers only) and never as a duration.
func TestRecoveredNonFiniteCallDurationIsStatedByItsBits(t *testing.T) {
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
			b := bundleOf(t, h, "coredata-deleted")
			rr, ok := parsertest.FirstClaimedRow(t, h, ioscalls.Parser{}, b, "ZCALLRECORD", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil })
			if !ok {
				t.Fatal("the fixture delivers no mappable ZCALLRECORD row to the typed pass")
			}
			isReal := false
			for i := range rr.Values {
				if rr.Values[i].Kind == sqlitefile.KindFloat { // ZDURATION is the table's only REAL in this fixture
					rr.Values[i].Float, isReal = tc.v, true
				}
			}
			if !isReal {
				t.Fatal("the recovered call holds no REAL cell")
			}
			res := parsertest.MapRecoveredRow(t, h, ioscalls.Parser{}, b, rr)
			if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
				t.Fatalf("%+v, want one record", res)
			}
			raw := recRaw(res.Records[0])
			if raw["ZDURATION_ieee"] != tc.bits || raw["ZDURATION_status"] != "invalid" {
				t.Errorf("raw = %v, want ZDURATION_ieee %s and ZDURATION_status invalid", raw, tc.bits)
			}
			if _, has := raw["ZDURATION"]; has {
				t.Errorf("a non-finite REAL became a number: raw.ZDURATION = %v", raw["ZDURATION"])
			}
			if _, has := res.Records[0].Payload["duration_s"]; has {
				t.Errorf("a non-finite REAL became a duration: %v", res.Records[0].Payload["duration_s"])
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
		{"drops-address", "payload differs", func(_ parse.Row, r []records.Record) []records.Record { delete(r[0].Payload, "address"); return r }},
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
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: ioscalls.Parser{}, Fault: tc.f}, bundleOf(t, h, "coredata-deleted"),
				parsertest.LoadRecoverExpect(t, "testdata", "coredata-deleted"))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: ioscalls.Parser{}}, bundleOf(t, h, "coredata-deleted"), parsertest.LoadRecoverExpect(t, "testdata", "coredata-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
