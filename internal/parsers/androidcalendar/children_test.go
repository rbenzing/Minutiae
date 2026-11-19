package androidcalendar_test

import (
	"reflect"
	"strings"
	"testing"
)

// Spec 7.3 and 2 (CalendarContract): row 17's attendees in row-id order, each code labelled by the contract and an
// undefined code (9, 7, 8) "unknown" with the stored codes in raw.attendees (aligned); the attendee row with neither
// address nor name is not an attendee and is counted in raw.attendees_unnamed.
func TestAndroidCalendarAttendeesFollowCalendarContract(t *testing.T) {
	r := parseVariant(t, "calendar-601").rec(t, 17)
	want := []any{
		map[string]any{"address": "a@example.test", "name": "A", "role": "organizer", "type": "required", "status": "accepted"},
		map[string]any{"address": "b@example.test", "role": "attendee", "type": "optional", "status": "declined"},
		map[string]any{"name": "C", "role": "performer", "type": "resource", "status": "invited"},
		map[string]any{"address": "d@example.test", "name": "D", "role": "speaker", "type": "none", "status": "tentative"},
		map[string]any{"address": "e@example.test", "role": "none", "type": "none", "status": "none"},
		map[string]any{"address": "f@example.test", "role": "unknown", "type": "unknown", "status": "unknown"},
	}
	if got := r.Payload["attendees"]; !reflect.DeepEqual(got, want) {
		t.Errorf("attendees\n got %#v\nwant %#v", got, want)
	}
	raw := rawOf(r)
	list, _ := raw["attendees"].([]any)
	if len(list) != 6 {
		t.Fatalf("raw.attendees %v, want 6 entries aligned with attendees", raw["attendees"])
	}
	if w := map[string]any{"attendeeRelationship": int64(9), "attendeeType": int64(7), "attendeeStatus": int64(8)}; !reflect.DeepEqual(list[5], w) {
		t.Errorf("raw.attendees[5] = %#v, want %#v", list[5], w)
	}
	if raw["attendees_unnamed"] != int64(1) {
		t.Errorf("raw.attendees_unnamed = %#v, want 1", raw["attendees_unnamed"])
	}
	for _, k := range []string{"attendees_total", "attendees_truncated"} {
		if _, has := r.Payload[k]; has {
			t.Errorf("%s stated for 6 attendees", k)
		}
	}
}

// Spec 7.3: before_s = minutes x 60 for minutes >= 0; -1 (the provider's default) gives no before_s; another negative
// keeps the reminder with its method only and states raw.reminders_state; an undefined method is "unknown" with the
// stored code in raw.reminders.
func TestAndroidCalendarRemindersFollowTheProvider(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	r := p.rec(t, 18)
	want := []any{
		map[string]any{"method": "default"},
		map[string]any{"before_s": int64(0), "method": "alert"},
		map[string]any{"before_s": int64(1800), "method": "email"},
		map[string]any{"method": "alarm"},
	}
	if got := r.Payload["reminders"]; !reflect.DeepEqual(got, want) {
		t.Errorf("row 18 reminders\n got %#v\nwant %#v", got, want)
	}
	raw := rawOf(r)
	if raw["reminders_state"] != "negative-minutes" {
		t.Errorf("row 18 raw.reminders_state = %#v, want negative-minutes", raw["reminders_state"])
	}
	wantRaw := []any{
		map[string]any{"minutes": int64(-1), "method": int64(0)},
		map[string]any{"minutes": int64(0), "method": int64(1)},
		map[string]any{"minutes": int64(30), "method": int64(2)},
		map[string]any{"minutes": int64(-5), "method": int64(4)},
	}
	if !reflect.DeepEqual(raw["reminders"], wantRaw) {
		t.Errorf("row 18 raw.reminders = %#v, want %#v", raw["reminders"], wantRaw)
	}
	one := p.rec(t, 1)
	if got := one.Payload["reminders"]; !reflect.DeepEqual(got, []any{map[string]any{"before_s": int64(600), "method": "unknown"}}) {
		t.Errorf("row 1 reminders = %#v", got)
	}
	if got := rawOf(one)["reminders"]; !reflect.DeepEqual(got, []any{map[string]any{"minutes": int64(10), "method": int64(9)}}) {
		t.Errorf("row 1 raw.reminders = %#v, want the stored method 9", got)
	}
	if _, has := rawOf(one)["reminders_state"]; has {
		t.Error("row 1 states reminders_state")
	}
	if _, has := p.rec(t, 2).Payload["reminders"]; has {
		t.Error("row 2 has no Reminders row and states reminders (a decided empty answer states nothing, C3)")
	}
}

