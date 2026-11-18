package androidwacontacts_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// recoverVariants are the fixtures that hold history in the claimed table, with a recover oracle.
var recoverVariants = []string{"wa-current", "wa-deleted"}

// Spec section 14: the claimed table maps its recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant where
// nothing was deleted or edited has no oracle file: it is checked against the empty set, which is a real claim (a typed
// record there would be invented). The variants that are never parsed are not run through the typed pass.
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range []string{"wa-2.11", "wa-current", "wa-business", "wa-deleted"} {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			want := parsertest.RecoverExpect{Variant: v, Claimed: androidwacontacts.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidwacontacts.Parser{}, b, want)
		})
	}
}

// The claims are exactly the claimable (total) tables, all of them tables of one mapping.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := androidwacontacts.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := androidwacontacts.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"wa_contacts"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [wa_contacts]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind.
func TestRecoverOracleShape(t *testing.T) {
	del := parsertest.LoadRecoverExpect(t, "testdata", "wa-deleted")
	ids := map[int64]bool{}
	for _, r := range del.Records {
		if r.Table != "wa_contacts" || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" || r.Confidence != 60 ||
			!slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Rowid == nil || *r.Rowid < 21 || *r.Rowid > 80 || r.Record.Type != "contact" {
			t.Fatalf("wa-deleted lists %+v, want a deleted contact (21 to 80) from the freelist, basis fit, 60", r)
		}
		ids[*r.Rowid] = true
	}
	if len(ids) < 30 {
		t.Errorf("wa-deleted lists %d deleted contacts, want at least 30 of the 60 deleted", len(ids))
	}
	if len(del.Digests) != len(del.Records) {
		t.Errorf("%d digests for %d records: every want has a digest", len(del.Digests), len(del.Records))
	}
	edit := parsertest.LoadRecoverExpect(t, "testdata", "wa-current")
	seen := map[int64]string{}
	for _, r := range edit.Records {
		if r.Rowid == nil || r.Method != "sqlite-wal-prior" || r.Basis != "schema" || r.Confidence != 75 || (r.File != "db" && r.File != "wal") {
			t.Fatalf("wa-current lists %+v, want a version the WAL superseded, basis schema, 75", r)
		}
		if old, dup := seen[*r.Rowid]; dup && old != r.Relation {
			t.Errorf("contact %d is listed as %s and as %s", *r.Rowid, old, r.Relation)
		}
		seen[*r.Rowid] = r.Relation
	}
	if seen[13] != "superseded-version" || seen[15] != "absent-from-live" || len(seen) != 2 {
		t.Errorf("wa-current lists %v, want contact 13 superseded (edited in the WAL) and contact 15 absent from live (deleted in the WAL)", seen)
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

// The contact mapping never reads the rowid, so a recovered contact that lost it maps exactly as the intact row does:
// no id is invented into the record.
func TestRecoveredContactWithLostRowidMapsWithoutInventingAnId(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "wa-deleted").Bundle(h, remoteFor("wa-deleted"))
	rr, ok := parsertest.FirstClaimedRow(t, h, androidwacontacts.Parser{}, b, "wa_contacts", nil)
	if !ok {
		t.Fatal("the fixture delivers no wa_contacts row to the typed pass")
	}
	whole := parsertest.MapRecoveredRow(t, h, androidwacontacts.Parser{}, b, rr)
	if whole.Err != nil || whole.Unmapped != "" || len(whole.Records) != 1 {
		t.Fatalf("the intact row: %+v, want one record", whole)
	}
	rr.Rowid = nil
	lost := parsertest.MapRecoveredRow(t, h, androidwacontacts.Parser{}, b, rr)
	if lost.Err != nil || lost.Unmapped != "" || len(lost.Records) != 1 {
		t.Fatalf("the row without a rowid: %+v, want one record", lost)
	}
	if !reflect.DeepEqual(whole.Records[0].Payload, lost.Records[0].Payload) || whole.Records[0].Summary != lost.Records[0].Summary {
		t.Errorf("losing the rowid changed the record: intact %v, lost %v", whole.Records[0].Payload, lost.Records[0].Payload)
	}
	// a recovered record is never presented as live: the host stamps its recovery, the mapper stamps none
	if whole.Records[0].Recovery != "" || whole.Records[0].Deleted {
		t.Errorf("the mapper stamped %q deleted %v: only the host states recovery", whole.Records[0].Recovery, whole.Records[0].Deleted)
	}
}

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	for _, tc := range []struct {
		fault, want string
		f           func(parse.Row, []records.Record) []records.Record
	}{
		{"drops-im", "payload differs", func(_ parse.Row, r []records.Record) []records.Record { delete(r[0].Payload, "ims"); return r }},
		{"adds-record", "typed records for the cell", func(_ parse.Row, r []records.Record) []records.Record { return append(r, r[0]) }},
		{"sets-recovery", "only the host stamps", func(_ parse.Row, r []records.Record) []records.Record { r[0].Recovery = "sqlite-freelist"; return r }},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", "wa-deleted").Bundle(h, remoteFor("wa-deleted"))
			rec := &parsertest.FailureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: androidwacontacts.Parser{}, Fault: tc.f}, b,
				parsertest.LoadRecoverExpect(t, "testdata", "wa-deleted"))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "wa-deleted").Bundle(h, remoteFor("wa-deleted"))
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: androidwacontacts.Parser{}}, b, parsertest.LoadRecoverExpect(t, "testdata", "wa-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
