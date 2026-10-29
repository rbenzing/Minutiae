package androidcontacts_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// recoverVariants are the fixtures that hold history in the claimed tables, with a recover oracle.
var recoverVariants = []string{"contacts-deleted", "contacts-wal-edit"}

// Spec section 14: the claimed tables map their recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant
// where nothing was deleted has no oracle file: it is checked against the empty set, which is a real claim (a
// typed record there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, fixtureRemote)
			want := parsertest.RecoverExpect{Variant: v, Claimed: []string{"raw_contacts", "deleted_contacts"}}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidcontacts.Parser{}, b, want)
		})
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind.
func TestRecoverOracleShape(t *testing.T) {
	want := parsertest.LoadRecoverExpect(t, "testdata", "contacts-deleted")
	raw := map[int64]bool{}
	for _, r := range want.Records {
		if r.Table != "raw_contacts" || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" || r.Confidence != 60 ||
			!slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Rowid == nil {
			t.Fatalf("contacts-deleted lists %+v, want a raw contact from the freelist, basis fit, 60", r)
		}
		raw[*r.Rowid] = true
		// the data rows of the contact are gone from the live view (4G-R8): no phone, only what the row itself holds
		if _, has := r.Record.Payload["phones"]; has || r.Record.Payload["display_name"] == nil {
			t.Errorf("raw contact %d: payload %v, want its display name and no phones", *r.Rowid, r.Record.Payload)
		}
		if r.Record.Payload["source_id"] != strconv.FormatInt(*r.Rowid, 10) {
			t.Errorf("raw contact %d: source_id %v", *r.Rowid, r.Record.Payload["source_id"])
		}
	}
	if len(raw) < 10 {
		t.Errorf("contacts-deleted lists %d raw contacts, want at least 10", len(raw))
	}
	if len(want.Unmapped) != 1 || want.Unmapped[0].Table != "raw_contacts" || *want.Unmapped[0].Rowid != 12 || want.Unmapped[0].Contains != "identifies nobody" {
		t.Errorf("unmapped %+v, want the nameless raw contact 12", want.Unmapped)
	}
	edit := parsertest.LoadRecoverExpect(t, "testdata", "contacts-wal-edit")
	seen := map[string]string{}
	for _, r := range edit.Records {
		if r.Rowid == nil || r.Method != "sqlite-wal-prior" || r.Basis != "schema" || r.Confidence != 75 || (r.File != "db" && r.File != "wal") {
			t.Fatalf("contacts-wal-edit lists %+v, want a version the WAL superseded, basis schema, 75", r)
		}
		seen[r.Table+"/"+strconv.FormatInt(*r.Rowid, 10)] = r.Relation
	}
	if seen["raw_contacts/2"] != "superseded-version" || seen["deleted_contacts/1001"] != "superseded-version" || seen["deleted_contacts/1002"] != "absent-from-live" || len(seen) != 3 {
		t.Errorf("contacts-wal-edit lists %v, want raw contact 2 and tombstone 1001 superseded and tombstone 1002 absent from live", seen)
	}
}

