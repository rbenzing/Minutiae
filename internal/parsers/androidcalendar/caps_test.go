package androidcalendar_test

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// Spec 4.1 and 9 (C6): every list and text cap is counted, never silent. 1,500 attendees, 1,200 reminders, 1,100
// alerts and 1,100 extended properties on one event keep 1,000 each with the total and the truncated flag; a 5 KiB
// title is cut at 4 KiB and named in truncated_fields; a 4 MiB description is cut with body_truncated and its total.
func TestCalendarCapsAreCountedNeverSilent(t *testing.T) {
	children := map[string][]row{}
	add := func(table string, n int, kw func(i int64) map[string]any) {
		for i := int64(1); i <= int64(n); i++ {
			m := kw(i)
			m["event_id"] = int64(1)
			children[table] = append(children[table], row{i, m})
		}
	}
	add("Attendees", 1500, func(int64) map[string]any {
		return map[string]any{"attendeeEmail": "a@example.test", "attendeeRelationship": int64(1)}
	})
	// 1,001 attendees of event 2 with an over-long address and no name: each is counted unnamed and its entry kept in
	// raw.attendees_unnamed_entries, capped at 1,000 with the total and the flag (coordinator ruling g)
	longAddr := strings.Repeat("a", 1<<10) + "@example.test"
	for i := int64(1); i <= 1001; i++ {
		children["Attendees"] = append(children["Attendees"], row{10000 + i, map[string]any{"event_id": int64(2), "attendeeEmail": longAddr}})
	}
	add("Reminders", 1200, func(i int64) map[string]any { return map[string]any{"minutes": i, "method": int64(1)} })
	add("CalendarAlerts", 1100, func(i int64) map[string]any {
		return map[string]any{"begin": tD, "end": tD + tH, "alarmTime": tD - i, "state": int64(0)}
	})
	add("ExtendedProperties", 1100, func(int64) map[string]any { return map[string]any{"name": "n", "value": "v"} })
	// final review I2: event 3 holds raw-only cells over 16 KiB (as stored, and of another class) and an attendee with
	// such cells; its calendar 2 holds account fields over 1 KiB
	over := strings.Repeat("u", 20<<10)
	children["Attendees"] = append(children["Attendees"], row{20001, map[string]any{
		"event_id": int64(3), "attendeeEmail": "b@example.test", "attendeeIdentity": over, "attendeeRelationship": over,
	}})
	longID := strings.Repeat("c", 2<<10)
	title := strings.Repeat("t", 5<<10)
	desc := strings.Repeat("d", 4<<20)
	db, _ := buildDB(t, dbSpec{
		events: []row{
			{1, map[string]any{"title": title, "description": desc}},
			{2, nil},
			{3, map[string]any{"calendar_id": int64(2), "customAppUri": over, "mutators": over, "eventStatus": over}},
		},
		calendars: []row{{1, nil}, {2, map[string]any{"account_type": longID, "account_name": longID, "ownerAccount": longID, "name": over}}},
		children:  children,
	})
	p := parseBuilt(t, db, false)
	r := p.rec(t, 1)
	raw := rawOf(r)
	lists := []struct {
		label string
		m     map[string]any
		key   string
		total int64
	}{
		{"payload", r.Payload, "attendees", 1500},
		{"payload", r.Payload, "reminders", 1200},
		{"raw", raw, "alerts", 1100},
		{"raw", raw, "extended_properties", 1100},
	}
	for _, l := range lists {
		got, _ := l.m[l.key].([]any)
		if len(got) != 1000 || l.m[l.key+"_total"] != l.total || l.m[l.key+"_truncated"] != true {
			t.Errorf("%s.%s: %d kept, total %#v, truncated %#v; want 1000, %d, true", l.label, l.key, len(got), l.m[l.key+"_total"], l.m[l.key+"_truncated"], l.total)
		}
	}
	for _, k := range []string{"attendees", "reminders"} {
		if got, _ := raw[k].([]any); len(got) != 1000 {
			t.Errorf("raw.%s has %d entries, want 1000 (aligned with the kept list)", k, len(got))
		}
	}
	if got, _ := r.Payload["title"].(string); len(got) != 4<<10 {
		t.Errorf("title of %d bytes, want 4096", len(got))
	}
	if !reflect.DeepEqual(r.Payload["truncated_fields"], []any{"title"}) && !reflect.DeepEqual(r.Payload["truncated_fields"], []string{"title"}) {
		t.Errorf("truncated_fields = %#v, want [title]", r.Payload["truncated_fields"])
	}
	if r.Payload["body_truncated"] != true || r.Payload["body_total_bytes"] != int64(4<<20) || len(r.Body) != 256<<10 {
		t.Errorf("body: truncated %#v, total %#v, %d bytes kept; want true, %d, 262144", r.Payload["body_truncated"], r.Payload["body_total_bytes"], len(r.Body), 4<<20)
	}
	raw2 := rawOf(p.rec(t, 2))
	if got, _ := raw2["attendees_unnamed_entries"].([]any); len(got) != 1000 || raw2["attendees_unnamed_entries_total"] != int64(1001) ||
		raw2["attendees_unnamed_entries_truncated"] != true || raw2["attendees_unnamed"] != int64(1001) {
		t.Errorf("row 2: raw.attendees_unnamed_entries %d kept, total %#v, truncated %#v, attendees_unnamed %#v; want 1000, 1001, true, 1001",
			len(got), raw2["attendees_unnamed_entries_total"], raw2["attendees_unnamed_entries_truncated"], raw2["attendees_unnamed"])
	}
	// final review I2 (spec 9, as ios.calendar's maxStored): a raw-only TEXT over 16 KiB is never kept nor cut: its
	// <key>_state "too-long", <key>_len and <key>_sha256 stand in its place, at the top of raw, in raw.calendar and in a
	// child entry; the account fields are identifiers (1 KiB, never cut), stated as ios.calendar states Store.name
	h := sha256.Sum256([]byte(over))
	overSum := hex.EncodeToString(h[:])
	h = sha256.Sum256([]byte(longID))
	idSum := hex.EncodeToString(h[:])
	r3 := p.rec(t, 3)
	raw3 := rawOf(r3)
	calRaw, _ := raw3["calendar"].(map[string]any)
	ra, _ := raw3["attendees"].([]any)
	var entry map[string]any
	if len(ra) == 1 {
		entry, _ = ra[0].(map[string]any)
	}
	for _, c := range []struct {
		label string
		m     map[string]any
		key   string
		n     int
		sum   string
	}{
		{"raw", raw3, "customAppUri", len(over), overSum},
		{"raw", raw3, "mutators", len(over), overSum},
		{"raw", raw3, "eventStatus", len(over), overSum},
		{"raw.calendar", calRaw, "name", len(over), overSum},
		{"raw.attendees[0]", entry, "attendeeIdentity", len(over), overSum},
		{"raw.attendees[0]", entry, "attendeeRelationship", len(over), overSum},
		{"raw", raw3, "calendar_account", len(longID), idSum},
		{"raw", raw3, "calendar_account_type", len(longID), idSum},
		{"raw", raw3, "calendar_owner", len(longID), idSum},
	} {
		if _, kept := c.m[c.key]; kept || c.m[c.key+"_state"] != "too-long" || c.m[c.key+"_len"] != int64(c.n) || c.m[c.key+"_sha256"] != c.sum {
			t.Errorf("row 3: %s.%s kept %v, state %#v, len %#v, sha256 %#v; want absent, too-long, %d and the hash",
				c.label, c.key, kept, c.m[c.key+"_state"], c.m[c.key+"_len"], c.m[c.key+"_sha256"], c.n)
		}
	}
	if cal, _ := r3.Payload["calendar"].(map[string]any); cal["account"] != nil || cal["owner"] != nil || cal["id"] != "2" {
		_, acct := cal["account"]
		_, owner := cal["owner"]
		t.Errorf("row 3: calendar id %v, account stated %v, owner stated %v; want id 2 and no account or owner", cal["id"], acct, owner)
	}
}
