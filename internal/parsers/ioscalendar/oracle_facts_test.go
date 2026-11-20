package ioscalendar_test

import (
	"encoding/json"
	"fmt"
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
// encoding of tools/fixtures/calendar_ios.py); "" when there is no start.
func (e ev) basis() string {
	switch {
	case e.r.Time == nil:
		return ""
	case len(*e.r.Time) < len("2006-01-02T15:04:05"):
		return "unrecognised"
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

func (e ev) rawList(k string) []any {
	l, _ := e.raw[k].([]any)
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

// field reads key of the i-th element of a list of objects ("" or nil when there is none).
func field(l []any, i int, key string) any {
	if i >= len(l) {
		return nil
	}
	m, _ := l[i].(map[string]any)
	return m[key]
}

func oracleRecords(t *testing.T, variant string) (map[int64]ev, parsertest.Expect) {
	t.Helper()
	fx := parsertest.LoadFixture(t, "testdata", variant)
	out := map[int64]ev{}
	for _, r := range fx.Expect.Records {
		if r.Table != "CalendarItem" || r.Type != "calendar_event" {
			t.Errorf("%s: record %s rowid %d of type %q", variant, r.Table, r.Rowid, r.Type)
		}
		raw, _ := r.Payload["raw"].(map[string]any)
		out[r.Rowid] = ev{r: r, payload: r.Payload, raw: raw}
	}
	return out, fx.Expect
}

// TestIOSCalendarOracleStatesTheSpecRules: the oracle's literal facts, written from the spec
// (§5 time, §5.2 and B1 the all-day midnight rule, B5 no start, B6 the timing classes, §6.2
// exceptions, §6.4 the deletion state, §8.2 the mapping, Q1 the codes kept raw, Q11 the non-event
// rows, F29 the organizer participant) and never read back from the generator, on the calendar-ios
// CORE-iOS scenario. A generator that drifts from the rules fails here, not silently in the pins.
func TestIOSCalendarOracleStatesTheSpecRules(t *testing.T) {
	recs, exp := oracleRecords(t, "calendar-ios")
	// rows 1 to 27 are live (27 in a committed WAL frame), 11 is a task (no record), 28 lies only in an uncommitted frame
	if len(recs) != 26 {
		t.Fatalf("%d records, want 26 (CORE rows 1-27 without the task row 11)", len(recs))
	}
	for _, id := range []int64{11, 28} {
		if _, has := recs[id]; has {
			t.Errorf("row %d has a record: a task row and a row of an uncommitted WAL frame are no events", id)
		}
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
	warning := func(rowid int64) string {
		for _, w := range exp.Warnings {
			if w.Table == "CalendarItem" && w.Rowid == rowid {
				return w.Contains
			}
		}
		return ""
	}
	// spec 5.1, 8.2: a zoned all_day 0 row is timed, a utc instant; the zone is stated, never applied; created is a Cocoa instant
	want("row 1", rec(1).time() == "2023-11-14T22:13:20Z" && rec(1).basis() == "utc" && rec(1).str("start_timezone") == "Europe/Berlin" && rec(1).r.Times["created"] == "2023-11-13T22:13:20Z")
	// spec 5.1, 8.2: start_tz _float is floating: the wall-clock reading, local-unknown, no zone; the marker is raw only
	want("row 2", rec(2).str("timing") == "floating" && rec(2).basis() == "local-unknown" && rec(2).time() == "2023-11-14T22:13:20" && rec(2).str("start_timezone") == "" && rec(2).raw["start_tz"] == "_float")
	// spec 5.2: an all-day value at a UTC midnight is a date; end_date exclusive
	want("row 3", rec(3).str("start_date") == "2023-11-15" && rec(3).str("end_date") == "2023-11-16")
	// spec 5.2 and B5: an all-day start off midnight has no date and no time, and then no end
	want("row 4", rec(4).time() == nil && rec(4).raw["start_state"] == "all-day-not-midnight" && rec(4).raw["end_state"] == "no-start")
	// B1: a 23:59:59 all-day end has no end_date and no TimeEnd, stated
	want("row 5", rec(5).str("start_date") == "2023-11-15" && rec(5).str("end_date") == "" && rec(5).timeEnd() == nil && rec(5).raw["end_state"] == "all-day-not-midnight")
	// spec 8.2: a NULL start_tz with all_day 0 is unknown timing
	want("row 6", rec(6).str("timing") == "unknown" && rec(6).raw["timing_state"] == "start-tz-absent")
	// spec 6.2: orig_item_id links the series; orig_date is raw only, its basis unestablished, so no original_start
	_, origStart := rec(7).r.Times["original_start"]
	want("row 7", rec(7).obj("exception_of")["event_id"] == "8" && rec(7).raw["orig_date_basis"] == "unestablished" && !origStart)
	// spec 6.1, 8.2: stored_rules counts the Recurrence rows, the exception dates are raw
	want("row 8", num(rec(8).obj("recurrence")["stored_rules"]) == "2" && len(rec(8).rawList("exception_dates")) == 2)
	// spec 8.2: the organizer from Participant 4 and Identity 1
	want("row 1 organizer", rec(1).obj("organizer")["address"] == "mailto:ann@example.test" && rec(1).obj("organizer")["name"] == "Ann")
	// F29: the organizer participant is also an attendee (Match(Participant.item_id)), its role code kept raw (Q1)
	a1 := rec(1).list("attendees")
	want("row 1 attendees", len(a1) == 1 && field(a1, 0, "address") == "mailto:ann@example.test" && field(a1, 0, "role") == "unknown")
	// Q1: a participant's codes are never labelled; the stored code is in raw.attendees
	a9 := rec(9).list("attendees")
	want("row 9", field(a9, 0, "address") == "c@example.test" && field(a9, 0, "role") == "unknown" && num(field(rec(9).rawList("attendees"), 0, "role")) == "1")
	// spec 8.2: alarms are raw only, the sign convention is unknown, so no reminders
	want("row 10", rec(10).payload["reminders"] == nil && len(rec(10).rawList("alarms")) == 2)
	// Q11: a task row (entity_type 3) is a counted warning, not a record
	want("row 11", warning(11) == "CalendarItem row is not an event (entity_type 3)")
	// spec 8.2: entity_type NULL or another value is a record with an undecided item kind
	want("rows 12 and 13", rec(12).raw["item_kind_state"] == "undecided" && num(rec(13).raw["entity_type"]) == "7")
	// Review Focus 5: two zones of one class keep the end; both names as stored
	want("row 14", rec(14).timeEnd() == "2023-11-15T06:13:20Z" && rec(14).str("end_timezone") == "Europe/London")
	// B6: start and end of different timing classes give no end
	want("row 15", rec(15).timeEnd() == nil && rec(15).raw["end_state"] == "timing-mismatch")
	// spec 9: a coordinate out of range is not stated, the value stays raw
	want("row 16", rec(16).obj("location")["latitude"] == nil && rec(16).raw["latitude_state"] == "out-of-range")
	// spec 8.2: a key naming no row is stated, never "no calendar"
	want("row 17", rec(17).raw["calendar_state"] == "no-Calendar-row" && rec(17).raw["location_state"] == "no-Location-row")
	// Q1: the status code is not mapped
	want("row 1 status", rec(1).str("status") == "unknown" && num(rec(1).raw["status"]) == "1")
	// spec 6.4: no iOS deletion marker is established, so every record says its deletion state was not evaluated
	for id, r := range recs {
		want(fmt.Sprintf("row %d deletion", id), r.raw["deletion_state"] == "not-evaluated" && !r.r.Deleted)
	}
	// 4I3-P10: the non-event count and the kinds of the live rows (NULL is no value)
	want("notes", exp.Notes["non_event_rows"] == "1" && exp.Notes["entity_type_values"] == "2:24,3:1,7:1")
}

// TestIOSCalendarOracleStatesTheVariants: without the entity_type column every row is a record and the
// note says the column is absent (Q11); the older Event layout and the extraction hold what their
// variants state (spec 8.1).
func TestIOSCalendarOracleStatesTheVariants(t *testing.T) {
	recs, exp := oracleRecords(t, "calendar-ios-noentitytype")
	if len(recs) != 3 || exp.Notes["entity_type"] != "absent" {
		t.Errorf("calendar-ios-noentitytype: %d records, note entity_type %q; want 3 and absent", len(recs), exp.Notes["entity_type"])
	}
	if recs, _ := oracleRecords(t, "calendar-ios-extract"); len(recs) != 3 {
		t.Errorf("calendar-ios-extract: %d records, want 3", len(recs))
	}
	if recs, _ := oracleRecords(t, "calendar-ios-oldlayout"); len(recs) != 0 {
		t.Errorf("calendar-ios-oldlayout: %d records, want none (no CalendarItem table)", len(recs))
	}
	if recs, _ := oracleRecords(t, "calendar-ios-deleted"); len(recs) != 3 {
		t.Errorf("calendar-ios-deleted: %d live records, want 3 (rows 40-44 are deleted)", len(recs))
	}
}
