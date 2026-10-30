package iosaddressbook_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold deleted people in the claimed table, with a recover oracle.
var recoverVariants = []string{"deleted"}

func recoverBundle(h *parsertest.Harness, variant string) parsertest.Bundle {
	return parsertest.LoadFixture(h.T, "testdata", variant).BundleIOS(h, remote, logical)
}

// Spec section 14: the claimed table maps its recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant where
// nothing was deleted has no oracle file: it is checked against the empty set, which is a real claim (a typed
// record there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			want := parsertest.RecoverExpect{Variant: v, Claimed: iosaddressbook.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, iosaddressbook.Parser{}, recoverBundle(h, v), want)
		})
	}
}

// 4G-R30 / 4h-R6: the claims are exactly the claimable (total) tables, all of them tables of one mapping.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := iosaddressbook.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := iosaddressbook.ClaimableTables(); !slices.Equal(got, want) || len(got) == 0 {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v", got, want)
	}
	if want := []string{"ABPerson"}; !slices.Equal(iosaddressbook.ClaimableTables(), want) {
		t.Errorf("ClaimableTables() = %v, want %v (4h-R6)", iosaddressbook.ClaimableTables(), want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind. Persons 11 to 35 were deleted
// with their multi-values (whole pages freed); person 20 has no name, organization or multi-value.
func TestRecoverOracleShape(t *testing.T) {
	x := parsertest.LoadRecoverExpect(t, "testdata", "deleted")
	typed := map[int64]bool{}
	for _, r := range x.Records {
		if r.Table != "ABPerson" || r.Rowid == nil || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" ||
			r.Confidence != 60 || !slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Record.Type != "contact" {
			t.Fatalf("deleted lists %+v, want a deleted person from the freelist, basis fit, 60", r)
		}
		typed[*r.Rowid] = true
		pl := r.Record.Payload
		if pl["display_name"] == nil || pl["phones"] != nil || pl["emails"] != nil {
			t.Errorf("person %d: payload %v, want a display name and NO phones or e-mails (the live multi-values are gone)", *r.Rowid, pl)
		}
	}
	for id := int64(11); id <= 35; id++ {
		if want := id != 20; typed[id] != want {
			t.Errorf("person %d typed = %v, want %v", id, typed[id], want)
		}
	}
	if len(typed) != 24 {
		t.Errorf("%d typed persons, want exactly 24", len(typed))
	}
	if len(x.Unmapped) != 1 || x.Unmapped[0].Table != "ABPerson" || x.Unmapped[0].Rowid == nil || *x.Unmapped[0].Rowid != 20 ||
		x.Unmapped[0].Contains != "identifies nobody" {
		t.Errorf("unmapped = %+v, want exactly person 20 (identifies nobody)", x.Unmapped)
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

func firstRecoveredPerson(t *testing.T, h *parsertest.Harness, b parsertest.Bundle) sqlitefile.RecoveredRow {
	t.Helper()
	rr, ok := parsertest.FirstClaimedRow(t, h, iosaddressbook.Parser{}, b, "ABPerson", func(r sqlitefile.RecoveredRow) bool {
		return r.Rowid != nil && *r.Rowid != 20
	})
	if !ok {
		t.Fatal("the fixture delivers no mappable ABPerson row to the typed pass")
	}
	return rr
}

// 4h-R6 / 4E-R25: the photo comes from the images database at live-parse time; the typed pass never reads it, so a
// recovered person carries no photo fact (and so does not claim there is none), while a live parse still states it.
func TestRecoveredPersonCarriesNoPhotoFact(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := recoverBundle(h, "deleted")
	res := parsertest.MapRecoveredRow(t, h, iosaddressbook.Parser{}, b, firstRecoveredPerson(t, h, b))
	if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
		t.Fatalf("%+v, want one record", res)
	}
	if v, has := res.Records[0].Payload["photo"]; has {
		t.Errorf("a recovered person states a photo fact: %v", v)
	}
	// the live view of a database with an images database still states it
	h2 := parsertest.NewHarness(t)
	p := iosaddressbook.Parser{}
	liveBundle := parsertest.LoadFixture(t, "testdata", "with-images").BundleIOSBackup(h2, "HomeDomain", relative)
	live := h2.RunLenient(p, liveBundle.Input(h2, p, false))
	if live.Err != nil || len(live.Records) == 0 {
		t.Fatalf("live parse of with-images: err %v, %d records", live.Err, len(live.Records))
	}
	for _, r := range live.Records {
		if _, has := r.Payload["photo"]; !has {
			t.Errorf("live record %v has no photo fact", r.Payload["source_id"])
		}
	}
}

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	odd := func(row parse.Row) bool { id, ok := row.Rowid(); return ok && id%2 == 1 }
	for _, tc := range []struct {
		fault, want string
		f           func(parse.Row, []records.Record) []records.Record
	}{
		{"drops-display-name", "payload differs", func(_ parse.Row, r []records.Record) []records.Record { delete(r[0].Payload, "display_name"); return r }},
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
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: iosaddressbook.Parser{}, Fault: tc.f}, recoverBundle(h, "deleted"),
				parsertest.LoadRecoverExpect(t, "testdata", "deleted"))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: iosaddressbook.Parser{}}, recoverBundle(h, "deleted"), parsertest.LoadRecoverExpect(t, "testdata", "deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