// Spec 7.3 and Q8: alerts, extended properties and raw times are raw only. A fired alert keeps its state and label
// (a fired alert is not a record); an undefined state has no label; a property value over 4 KiB is cut and names the
// cut in value_flags; the RFC 2445 strings are as stored.
func TestAndroidCalendarRawOnlyChildren(t *testing.T) {
	r := parseVariant(t, "calendar-601").rec(t, 19)
	raw := rawOf(r)
	alerts, _ := raw["alerts"].([]any)
	if len(alerts) != 2 {
		t.Fatalf("raw.alerts %v, want 2", raw["alerts"])
	}
	a0, _ := alerts[0].(map[string]any)
	a1, _ := alerts[1].(map[string]any)
	want0 := map[string]any{
		"begin": tD, "end": tD + tH, "alarmTime": tD - 600000, "creationTime": tD - 700000, "receivedTime": tD - 650000,
		"notifyTime": tD - 600000, "state": int64(1), "state_label": "fired",
	}
	if !reflect.DeepEqual(a0, want0) {
		t.Errorf("raw.alerts[0] = %#v, want %#v", a0, want0)
	}
	if _, has := a1["state_label"]; has || a1["state"] != int64(7) {
		t.Errorf("raw.alerts[1] = %#v, want state 7 and no label", a1)
	}
	props, _ := raw["extended_properties"].([]any)
	if len(props) != 2 {
		t.Fatalf("raw.extended_properties %v, want 2", raw["extended_properties"])
	}
	if p0 := props[0]; !reflect.DeepEqual(p0, map[string]any{"name": "vnd.example/x", "value": "1"}) {
		t.Errorf("raw.extended_properties[0] = %#v", p0)
	}
	p1, _ := props[1].(map[string]any)
	if v, _ := p1["value"].(string); len(v) != 4<<10 || p1["value_flags"] != "truncated" || p1["name"] != "vnd.example/big" {
		t.Errorf("raw.extended_properties[1]: value %d bytes, value_flags %v, name %v; want 4096, truncated", len(v), p1["value_flags"], p1["name"])
	}
	if w := map[string]any{"dtstart2445": "20231114T221320Z", "dtend2445": "20231114T231320Z"}; !reflect.DeepEqual(raw["raw_times"], w) {
		t.Errorf("raw.raw_times = %#v, want %#v", raw["raw_times"], w)
	}
	for _, k := range []string{"alerts", "extended_properties", "raw_times"} {
		if _, has := r.Payload[k]; has {
			t.Errorf("payload.%s: a raw-only child is in the payload", k)
		}
	}
}

// Spec 7.3 and 9: a child join through an index that lost rows (a damaged page of Attendees) is undecided, never "no
// attendees": raw.join_undecided names the table, no attendees are stated, and one counted warning follows Run.
func TestAndroidCalendarUndecidedChildJoinIsStated(t *testing.T) {
	var att []row
	for i := int64(1); i <= 120; i++ {
		att = append(att, row{i, map[string]any{"event_id": int64(1), "attendeeEmail": "a" + strings.Repeat("x", 60) + "@example.test", "attendeeName": strings.Repeat("N", 60)}})
	}
	db, tables := buildDB(t, dbSpec{events: []row{{1, nil}}, calendars: []row{{1, nil}}, children: map[string][]row{"Attendees": att}})
	leaves := tables["Attendees"].Leaves()
	if len(leaves) < 3 {
		t.Fatalf("Attendees has %d leaves, want at least 3", len(leaves))
	}
	page := db[int(leaves[1]-1)*4096:][:4096]
	page[3], page[4] = 0, 0 // the leaf's cell count: its cells are lost to the scan that builds the event_id index
	p := parseBuilt(t, db, false)
	r := p.rec(t, 1)
	if got := rawOf(r)["join_undecided"]; !reflect.DeepEqual(got, []any{"Attendees"}) {
		t.Errorf("raw.join_undecided = %#v, want [Attendees]", got)
	}
	if _, has := r.Payload["attendees"]; has {
		t.Errorf("attendees %v stated from an undecided join", r.Payload["attendees"])
	}
	for _, k := range []string{"attendees", "attendees_unnamed"} {
		if _, has := rawOf(r)[k]; has {
			t.Errorf("raw.%s stated from an undecided join", k)
		}
	}
	w := warningsWith(p.res, "calendar-join-undecided")
	if len(w) != 1 || w[0].Reason != "calendar-join-undecided: Attendees: 1 records" || w[0].Locator != "sqlite:table=Attendees" {
		t.Errorf("join warnings %+v, want one calendar-join-undecided: Attendees: 1 records", w)
	}
}

// Spec 7.3 (C3): a child table the database lacks makes every join through it undecided, never "no children".
func TestAndroidCalendarAbsentChildTableIsUndecided(t *testing.T) {
	db, _ := buildDB(t, dbSpec{events: []row{{1, nil}}, calendars: []row{{1, nil}}, omit: []string{"Reminders"}})
	p := parseBuilt(t, db, false)
	if got := rawOf(p.rec(t, 1))["join_undecided"]; !reflect.DeepEqual(got, []any{"Reminders"}) {
		t.Errorf("raw.join_undecided = %#v, want [Reminders]", got)
	}
	if p.res.Notes["join_undecided.Reminders"] != "table absent" {
		t.Errorf("notes %v, want join_undecided.Reminders = table absent", p.res.Notes)
	}
}
