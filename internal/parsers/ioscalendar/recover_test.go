package ioscalendar_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// The claims are exactly the claimable (total) tables: CalendarItem, every row of which is a record or a counted
// non-event warning (spec 10). The join tables, the occurrence cache and the change-tracking tables are never claimed.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := ioscalendar.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := ioscalendar.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"CalendarItem"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [CalendarItem]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
	if err := parse.ValidateMeta(p.Meta()); err != nil {
		t.Errorf("Meta with its claims: %v", err)
	}
}

// Spec 10 and 4I2 PF13: the claimed CalendarItem table maps its recovered rows, checked against the oracle that the
// generator computed with an independent page walker from the committed calendar-ios-deleted file (a lower bound:
// every recovered event row is a typed record, and row 44, a task, is reported unmapped with the non-event reason,
// never dropped silently). A variant with no oracle file and no history is checked against the empty set, a real
// claim (a typed record there would be invented). calendar-ios holds history but has no oracle: its WAL holds row 28
// in a frame that was never committed, checked by TestCalendarUncommittedWALRowIsTypedNotLive.
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range recordedVariants() {
		if v == "calendar-ios" {
			continue
		}
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			want := parsertest.RecoverExpect{Variant: v, Claimed: ioscalendar.ClaimableTables()}
			if v == "calendar-ios-deleted" {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
				if len(want.Records) == 0 || len(want.Unmapped) == 0 {
					t.Fatal("the calendar-ios-deleted recover oracle lists no record or no unmapped row: the check proves nothing")
				}
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, ioscalendar.Parser{}, b, want)
		})
	}
}

// liveRow reads one row of the live view of a fixture by its rowid, and the integer of col in it (F17: the rows that
// name a recovered item survive).
func liveRow(t *testing.T, variant, table string, rowid int64, col string) (found bool, v int64, isInt bool) {
	t.Helper()
	db := fixtureFiles(t, variant)["db"]
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	in := bundleAt(h, map[string][]byte{"db": db}, true).Input(h, p, false)
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	mc := sqlitedb.NewContext(t.Context(), d, in, noteSink{})
	defer mc.Close()
	row, found, err := mc.Get(table, rowid)
	if err != nil || !found {
		return false, 0, false
	}
	if col == "" {
		return true, 0, false
	}
	v, isInt = row.Int(rowrun.Resolve(mc, table, col).At(col))
	return true, v, isInt
}

// B2 (4I3-P4): a recovered row joins no table, even when rows that name it survive in the live view. Recovered row 40
// of calendar-ios-deleted names Location 1 and Calendar 1, and Participant row 10 names it by item_id: all three are in
// the live view (F17). Its record carries no location, calendar, organizer, attendees or recurrence, and every
// join-derived state its row can have is "not-joined-recovered", never absent (its organizer_id is NULL, so it has no
// organizer join to state, as in a live parse); its deletion state is "not-evaluated" (spec 6.4). Recovered row 44, a
// task (entity_type 3), is the counted non-event warning, never a record.
func TestCalendarRecoveredRowsJoinNothing(t *testing.T) {
	const notJoined = "not-joined-recovered" // pinned literally (F20)
	const variant = "calendar-ios-deleted"
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", variant), variant)
	if _, live := byRowid(t, parseVariant(t, variant))[40]; live {
		t.Fatal("CalendarItem row 40 is live in calendar-ios-deleted: the check proves nothing")
	}
	for _, s := range []struct {
		table string
		rowid int64
		col   string
		want  int64
	}{{"Location", 1, "", 0}, {"Calendar", 1, "", 0}, {"Participant", 10, "item_id", 40}} {
		found, v, isInt := liveRow(t, variant, s.table, s.rowid, s.col)
		if !found || s.col != "" && (!isInt || v != s.want) {
			t.Fatalf("live %s row %d: found %v, %s %v (int %v); want it to survive and name row 40 (F17)", s.table, s.rowid, found, s.col, v, isInt)
		}
	}
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "CalendarItem", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 40 })
	if !ok {
		t.Fatal("no recovered CalendarItem row 40 in calendar-ios-deleted")
	}
	got := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
		t.Fatalf("recovered row 40: err %v, unmapped %q, %d records", got.Err, got.Unmapped, len(got.Records))
	}
	r := got.Records[0]
	if r.Payload["title"] != "Deleted 40" {
		t.Errorf("recovered row 40: title %v, want \"Deleted 40\"", r.Payload["title"])
	}
	for _, k := range []string{"location", "calendar", "organizer", "attendees", "attendees_total", "recurrence", "exception_of"} {
		if v, has := r.Payload[k]; has {
			t.Errorf("recovered row 40: payload.%s = %v: a recovered row joins nothing", k, v)
		}
	}
	raw := rawOf(r)
	for _, k := range []string{"attendees", "attendees_unnamed", "recurrence", "exception_dates", "alarms", "join_undecided", "store_type", "organizer_state"} {
		if v, has := raw[k]; has {
			t.Errorf("recovered row 40: raw.%s = %v: a recovered row joins nothing", k, v)
		}
	}
	for _, k := range []string{"location_state", "calendar_state", "participants_state", "recurrence_state", "exception_dates_state", "alarms_state"} {
		if raw[k] != notJoined {
			t.Errorf("recovered row 40: raw.%s = %#v, want %q", k, raw[k], notJoined)
		}
	}
	if raw["deletion_state"] != "not-evaluated" || r.Deleted {
		t.Errorf("recovered row 40: deletion_state %v, Deleted %v; want not-evaluated and not Deleted", raw["deletion_state"], r.Deleted)
	}

	// row 44 is a task: the counted non-event warning, never a record and never dropped
	rr, ok = parsertest.FirstClaimedRow(t, h, p, b, "CalendarItem", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 44 })
	if !ok {
		t.Fatal("no recovered CalendarItem row 44 in calendar-ios-deleted")
	}
	got = parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || len(got.Records) != 0 || !strings.Contains(got.Unmapped, "CalendarItem row is not an event (entity_type 3)") {
		t.Errorf("recovered row 44: err %v, %d records, unmapped %q; want the non-event warning", got.Err, len(got.Records), got.Unmapped)
	}
}

// calendar-ios's only history is row 28, written in a WAL frame that was never committed: the typed pass maps it to a
// record of its own stored values, which joins nothing (B2) and is never one of the live records.
func TestCalendarUncommittedWALRowIsTypedNotLive(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "calendar-ios"), "calendar-ios")
	if _, live := byRowid(t, parseVariant(t, "calendar-ios"))[28]; live {
		t.Fatal("row 28 is live in calendar-ios")
	}
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "CalendarItem", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 28 })
	if !ok {
		t.Fatal("no recovered CalendarItem row 28 in calendar-ios")
	}
	got := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if got.Err != nil || got.Unmapped != "" || len(got.Records) != 1 {
		t.Fatalf("recovered row 28: err %v, unmapped %q, %d records", got.Err, got.Unmapped, len(got.Records))
	}
	r := got.Records[0]
	if r.Payload["title"] != "Only in the WAL, uncommitted" || rawOf(r)["calendar_state"] != "not-joined-recovered" || r.Payload["calendar"] != nil {
		t.Errorf("recovered row 28: title %v, calendar_state %v, calendar %v", r.Payload["title"], rawOf(r)["calendar_state"], r.Payload["calendar"])
	}
}