// A freed [rowid, integer] record (a tombstone) fits several live tables, so the library cannot name its table: the
// 100 tombstones that contacts-deleted frees are not typed, they stay generic db_row records (the oracle wants none and
// check b would refuse an invented typed one).
func TestFreedTombstonesAreNotTypedForWantOfATable(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "contacts-deleted").Bundle(h, fixtureRemote)
	if rr, ok := parsertest.FirstClaimedRow(t, h, androidcontacts.Parser{}, b, "deleted_contacts", nil); ok {
		t.Fatalf("a freed tombstone was delivered to the typed pass as %s (%v): its table is not decidable", rr.Table, rr.TableBasis)
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

// Review Focus 3: a recovered raw contact that identifies nobody (no name, no source id, and its data rows are not
// in the live view) is an unmapped warning, never a record and never a row the writer's rules refuse.
func TestNamelessRecoveredContactIsUnmappedNotRejected(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "contacts-deleted").Bundle(h, fixtureRemote)
	rr, ok := parsertest.FirstClaimedRow(t, h, androidcontacts.Parser{}, b, "raw_contacts", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 12 })
	if !ok {
		t.Fatal("the fixture delivers no recovered raw contact 12 (the nameless one) to the typed pass")
	}
	res := parsertest.MapRecoveredRow(t, h, androidcontacts.Parser{}, b, rr)
	if res.Err != nil || len(res.Records) != 0 || !strings.Contains(res.Unmapped, "identifies nobody") {
		t.Fatalf("the nameless row: %+v, want it unmapped with the reason naming that it identifies nobody", res)
	}
	// and through the whole pass: the kit's strict emitter and the writer's validator refuse nothing, and the
	// oracle's unmapped entry is matched by a reported row (check c)
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, androidcontacts.Parser{}, b, parsertest.LoadRecoverExpect(t, "testdata", "contacts-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the typed pass over contacts-deleted: %v", rec.Errs)
	}
}

// Review Focus 3: the contact mapping reads the rowid (it is the source id and the join key of the data rows), so a
// recovered raw contact that lost it is unmapped with that reason, not mapped through an invented id.
func TestRecoveredContactWithLostRowidIsUnmappedNotDropped(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "contacts-deleted").Bundle(h, fixtureRemote)
	rr, ok := parsertest.FirstClaimedRow(t, h, androidcontacts.Parser{}, b, "raw_contacts", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 7 })
	if !ok {
		t.Fatal("the fixture delivers no recovered raw contact 7 to the typed pass")
	}
	if res := parsertest.MapRecoveredRow(t, h, androidcontacts.Parser{}, b, rr); res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
		t.Fatalf("the intact row: %+v, want one record", res)
	}
	rr.Rowid = nil
	res := parsertest.MapRecoveredRow(t, h, androidcontacts.Parser{}, b, rr)
	if res.Err != nil || len(res.Records) != 0 || !strings.Contains(res.Unmapped, "no rowid") {
		t.Fatalf("the row without a rowid: %+v, want it unmapped with a reason naming the missing rowid", res)
	}
}

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	oddRow := func(row parse.Row) bool { id, ok := row.Rowid(); return ok && id%2 == 1 }
	for _, tc := range []struct {
		fault, want, variant string
		f                    func(parse.Row, []records.Record) []records.Record
	}{
		{"drops-name", "payload differs", "contacts-deleted", func(_ parse.Row, r []records.Record) []records.Record { delete(r[0].Payload, "display_name"); return r }},
		{"adds-record", "typed records for the cell", "contacts-deleted", func(_ parse.Row, r []records.Record) []records.Record { return append(r, r[0]) }},
		{"sets-recovery", "only the host stamps", "contacts-deleted", func(_ parse.Row, r []records.Record) []records.Record { r[0].Recovery = "sqlite-freelist"; return r }},
		// a fault that corrupts only some records (odd ids) must be reported too
		{"corrupts-odd-contact", "payload differs", "contacts-deleted", func(row parse.Row, r []records.Record) []records.Record {
			if oddRow(row) {
				r[0].Payload["source_id"] = "corrupted"
			}
			return r
		}},
		{"corrupts-odd-tombstone", "payload differs", "contacts-wal-edit", func(row parse.Row, r []records.Record) []records.Record {
			if d, _ := r[0].Payload["deleted"].(map[string]any); oddRow(row) && d["source"] == "deleted_contacts" {
				r[0].Payload["source_ids"] = []string{"corrupted"}
			}
			return r
		}},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", tc.variant).Bundle(h, fixtureRemote)
			rec := &parsertest.FailureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: androidcontacts.Parser{}, Fault: tc.f}, b,
				parsertest.LoadRecoverExpect(t, "testdata", tc.variant))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "contacts-deleted").Bundle(h, fixtureRemote)
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: androidcontacts.Parser{}}, b, parsertest.LoadRecoverExpect(t, "testdata", "contacts-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
