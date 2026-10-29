package androidcalllog_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalllog"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// recoverVariants are the fixtures that hold history in the claimed table, with a recover oracle.
var recoverVariants = []string{"calllog-deleted", "calllog-wal-edit"}

// Spec section 14: the claimed table maps its recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant
// where nothing was deleted or edited has no oracle file: it is checked against the empty set, which is a real
// claim (a typed record there would be invented, check b).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range variants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			want := parsertest.RecoverExpect{Variant: v, Claimed: []string{"calls"}}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidcalllog.Parser{}, b, want)
		})
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind.
func TestRecoverOracleShape(t *testing.T) {
	del := parsertest.LoadRecoverExpect(t, "testdata", "calllog-deleted")
	ids := map[int64]bool{}
	for _, r := range del.Records {
		if r.Table != "calls" || r.Method != "sqlite-freelist" || r.Basis != "fit" || r.Relation != "unknown" || r.Confidence != 60 ||
			!slices.Equal(r.Notes, []string{"identity-by-fit-only"}) || r.Rowid == nil || *r.Rowid < 21 || *r.Rowid > 80 {
			t.Fatalf("calllog-deleted lists %+v, want a deleted call (21 to 80) from the freelist, basis fit, 60", r)
		}
		ids[*r.Rowid] = true
	}
	if len(ids) < 30 {
		t.Errorf("calllog-deleted lists %d deleted calls, want at least 30 of the 60 deleted", len(ids))
	}
	if len(del.Digests) != len(del.Records) {
		t.Errorf("%d digests for %d records: every want has a digest", len(del.Digests), len(del.Records))
	}
	edit := parsertest.LoadRecoverExpect(t, "testdata", "calllog-wal-edit")
	seen := map[int64]string{}
	for _, r := range edit.Records {
		if r.Rowid == nil || r.Method != "sqlite-wal-prior" || r.Basis != "schema" || r.Confidence != 75 || (r.File != "db" && r.File != "wal") {
			t.Fatalf("calllog-wal-edit lists %+v, want a version the WAL superseded, basis schema, 75", r)
		}
		if old, dup := seen[*r.Rowid]; dup && old != r.Relation {
			t.Errorf("call %d is listed as %s and as %s", *r.Rowid, old, r.Relation)
		}
		seen[*r.Rowid] = r.Relation
	}
	if seen[3] != "superseded-version" || seen[7] != "absent-from-live" || len(seen) != 2 {
		t.Errorf("calllog-wal-edit lists %v, want call 3 superseded (edited in the WAL) and call 7 absent from live (deleted in the WAL)", seen)
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

// Review Focus 3: the call mapping never reads the rowid, so a recovered call that lost it maps exactly as the
// intact row does: no id is invented into the record (contrast android.contacts, whose source_id is the rowid).
func TestRecoveredCallWithLostRowidMapsWithoutInventingAnId(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "calllog-deleted").Bundle(h, remoteFor("calllog-deleted"))
	rr, ok := parsertest.FirstClaimedRow(t, h, androidcalllog.Parser{}, b, "calls", nil)
	if !ok {
		t.Fatal("the fixture delivers no calls row to the typed pass")
	}
	whole := parsertest.MapRecoveredRow(t, h, androidcalllog.Parser{}, b, rr)
	if whole.Err != nil || whole.Unmapped != "" || len(whole.Records) != 1 {
		t.Fatalf("the intact row: %+v, want one record", whole)
	}
	rr.Rowid = nil
	lost := parsertest.MapRecoveredRow(t, h, androidcalllog.Parser{}, b, rr)
	if lost.Err != nil || lost.Unmapped != "" || len(lost.Records) != 1 {
		t.Fatalf("the row without a rowid: %+v, want one record", lost)
	}
	if !reflect.DeepEqual(whole.Records[0].Payload, lost.Records[0].Payload) || whole.Records[0].Summary != lost.Records[0].Summary {
		t.Errorf("losing the rowid changed the record: intact %v, lost %v", whole.Records[0].Payload, lost.Records[0].Payload)
	}
}

func TestAssertClaimedTablesMapRecoveredRowsCatchesAWrongMapper(t *testing.T) {
	odd := func(row parse.Row) bool { id, ok := row.Rowid(); return ok && id%2 == 1 }
	for _, tc := range []struct {
		fault, variant, want string
		f                    func(parse.Row, []records.Record) []records.Record
	}{
		{"drops-address", "calllog-wal-edit", "payload differs", func(_ parse.Row, r []records.Record) []records.Record { delete(r[0].Payload, "address"); return r }},
		{"adds-record", "calllog-wal-edit", "typed records for the cell", func(_ parse.Row, r []records.Record) []records.Record { return append(r, r[0]) }},
		{"sets-recovery", "calllog-wal-edit", "only the host stamps", func(_ parse.Row, r []records.Record) []records.Record { r[0].Recovery = "sqlite-freelist"; return r }},
		// a fault that corrupts only some records (odd _ids) must be reported too, whatever the oracle lists in full
		{"corrupts-odd-duration", "calllog-deleted", "payload differs", func(row parse.Row, r []records.Record) []records.Record {
			if odd(row) {
				r[0].Payload["duration_s"] = 1
			}
			return r
		}},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", tc.variant).Bundle(h, remoteFor(tc.variant))
			rec := &parsertest.FailureRecorder{TB: t}
			parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: androidcalllog.Parser{}, Fault: tc.f}, b,
				parsertest.LoadRecoverExpect(t, "testdata", tc.variant))
			if got := strings.Join(rec.Errs, "\n"); len(rec.Errs) == 0 || !strings.Contains(got, tc.want) {
				t.Fatalf("the kit did not report the %s fault (wanted %q): %v", tc.fault, tc.want, rec.Errs)
			}
		})
	}
	// the unfaulted mapper passes the same oracle through the same double
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "calllog-deleted").Bundle(h, remoteFor("calllog-deleted"))
	rec := &parsertest.FailureRecorder{TB: t}
	parsertest.AssertClaimedTablesMapRecoveredRows(rec, h, parsertest.FaultyMapper{RowMapper: androidcalllog.Parser{}}, b, parsertest.LoadRecoverExpect(t, "testdata", "calllog-deleted"))
	if len(rec.Errs) != 0 {
		t.Fatalf("the correct mapper was reported: %v", rec.Errs)
	}
}
