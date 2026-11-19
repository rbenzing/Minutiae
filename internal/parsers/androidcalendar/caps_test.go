package androidcalendar_test

import (
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
	add("Reminders", 1200, func(i int64) map[string]any { return map[string]any{"minutes": i, "method": int64(1)} })
	add("CalendarAlerts", 1100, func(i int64) map[string]any {
		return map[string]any{"begin": tD, "end": tD + tH, "alarmTime": tD - i, "state": int64(0)}
	})
	add("ExtendedProperties", 1100, func(int64) map[string]any { return map[string]any{"name": "n", "value": "v"} })
	title := strings.Repeat("t", 5<<10)
	desc := strings.Repeat("d", 4<<20)
	db, _ := buildDB(t, dbSpec{
		events: []row{{1, map[string]any{"title": title, "description": desc}}}, calendars: []row{{1, nil}}, children: children,
	})
	r := parseBuilt(t, db, false).rec(t, 1)
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
}
