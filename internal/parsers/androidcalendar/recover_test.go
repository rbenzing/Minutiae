package androidcalendar_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// The claims are exactly the claimable (total) tables: Events, every row of which is a record (spec 10). The join
// tables, the derived Instances cache, Colors and the meta tables are never claimed.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := androidcalendar.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := androidcalendar.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"Events"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [Events]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
	if err := parse.ValidateMeta(p.Meta()); err != nil {
		t.Errorf("Meta with its claims: %v", err)
	}
}

// Spec 10 and 4I2 PF13: the claimed Events table maps its recovered rows, checked against the oracle that the generator
// computed with an independent page walker from the committed calendar-deleted files (a lower bound: every recovered
// Events row is a typed record). A variant with no oracle file and no history is checked against the empty set, a real
// claim (a typed record there would be invented). calendar-601 holds history but has no oracle: its WAL holds row 43
// in a frame that was never committed, checked by TestCalendarUncommittedWALRowIsTypedNotLive.
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range recordedVariants {
		if v == "calendar-601" {
			continue
		}
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteProvider)
			want := parsertest.RecoverExpect{Variant: v, Claimed: androidcalendar.ClaimableTables()}
			if v == "calendar-deleted" {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
				if len(want.Records) == 0 {
					t.Fatal("the calendar-deleted recover oracle lists no record: the check proves nothing")
				}
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidcalendar.Parser{}, b, want)
		})
	}
}

// B2 (4I3-P4): a recovered row joins no table, even when rows that name it survive in the live view. Recovered row 50
// of calendar-deleted has calendar_id 1 (a live Calendars row) and an Attendees and a Reminders row written after the
// delete (F17); its record carries no calendar, no attendees and no reminders, and every join-derived state is
// stated "not-joined-recovered", never absent. Its own deleted and lastSynced flags are stated as in a live parse.
func TestCalendarRecoveredRowsJoinNothing(t *testing.T) {
	const notJoined = "not-joined-recovered" // pinned literally (F20)
	h := parsertest.NewHarness(t)
	p := androidcalendar.Parser{}
	b := parsertest.LoadFixture(t, "testdata", "calendar-deleted").Bundle(h, remoteProvider)
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "Events", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 50 })
	if !ok {
		t.Fatal("no recovered Events row 50 in calendar-deleted")
	}
	live := parseVariant(t, "calendar-deleted")
	if _, has := live.recs[50]; has {
		t.Fatal("Events row 50 is live in calendar-deleted: the check proves nothing")
	}
	got := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
		t.Fatalf("recovered row 50: err %v, unmapped %q, %d records", got.Err, got.Unmapped, len(got.Records))
	}
	r := got.Records[0]
	for _, k := range []string{"calendar", "attendees", "reminders", "attendees_total", "reminders_total"} {
		if v, has := r.Payload[k]; has {
			t.Errorf("recovered row 50: payload.%s = %v: a recovered row joins nothing", k, v)
		}
	}
	raw := rawOf(r)
	for _, k := range []string{"attendees", "reminders", "alerts", "extended_properties", "raw_times", "join_undecided", "deleted_via"} {
		if v, has := raw[k]; has {
			t.Errorf("recovered row 50: raw.%s = %v: a recovered row joins nothing", k, v)
		}
	}
	states := []string{"calendar_state", "attendees_state", "reminders_state", "alerts_state", "extended_properties_state", "raw_times_state", "deleted_via_state"}
	if raw["original_id"] != nil {
		states = append(states, "original_state")
	}
	for _, k := range states {
		if raw[k] != notJoined {
			t.Errorf("recovered row 50: raw.%s = %#v, want %q", k, raw[k], notJoined)
		}
	}
	// its own flags are stated as in a live parse: deleted 0 and lastSynced 0 decide nothing, so it is not Deleted by them
	if r.Payload["deleted"] != nil || raw["deleted"] != nil && raw["deleted"] != int64(0) || raw["deleted_state"] != nil || raw["lastSynced_state"] != nil {
		t.Errorf("recovered row 50: deleted %v, raw.deleted %v, deleted_state %v, lastSynced_state %v", r.Payload["deleted"], raw["deleted"], raw["deleted_state"], raw["lastSynced_state"])
	}

	// a recovered row whose own Events.deleted is 1 is Deleted by it (4I-R4a): the row's flag is read, the calendar is not
	rr.Values = slices.Clone(rr.Values)
	cols := parsertest.LoadRecoverExpect(t, "testdata", "calendar-deleted").Columns["Events"]
	i := slices.Index(cols, "deleted")
	if i < 0 || i >= len(rr.Values) || rr.Values[i].Kind != sqlitefile.KindInt {
		t.Fatalf("recovered row 50 has no integer deleted column (index %d of %d)", i, len(rr.Values))
	}
	rr.Values[i].Int = 1
	got = parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || len(got.Records) != 1 {
		t.Fatalf("recovered row 50 with deleted = 1: err %v, unmapped %q, %d records", got.Err, got.Unmapped, len(got.Records))
	}
	d := got.Records[0]
	if !d.Deleted || objOf(d, "deleted")["source"] != "Events.deleted" || rawOf(d)["deleted_via_state"] != notJoined {
		t.Errorf("recovered deleted row: Deleted %v, deleted %v, deleted_via_state %v", d.Deleted, d.Payload["deleted"], rawOf(d)["deleted_via_state"])
	}
}

// calendar-601's only history is row 43, written in a WAL frame that was never committed: the typed pass maps it to a
// record of its own stored values, which joins nothing (B2) and is never one of the live records.
func TestCalendarUncommittedWALRowIsTypedNotLive(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := androidcalendar.Parser{}
	b := parsertest.LoadFixture(t, "testdata", "calendar-601").Bundle(h, remoteProvider)
	if _, live := parseVariant(t, "calendar-601").recs[43]; live {
		t.Fatal("row 43 is live in calendar-601")
	}
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "Events", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 43 })
	if !ok {
		t.Fatal("no recovered Events row 43 in calendar-601")
	}
	got := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
		t.Fatalf("recovered row 43: err %v, unmapped %q, %d records", got.Err, got.Unmapped, len(got.Records))
	}
	r := got.Records[0]
	if r.Payload["title"] != "Only in the WAL, uncommitted" || rawOf(r)["calendar_state"] != "not-joined-recovered" || r.Payload["calendar"] != nil {
		t.Errorf("recovered row 43: title %v, calendar_state %v, calendar %v", r.Payload["title"], rawOf(r)["calendar_state"], r.Payload["calendar"])
	}
}
