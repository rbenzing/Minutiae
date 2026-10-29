package chromium_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold history in the claimed tables, with a recover oracle.
var recoverVariants = []string{"chromium-deleted"}

// Spec section 14: the claimed tables map their recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant
// where nothing was deleted or edited has no oracle file: it is checked against the empty set, which is a real
// claim (a typed record there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			want := parsertest.RecoverExpect{Variant: v, Claimed: chromium.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, chromium.Parser{}, b, want)
		})
	}
}

// 4G-R30 / 4F-R3: the claims are exactly the claimable (total) tables, all of them tables of one mapping.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := chromium.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := chromium.ClaimableTables(); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind.
func TestRecoverOracleShape(t *testing.T) {
	x := parsertest.LoadRecoverExpect(t, "testdata", "chromium-deleted")
	typed := map[string]map[int64]bool{}
	for _, r := range x.Records {
		if r.Rowid == nil || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" || r.Confidence != 60 ||
			!slices.Equal(r.Notes, []string{"identity-by-fit-only"}) {
			t.Fatalf("chromium-deleted lists %+v, want a row from the freelist, basis fit, 60", r)
		}
		if typed[r.Table] == nil {
			typed[r.Table] = map[int64]bool{}
		}
		typed[r.Table][*r.Rowid] = true
		wantType := map[string]string{"visits": "web_visit", "keyword_search_terms": "web_search", "downloads": "download"}[r.Table]
		if r.Record.Type != wantType {
			t.Errorf("%s %d is a %s record, want %s", r.Table, *r.Rowid, r.Record.Type, wantType)
		}
	}
	// the five visits whose urls rows stay are typed (4F-R4), all of them
	for id := int64(41); id <= 45; id++ {
		if !typed["visits"][id] {
			t.Errorf("deleted visit %d (its urls row stays) is not listed as a typed record", id)
		}
	}
	// the visits whose urls rows are gone have no URL to state: unmapped, never typed
	unmapped := 0
	for _, u := range x.Unmapped {
		if u.Table != "visits" || u.Rowid == nil || *u.Rowid < 31 || *u.Rowid > 40 || u.Contains != "urls" {
			t.Errorf("unmapped %+v: want a visit of 31 to 40 whose urls row is gone", u)
			continue
		}
		unmapped++
		if typed["visits"][*u.Rowid] {
			t.Errorf("visit %d is both typed and unmapped", *u.Rowid)
		}
	}
	if unmapped < 5 {
		t.Errorf("%d visits listed unmapped, want at least 5 of the 10 deleted with their urls rows", unmapped)
	}
	if len(typed["keyword_search_terms"]) < 6 || len(typed["downloads"]) < 5 {
		t.Errorf("typed terms %d and downloads %d, want at least 6 and 5", len(typed["keyword_search_terms"]), len(typed["downloads"]))
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

// Review Focus 3: a recovered visit that lost its rowid is mapped without an invented visit id, and says so.
func TestRecoveredVisitWithLostRowidHasNoVisitIDAndSaysSo(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "chromium-deleted").Bundle(h, remoteFor("chromium-deleted"))
	rr, ok := parsertest.FirstClaimedRow(t, h, chromium.Parser{}, b, "visits", func(r sqlitefile.RecoveredRow) bool {
		return r.Rowid != nil && *r.Rowid >= 41 // a visit whose urls row stays
	})
	if !ok {
		t.Fatal("the fixture delivers no mappable visits row to the typed pass")
	}
	whole := parsertest.MapRecoveredRow(t, h, chromium.Parser{}, b, rr)
	if whole.Err != nil || whole.Unmapped != "" || len(whole.Records) != 1 || whole.Records[0].Payload["visit_id"] == nil {
		t.Fatalf("the intact row: %+v, want one record with its visit id", whole)
	}
	rr.Rowid = nil
	lost := parsertest.MapRecoveredRow(t, h, chromium.Parser{}, b, rr)
	if lost.Err != nil || lost.Unmapped != "" || len(lost.Records) != 1 {
		t.Fatalf("the row without a rowid: %+v, want one record", lost)
	}
	p := lost.Records[0].Payload
	if _, has := p["visit_id"]; has {
		t.Errorf("a visit without a rowid states a visit id: %v", p["visit_id"])
	}
	unk, _ := asMap(p["raw"])["unknown_columns"].(map[string]any)
	if unk["id"] != "rowid-lost" {
		t.Errorf("raw.unknown_columns = %v, want id marked rowid-lost", unk)
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
			b := parsertest.LoadFixture(t, "testdata", "chromium-deleted").Bundle(h, remoteFor("chromium-deleted"))
			rec := &parsertest.FailureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: chromium.Parser{}, Fault: tc.f}, b,
				parsertest.LoadRecoverExpect(t, "testdata", "chromium-deleted"))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "chromium-deleted").Bundle(h, remoteFor("chromium-deleted"))
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: chromium.Parser{}}, b, parsertest.LoadRecoverExpect(t, "testdata", "chromium-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
