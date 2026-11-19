package androidcalendar_test

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const tM = int64(1700006400000) // 2023-11-15T00:00:00Z

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The plan's state vocabularies (android.calendar), the keys Tasks 4 and 5 own: one Go-built row per value, each
// asserted with a hard-coded expected value (a nil want means the key is absent). Task 6 extends this function (F20).
func TestAndroidCalendarStateVocabularyIsPinned(t *testing.T) {
	longDur := "P" + strings.Repeat("1", 70) + "S"
	longSync := strings.Repeat("s", 1500)
	longOrg := strings.Repeat("o", 1100) + "@example.test"
	longAddr := strings.Repeat("a", 2<<10) + "@example.test"
	cases := []struct {
		id   int64
		kw   map[string]any
		want map[string]any // raw key -> value
		pay  map[string]any // payload key -> value
	}{
		// timing (spec 7.3)
		{1, map[string]any{"allDay": int64(2)}, map[string]any{"timing_state": "undecided", "allDay": int64(2)}, map[string]any{"timing": "unknown"}},
		// zones (spec 9, 4I3-P8)
		{2, map[string]any{"eventTimezone": "Z/" + strings.Repeat("z", 300)}, map[string]any{"eventTimezone_state": "too-long", "eventTimezone_len": int64(302), "eventTimezone_sha256": sha("Z/" + strings.Repeat("z", 300))}, map[string]any{"start_timezone": nil}},
		{3, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + 86400000}, map[string]any{"all_day_zone_state": "not-utc", "time_end_basis": "dtend"}, map[string]any{"start_date": "2023-11-15", "end_date": "2023-11-16", "start_timezone": "Europe/Berlin"}},
		{4, map[string]any{"eventEndTimezone": "America/New_York"}, map[string]any{"eventEndTimezone_state": nil}, map[string]any{"end_timezone": "America/New_York"}},
		// duration and end (spec 5.3, 4I3-P7, coordinator rulings)
		{5, map[string]any{"dtend": nil, "duration": "P1DT"}, map[string]any{"duration_state": "unparsed", "end_state": nil, "time_end_basis": nil}, map[string]any{"duration": "P1DT"}},
		{6, map[string]any{"dtend": nil, "duration": "P9999999999S"}, map[string]any{"duration_state": "out-of-range"}, nil},
		{7, map[string]any{"duration": longDur}, map[string]any{"duration_state": "unparsed", "duration_len": int64(72), "duration_sha256": sha(longDur)}, map[string]any{"duration": nil}},
		{8, map[string]any{"dtstart": nil, "duration": longDur}, map[string]any{"duration_state": "unparsed", "end_state": "no-start", "duration_len": int64(72)}, nil},
		{9, map[string]any{"allDay": int64(2), "duration": longDur}, map[string]any{"duration_state": "unparsed", "timing_state": "undecided"}, nil},
		{10, map[string]any{"duration": "PT1H", "dtend": tD + 2*tH}, map[string]any{"end_conflict": true, "time_end_basis": "dtstart+duration"}, map[string]any{"duration": "PT1H"}},
		{11, map[string]any{"dtend": nil}, map[string]any{"end_state": "none-stored"}, nil},
		{12, map[string]any{"dtend": tD - tH}, map[string]any{"end_state": "end-before-start"}, nil},
		{13, map[string]any{"allDay": int64(1), "dtstart": tM + tH, "eventTimezone": "UTC"}, map[string]any{"start_state": "all-day-not-midnight", "end_state": "no-start", "all_day_zone_state": nil}, nil},
		{14, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + tH, "eventTimezone": "UTC"}, map[string]any{"end_state": "all-day-not-midnight"}, map[string]any{"start_date": "2023-11-15", "end_date": nil}},
		// original (spec 5.2, 6.2)
		{15, map[string]any{"original_id": int64(99), "originalInstanceTime": tD, "originalAllDay": int64(5)}, map[string]any{"original_start_state": "undecided", "original_state": "no-events-row"}, map[string]any{"exception_of": map[string]any{"event_id": "99"}}},
		{16, map[string]any{"original_id": int64(1), "originalInstanceTime": tM + tH, "originalAllDay": int64(1)}, map[string]any{"original_start_state": "all-day-not-midnight", "original_state": nil}, nil},
		// organizer (spec 7.3, 9)
		{17, map[string]any{"organizer": int64(15550000001)}, map[string]any{"organizer_state": "not-text", "organizer": int64(15550000001)}, map[string]any{"organizer": nil}},
		{18, map[string]any{"organizer": longOrg}, map[string]any{"organizer_state": "too-long", "organizer_len": int64(len(longOrg)), "organizer_sha256": sha(longOrg)}, map[string]any{"organizer": nil}},
		{19, map[string]any{"organizer": "ann@example.test"}, map[string]any{"organizer_state": nil}, map[string]any{"organizer": map[string]any{"address": "ann@example.test"}}},
		// calendar (spec 7.3)
		{20, map[string]any{"calendar_id": int64(77)}, map[string]any{"calendar_state": "no-calendars-row", "calendar": nil, "deleted_via_state": nil}, map[string]any{"calendar": nil}},
		{
			21, nil,
			map[string]any{"calendar": map[string]any{"deleted": int64(0), "visible": int64(1), "sync_events": int64(1)}},
			map[string]any{"calendar": map[string]any{"id": "1", "title": "Personal", "owner": "ann@example.test", "account": map[string]any{"type": "com.example.sync", "name": "ann@example.test"}}},
		},
		// deletion and sync (spec 6.3, B7)
		{22, map[string]any{"deleted": nil}, map[string]any{"deleted_state": "undecided", "unknown_columns": nil}, map[string]any{"deleted": nil}},
		{23, map[string]any{"deleted": int64(7)}, map[string]any{"deleted_state": "undecided", "unknown_columns": map[string]any{"Events.deleted": "out-of-set"}}, nil},
		{24, map[string]any{"lastSynced": nil}, map[string]any{"lastSynced_state": "undecided"}, nil},
		{25, map[string]any{"lastSynced": int64(1)}, map[string]any{"last_synced": int64(1)}, map[string]any{"deleted": map[string]any{"source": "Events.lastSynced", "relation": "last-synced-copy"}}},
		{26, map[string]any{"dirty": int64(3)}, map[string]any{"dirty": int64(3), "dirty_state": "undecided", "unknown_columns": map[string]any{"Events.dirty": "out-of-set"}}, map[string]any{"deleted": nil}},
		{27, map[string]any{"dirty": "x"}, map[string]any{"dirty": nil, "dirty_state": "undecided", "unknown_columns": map[string]any{"Events.dirty": "type-mismatch"}}, nil},
		{28, map[string]any{"dirty": int64(1), "mutators": "com.example.app"}, map[string]any{"dirty": int64(1), "mutators": "com.example.app", "dirty_state": nil}, map[string]any{"deleted": nil}},
		{29, map[string]any{"calendar_id": int64(3)}, map[string]any{"deleted_via_state": "undecided", "unknown_columns": map[string]any{"Calendars.deleted": "out-of-set"}}, map[string]any{"deleted": nil}},
		// codes (spec 2): an unmapped code is unknown with the code in raw; a wrongly typed one is also listed
		{
			30,
			map[string]any{"eventStatus": int64(3), "availability": int64(3), "accessLevel": int64(4), "selfAttendeeStatus": int64(5)},
			map[string]any{"eventStatus": int64(3), "availability": int64(3), "accessLevel": int64(4), "selfAttendeeStatus": int64(5)},
			map[string]any{"status": "unknown", "availability": "unknown", "privacy": "unknown", "self_status": "unknown"},
		},
		{31, map[string]any{"eventStatus": int64(2), "selfAttendeeStatus": int64(1)}, map[string]any{"eventStatus": int64(2), "selfAttendeeStatus": nil}, map[string]any{"status": "cancelled", "self_status": "accepted"}},
		{32, map[string]any{"eventStatus": "x"}, map[string]any{"eventStatus": "x", "unknown_columns": map[string]any{"Events.eventStatus": "type-mismatch"}}, map[string]any{"status": "unknown"}},
		// a stored NaN read through the mapping's row (coordinator check a): undecided, never NULL
		{33, map[string]any{"dtstart": math.NaN()}, map[string]any{"dtstart": nil, "unknown_columns": map[string]any{"Events.dtstart": "type-mismatch"}}, map[string]any{"start_date": nil}},
		// raw only (spec 7.3)
		{
			34,
			map[string]any{"sync_data1": longSync, "sync_data2": "short", "hasAlarm": int64(1), "lastDate": int64(0)},
			map[string]any{"sync_data1": strings.Repeat("s", 1024), "sync_data1_len": int64(1500), "sync_data1_sha256": sha(longSync), "sync_data2": "short", "hasAlarm": int64(1), "lastDate": int64(0), "lastDate_status": "sentinel"},
			nil,
		},
		// text (C4): a wrongly typed free text is listed, never absent silently
		{35, map[string]any{"title": []byte{1, 2}}, map[string]any{"unknown_columns": map[string]any{"Events.title": "type-mismatch"}}, map[string]any{"title": nil}},
		// a wrongly typed duration is listed and kept as stored even when no end is computed (fix round 1, I1)
		{36, map[string]any{"allDay": int64(2), "duration": int64(5)}, map[string]any{"duration": int64(5), "unknown_columns": map[string]any{"Events.duration": "type-mismatch"}}, map[string]any{"duration": nil}},
		{37, map[string]any{"dtstart": nil, "duration": []byte{1, 2, 3}}, map[string]any{"duration": map[string]any{"blob_b64": "AQID"}, "unknown_columns": map[string]any{"Events.duration": "type-mismatch"}}, nil},
		// an intact row states no decoder damage (fix round 1, M2); a row with no child rows states no child key (C3)
		{
			38, nil,
			map[string]any{
				"row_flags": nil, "attendees": nil, "attendees_unnamed": nil, "reminders": nil, "reminders_state": nil, "alerts": nil,
				"extended_properties": nil, "raw_times": nil, "join_undecided": nil, "attendees_state": nil, "alerts_state": nil,
				"extended_properties_state": nil, "raw_times_state": nil,
			},
			map[string]any{"attendees": nil, "reminders": nil, "attendees_total": nil, "reminders_total": nil},
		},
		// children (spec 7.3, 9, F27): an address over 1 KiB is never kept nor cut; with no name the attendee is counted
		// unnamed and its entry, in raw.attendees_unnamed_entries (raw.attendees stays parallel to attendees, fix round
		// 1, I2), keeps the length and SHA-256 as sibling scalars
		{
			39, nil,
			map[string]any{
				"attendees_unnamed": int64(1), "attendees": nil,
				"attendees_unnamed_entries": []any{map[string]any{"attendeeRelationship": int64(1), "address_state": "too-long", "address_len": int64(len(longAddr)), "address_sha256": sha(longAddr)}},
			},
			map[string]any{"attendees": nil},
		},
		{
			40, nil,
			map[string]any{"attendees_unnamed": nil, "attendees": []any{map[string]any{"address_state": "too-long", "address_len": int64(len(longAddr)), "address_sha256": sha(longAddr)}}},
			map[string]any{"attendees": []any{map[string]any{"name": "Named"}}},
		},
		{41, nil, map[string]any{"reminders_state": "negative-minutes", "reminders": []any{map[string]any{"minutes": int64(-5), "method": int64(2)}}}, map[string]any{"reminders": []any{map[string]any{"method": "email"}}}},
		// the caps (spec 9, C6): 1,001 of each list keep 1,000 with the total and the flag
		{
			42, nil,
			map[string]any{"alerts_total": int64(1001), "alerts_truncated": true, "extended_properties_total": int64(1001), "extended_properties_truncated": true},
			map[string]any{"attendees_total": int64(1001), "attendees_truncated": true, "reminders_total": int64(1001), "reminders_truncated": true},
		},
		// a wrongly typed attendee address is listed and kept as stored with address_state "undecided", never absent
		// silently (spec 9, C1, fix round 1, I3): beside a name in the parallel raw.attendees entry; without one in
		// raw.attendees_unnamed_entries, not counted as an attendee with neither address nor name
		{
			43, nil,
			map[string]any{
				"unknown_columns": map[string]any{"Attendees.attendeeEmail": "type-mismatch"},
				"attendees":       []any{map[string]any{"attendeeEmail": int64(5), "address_state": "undecided"}},
			},
			map[string]any{"attendees": []any{map[string]any{"name": "Num"}}},
		},
		{
			44, nil,
			map[string]any{
				"attendees_unnamed": nil, "attendees": nil,
				"attendees_unnamed_entries": []any{map[string]any{"attendeeEmail_blob_b64": "AQI=", "address_state": "undecided"}},
			},
			map[string]any{"attendees": nil},
		},
		// a BLOB in a child cell is flattened to a sibling scalar <col>_blob_b64 (B8: raw stays within the container
		// depth; fix round 1, C1), and the event is still emitted
		{
			45, nil,
			map[string]any{
				"attendees":           []any{map[string]any{"attendeeType_blob_b64": "AQI="}},
				"alerts":              []any{map[string]any{"begin_blob_b64": "AQI=", "state": int64(1), "state_label": "fired"}},
				"extended_properties": []any{map[string]any{"name": "n", "value_blob_b64": "AQI="}},
				"raw_times":           map[string]any{"dtstart2445_blob_b64": "AQI="},
				"unknown_columns":     map[string]any{"Attendees.attendeeType": "type-mismatch"},
			},
			map[string]any{"attendees": []any{map[string]any{"name": "Blob", "type": "unknown"}}},
		},
		// more than one EventsRawTimes row: the first is kept and the number is stated (fix round 1, M5)
		{46, nil, map[string]any{"raw_times": map[string]any{"dtstart2445": "A"}, "raw_times_total": int64(2)}, nil},
	}
	var rows []row
	for _, c := range cases {
		rows = append(rows, row{c.id, c.kw})
	}
	kids := map[string][]row{
		"Attendees": {
			{1, map[string]any{"event_id": int64(39), "attendeeEmail": longAddr, "attendeeRelationship": int64(1)}},
			{2, map[string]any{"event_id": int64(40), "attendeeEmail": longAddr, "attendeeName": "Named"}},
			{3, map[string]any{"event_id": int64(43), "attendeeEmail": int64(5), "attendeeName": "Num"}},
			{4, map[string]any{"event_id": int64(44), "attendeeEmail": []byte{1, 2}}},
			{5, map[string]any{"event_id": int64(45), "attendeeName": "Blob", "attendeeType": []byte{1, 2}}},
		},
		"Reminders":          {{1, map[string]any{"event_id": int64(41), "minutes": int64(-5), "method": int64(2)}}},
		"CalendarAlerts":     {{2000, map[string]any{"event_id": int64(45), "begin": []byte{1, 2}, "state": int64(1)}}},
		"ExtendedProperties": {{2000, map[string]any{"event_id": int64(45), "name": "n", "value": []byte{1, 2}}}},
		"EventsRawTimes": {
			{1, map[string]any{"event_id": int64(45), "dtstart2445": []byte{1, 2}}},
			{2, map[string]any{"event_id": int64(46), "dtstart2445": "A"}},
			{3, map[string]any{"event_id": int64(46), "dtstart2445": "B"}},
		},
	}
	for i := int64(1); i <= 1001; i++ {
		kids["Attendees"] = append(kids["Attendees"], row{100 + i, map[string]any{"event_id": int64(42), "attendeeName": "N"}})
		kids["Reminders"] = append(kids["Reminders"], row{100 + i, map[string]any{"event_id": int64(42), "minutes": int64(1), "method": int64(1)}})
		kids["CalendarAlerts"] = append(kids["CalendarAlerts"], row{i, map[string]any{"event_id": int64(42), "state": int64(0)}})
		kids["ExtendedProperties"] = append(kids["ExtendedProperties"], row{i, map[string]any{"event_id": int64(42), "name": "n"}})
	}
	cals := []row{{1, nil}, {3, map[string]any{"calendar_displayName": "Odd", "deleted": int64(7)}}}
	// row 90: a record whose body is longer than its header says (the decoder's length_mismatch): _id NULL, _sync_id
	// NULL, dirty 0, mutators NULL, lastSynced 0, calendar_id 1, then one byte too many (fix round 1, M2)
	damaged := func(tb *sqlitetest.Table) { tb.InsertRaw(90, []uint64{0, 0, 1, 0, 1, 1}, []byte{0, 0, 1, 'Z'}) }
	built, _ := buildDB(t, dbSpec{events: rows, calendars: cals, children: kids, more: damaged})
	p := parseBuilt(t, built, false)
	if f, _ := rawOf(p.rec(t, 90))["row_flags"].([]any); !slices.Contains(f, any("length_mismatch")) {
		t.Errorf("row 90: raw.row_flags %v, want length_mismatch (raw %v)", f, rawOf(p.rec(t, 90)))
	}
	for _, c := range cases {
		r := p.rec(t, c.id)
		raw := rawOf(r)
		for k, w := range c.want {
			check(t, c.id, "raw."+k, raw, k, w)
		}
		for k, w := range c.pay {
			check(t, c.id, k, r.Payload, k, w)
		}
	}
	if v, has := rawOf(p.rec(t, 11))["dtend"]; !has || v != nil {
		t.Errorf("row 11: raw.dtend = %v (present %v), want a stated null (spec 5.4)", v, has)
	}
	if r := p.rec(t, 33); r.Time != nil {
		t.Errorf("row 33: a NaN dtstart gave the time %v", r.Time)
	}
	if w := warningsWith(p.res, "calendar-flag-undecided"); len(w) != 4 {
		t.Errorf("flag warnings %v, want one each for Events.deleted, Events.lastSynced, Events.dirty, Calendars.deleted", w)
	}

	// the typed pass (Mappings, no Parse context, 4I3-P4, B2): a row joins nothing and each join-derived state says so;
	// the row's own flags are stated as in a live parse (fix round 1, M3)
	// (event 1 has an attendee and a reminder: a recovered row joins neither, B2)
	typedDB, _ := buildDB(t, dbSpec{events: []row{
		{1, map[string]any{"original_id": int64(1), "originalInstanceTime": tD, "originalAllDay": int64(0)}},
		{2, map[string]any{"calendar_id": int64(3), "deleted": int64(1)}},
	}, calendars: cals, children: map[string][]row{
		"Attendees": {{1, map[string]any{"event_id": int64(1), "attendeeName": "A"}}},
		"Reminders": {{1, map[string]any{"event_id": int64(1), "minutes": int64(5)}}},
	}})
	typed := mapTyped(t, typedDB)
	for id, w := range map[int64]map[string]any{
		1: {
			"calendar_state": "not-joined-recovered", "deleted_via_state": "not-joined-recovered", "original_state": "not-joined-recovered", "calendar": nil, "join_undecided": nil,
			"attendees_state": "not-joined-recovered", "reminders_state": "not-joined-recovered", "alerts_state": "not-joined-recovered",
			"extended_properties_state": "not-joined-recovered", "raw_times_state": "not-joined-recovered", "attendees": nil, "reminders": nil,
		},
		2: {"calendar_state": "not-joined-recovered", "deleted_via_state": "not-joined-recovered", "original_state": nil, "deleted_via": nil},
	} {
		r, ok := typed[id]
		if !ok {
			t.Fatalf("typed pass: no record for row %d", id)
		}
		for k, v := range w {
			check(t, id, "typed raw."+k, rawOf(r), k, v)
		}
		check(t, id, "typed calendar", r.Payload, "calendar", nil)
	}
	if r := typed[2]; !r.Deleted || objOf(r, "deleted")["source"] != "Events.deleted" || rawOf(r)["deletion_also"] != nil {
		t.Errorf("typed row 2: Deleted %v, deleted %v, deletion_also %v (its own flag decides; the calendar is not joined)", r.Deleted, r.Payload["deleted"], rawOf(r)["deletion_also"])
	}

	// an absent Calendars table under a -wal (Probe deferred): every calendar join is undecided, never "no row", and
	// so is the calendar's deletion state; the absence is a note, no schema warning (4I2 PF13)
	q := parseBuilt(t, buildCalendarDB(t, []row{{1, nil}, {2, nil}}, nil), true)
	for id := int64(1); id <= 2; id++ {
		raw := rawOf(q.rec(t, id))
		check(t, id, "raw.join_undecided", raw, "join_undecided", []any{"Calendars"})
		check(t, id, "raw.deleted_via_state", raw, "deleted_via_state", "undecided")
		check(t, id, "raw.calendar_state", raw, "calendar_state", nil)
	}
	if q.res.Notes["join_undecided.Calendars"] != "table absent" {
		t.Errorf("notes %v, want join_undecided.Calendars = table absent", q.res.Notes)
	}
	want := map[string]string{
		"calendar-flag-undecided: Calendars.deleted: 2 records": "sqlite:table=Calendars",
		"calendar-join-undecided: Calendars: 2 records":         "sqlite:table=Calendars",
	}
	got := append(warningsWith(q.res, "calendar-flag-undecided"), warningsWith(q.res, "calendar-join-undecided")...)
	if len(got) != len(want) {
		t.Errorf("warnings %v, want %v", q.res.Warnings, want)
	}
	for _, w := range got {
		if want[w.Reason] != w.Locator {
			t.Errorf("warning %+v is not one of %v", w, want)
		}
	}
}

func check(t *testing.T, id int64, label string, m map[string]any, k string, w any) {
	t.Helper()
	g, has := m[k]
	switch {
	case w == nil && has:
		t.Errorf("row %d: %s = %#v, want absent", id, label, g)
	case w != nil && !reflect.DeepEqual(g, w):
		t.Errorf("row %d: %s = %#v, want %#v", id, label, g, w)
	}
}
