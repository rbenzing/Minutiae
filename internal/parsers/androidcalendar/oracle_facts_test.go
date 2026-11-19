package androidcalendar_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// ev is one expected record of an oracle, read through the names the plan's facts use.
type ev struct {
	r       parsertest.ExpRecord
	payload map[string]any
	raw     map[string]any
}

// basis is the time basis the oracle states for the record's start: the oracle writes a utc
// instant with a trailing Z and a local-unknown reading without a zone suffix (the time
// encoding of tools/fixtures/calendar_android.py); "" when there is no start.
func (e ev) basis() string {
	switch {
	case e.r.Time == nil:
		return ""
	case strings.HasSuffix(*e.r.Time, "Z"):
		return "utc"
	case !strings.ContainsAny((*e.r.Time)[10:], "Z+-"):
		return "local-unknown"
	}
	return "unrecognised"
}

func (e ev) time() any    { return strOrNil(e.r.Time) }
func (e ev) timeEnd() any { return strOrNil(e.r.TimeEnd) }
func (e ev) str(k string) string {
	s, _ := e.payload[k].(string)
	return s
}

func (e ev) obj(k string) map[string]any {
	m, _ := e.payload[k].(map[string]any)
	return m
}

func (e ev) list(k string) []any {
	l, _ := e.payload[k].([]any)
	return l
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// num renders a JSON number of the oracle (decoded with UseNumber) as its decimal text.
func num(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprintf("not a number: %v", v)
}

type att struct{ address, name, role, typ, status string }

func attOf(v any) att {
	m, _ := v.(map[string]any)
	s := func(k string) string { x, _ := m[k].(string); return x }
	return att{s("address"), s("name"), s("role"), s("type"), s("status")}
}

// rem is one reminder: before_s as decimal text ("" when absent) and the method.
type rem struct{ beforeS, method string }

func remOf(v any) rem {
	m, _ := v.(map[string]any)
	r := rem{}
	if b, ok := m["before_s"]; ok {
		r.beforeS = num(b)
	}
	r.method, _ = m["method"].(string)
	return r
}

func oracleRecords(t *testing.T, variant string) map[int64]ev {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[int64]ev{}
	for _, r := range fx.Expect.Records {
		if r.Table != "Events" || r.Type != "calendar_event" {
			t.Errorf("%s: record %s rowid %d of type %q", variant, r.Table, r.Rowid, r.Type)
		}
		raw, _ := r.Payload["raw"].(map[string]any)
		out[r.Rowid] = ev{r: r, payload: r.Payload, raw: raw}
	}
	return out
}

// TestAndroidCalendarOracleStatesTheSpecRules: the oracle's literal facts, written from the spec
// (§5 time, §5.3 the provider's end rule, §6 recurrence, exceptions and deletion, §7.3 the
// CalendarContract codes, B5, B7, 4I3-P7, F28) and never read back from the generator, on the
// calendar-601 CORE scenario. A generator that drifts from the rules fails here, not silently in the pins.
func TestAndroidCalendarOracleStatesTheSpecRules(t *testing.T) {
	recs := oracleRecords(t, "calendar-601")
	if len(recs) != 42 {
		t.Fatalf("%d records, want 42 (CORE rows 1-41 and the committed WAL row 42; row 43 is uncommitted)", len(recs))
	}
	if _, has := recs[43]; has {
		t.Error("row 43 lies only in an uncommitted WAL frame and is not live")
	}
	rec := func(id int64) ev {
		e, ok := recs[id]
		if !ok {
			t.Fatalf("no record for row %d", id)
		}
		return e
	}
	want := func(id string, ok bool) {
		t.Helper()
		if !ok {
			t.Errorf("fact %s does not hold", id)
		}
	}
	// spec 5.1 and 5.3: a timed row is a utc instant; dtend is the end when no duration is stored (4I3-P7)
	want("row 1", rec(1).time() == "2023-11-14T22:13:20Z" && rec(1).basis() == "utc" && rec(1).timeEnd() == "2023-11-14T23:13:20Z" && rec(1).raw["time_end_basis"] == "dtend")
	// spec 5.3: duration first (the provider's end rule); recurrence as stored, never expanded (6.1)
	want("row 2", rec(2).timeEnd() == "2023-11-14T23:13:20Z" && rec(2).raw["time_end_basis"] == "dtstart+duration" && rec(2).obj("recurrence")["rrule"] == "FREQ=WEEKLY;BYDAY=TU")
	// P3600S, and P2W as fixed 604,800-second weeks
	want("rows 3 and 5", rec(3).timeEnd() == "2023-11-14T23:13:20Z" && rec(5).timeEnd() == "2023-11-28T22:13:20Z")
	// spec 5.2: an all-day value at a UTC midnight is a date; end_date is exclusive; basis local-unknown
	want("row 4", rec(4).str("start_date") == "2023-11-15" && rec(4).str("end_date") == "2023-11-16" && rec(4).basis() == "local-unknown")
	want("rows 6 and 7", rec(6).str("start_date") == "2023-11-15" && rec(6).str("end_date") == "2023-11-16" && rec(7).str("end_date") == "2023-11-18")
	// spec 5.2 and B5: an all-day start off midnight gives no date and no time, and then no end
	want("row 8", rec(8).time() == nil && rec(8).str("start_date") == "" && rec(8).raw["start_state"] == "all-day-not-midnight" && rec(8).raw["end_state"] == "no-start")
	// spec 6.2 and 4.3: an exception links its series; original_start is a utc instant (originalAllDay 0)
	want("row 9", rec(9).obj("exception_of")["event_id"] == "2" && rec(9).r.Times["original_start"] == "2023-11-21T22:13:20Z")
	// spec 6.2: a cancelled occurrence is a status, not a deletion
	want("row 10", rec(10).str("status") == "cancelled" && !rec(10).r.Deleted)
	// spec 6.2: a dangling original_id is stated; F28: rows 10 and 11 still have original_start
	want("row 11", rec(11).raw["original_state"] == "no-events-row" && rec(10).r.Times["original_start"] != "" && rec(11).r.Times["original_start"] != "")
	// spec 6.3 and B7: the soft delete, a deleted calendar, the last-synced copy; dirty alone is live
	want("row 12", rec(12).r.Deleted && rec(12).obj("deleted")["source"] == "Events.deleted")
	want("row 13", rec(13).r.Deleted && rec(13).obj("deleted")["source"] == "Calendars.deleted" && rec(13).raw["deleted_via"] == "calendar")
	want("row 14", !rec(14).r.Deleted && num(rec(14).raw["dirty"]) == "1")
	want("row 15", rec(15).r.Deleted && rec(15).obj("deleted")["relation"] == "last-synced-copy" && rec(15).str("sync_id") == "s14")
	// spec 7.3: Events.organizer has NUMERIC affinity; a number is not the text that was written
	want("row 16", rec(16).payload["organizer"] == nil && rec(16).raw["organizer_state"] == "not-text")
	// spec 2 (CalendarContract): relationship 2 organizer, type 1 required, status 1 accepted; codes 9, 7, 8 unknown
	a := rec(17).list("attendees")
	want("row 17", len(a) == 6 && attOf(a[0]) == att{"a@example.test", "A", "organizer", "required", "accepted"} && attOf(a[5]).role == "unknown")
	// the attendee row with neither address nor name is raw only (len 6 of 7 rows, asserted above)
	// spec 7.3: minutes -1 is the default (no before_s), 0 and 30 are 0 and 1800 s, -5 is stated
	var rs []rem
	for _, r := range rec(18).list("reminders") {
		rs = append(rs, remOf(r))
	}
	rraw, _ := rec(18).raw["reminders"].([]any)
	minutes3 := ""
	if len(rraw) == 4 {
		m3, _ := rraw[3].(map[string]any)
		minutes3 = num(m3["minutes"])
	}
	want("row 18", reflect.DeepEqual(rs, []rem{{"", "default"}, {"0", "alert"}, {"1800", "email"}, {"", "alarm"}}) &&
		rec(18).raw["reminders_state"] == "negative-minutes" && minutes3 == "-5")
	// spec 5.3: duration and dtend both stored and disagreeing: duration wins, the conflict is stated
	want("row 24", rec(24).timeEnd() == "2023-11-14T23:13:20Z" && rec(24).raw["end_conflict"] == true)
	// spec 5.3: an unparsed or out-of-range duration gives no end and no fallback to dtend
	want("rows 25 and 26", rec(25).timeEnd() == nil && rec(25).raw["duration_state"] == "unparsed" && rec(26).raw["duration_state"] == "out-of-range")
	// spec 5.3: an end before the start is not stated
	want("row 27", rec(27).timeEnd() == nil && rec(27).raw["end_state"] == "end-before-start")
	// spec 5.4 and Q5: a sentinel and an out-of-range start; an all-day 1955 date has no start_date
	want("rows 29, 30 and 41", rec(29).raw["dtstart_status"] == "sentinel" && rec(30).raw["dtstart_status"] == "invalid" && rec(41).str("start_date") == "")
	// spec 7.3: allDay other than 0 or 1 is unknown timing, with no time
	want("row 31", rec(31).str("timing") == "unknown" && rec(31).time() == nil)
	// spec 6.3 and B7: an undecidable flag is stated, never deleted-by-guess and never live-by-default
	want("rows 32 and 33", !rec(32).r.Deleted && rec(32).raw["deleted_state"] == "undecided" && !rec(33).r.Deleted && rec(33).raw["deleted_via_state"] == "undecided")
}

// TestAndroidCalendarOracleStatesTheSchemaVersions: a version-403 database has no uid2445 column,
// so no record has a uid; in the upgraded database rows 1-3 were written before the ALTERs (short
// records), so row 1 has no uid there although the CORE row 1 states one (spec 7.4).
func TestAndroidCalendarOracleStatesTheSchemaVersions(t *testing.T) {
	up := oracleRecords(t, "calendar-403-upgraded-601")
	if len(up) != 6 {
		t.Errorf("calendar-403-upgraded-601: %d records, want 6", len(up))
	}
	for id := int64(1); id <= 3; id++ {
		if e, ok := up[id]; !ok || e.payload["uid"] != nil {
			t.Errorf("calendar-403-upgraded-601 row %d: present %v, uid %v (a short record has no uid)", id, ok, e.payload["uid"])
		}
	}
	for id, e := range oracleRecords(t, "calendar-403") {
		if e.payload["uid"] != nil {
			t.Errorf("calendar-403 row %d has a uid: the 403 schema has no uid2445 column", id)
		}
	}
	if recs := oracleRecords(t, "calendar-601"); recs[1].payload["uid"] != "uid-1@example.test" {
		t.Errorf("calendar-601 row 1 uid %v: the CORE row states uid-1@example.test", recs[1].payload["uid"])
	}
}
