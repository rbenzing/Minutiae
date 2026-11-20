package ioscalendar_test

import (
	"math"
	"strings"
	"testing"
)

const reasonNotEvent = "CalendarItem row is not an event (entity_type 3)"

// Q11, 4I3-P10, spec 8.2 and 9: a task row (entity_type 3) is no record but one counted warning at its row; every
// other row is a record, its kind undecided unless entity_type is 2; the notes give the exact counts of the live
// rows (NULL is no value), or say that the column is absent. Past the cap of 1,000 row warnings one warning names the
// number dropped (spec 8.2, 9) and the note warnings_dropped states it.
func TestIOSCalendarNonEventRowsAreCountedWarnings(t *testing.T) {
	res := parseVariant(t, "calendar-ios")
	if res.Err != nil || res.Refused != 0 {
		t.Fatalf("calendar-ios: err %v, %d refused", res.Err, res.Refused)
	}
	var notEvent []string
	for _, w := range res.Warnings {
		if w.Reason == reasonNotEvent {
			notEvent = append(notEvent, w.Locator)
		}
	}
	if len(notEvent) != 1 || !strings.HasPrefix(notEvent[0], "sqlite:table=CalendarItem;rowid=11") {
		t.Errorf("calendar-ios: non-event warnings at %v, want one at rowid 11", notEvent)
	}
	recs := byRowid(t, res)
	if _, has := recs[11]; has || len(recs) != 26 {
		t.Errorf("calendar-ios: %d records (row 11 present: %v), want 26 without the task row", len(recs), has)
	}
	if res.Notes["non_event_rows"] != "1" || res.Notes["entity_type_values"] != "2:24,3:1,7:1" || res.Notes["entity_type"] != "" {
		t.Errorf("calendar-ios: notes non_event_rows %q, entity_type_values %q, entity_type %q", res.Notes["non_event_rows"], res.Notes["entity_type_values"], res.Notes["entity_type"])
	}
	if raw := rawOf(recs[12]); raw["item_kind_state"] != "undecided" || raw["entity_type"] != nil {
		t.Errorf("row 12 (entity_type NULL): raw %v", raw)
	} else if _, has := raw["entity_type"]; !has {
		t.Error("row 12 (entity_type NULL): raw.entity_type is not stated as null")
	}
	if raw := rawOf(recs[13]); raw["item_kind_state"] != "undecided" || raw["entity_type"] != int64(7) {
		t.Errorf("row 13 (entity_type 7): raw %v", raw)
	}
	if raw := rawOf(recs[1]); raw["item_kind_state"] != nil || raw["entity_type"] != nil {
		t.Errorf("row 1 (an event): raw %v states a kind", raw)
	}

	// the column absent: every row is a record and the note says so
	res = parseVariant(t, "calendar-ios-noentitytype")
	if res.Err != nil || len(res.Records) != 3 || res.Notes["entity_type"] != "absent" || res.Notes["entity_type_values"] != "" || res.Notes["non_event_rows"] != "" {
		t.Errorf("calendar-ios-noentitytype: err %v, %d records, notes %v", res.Err, len(res.Records), res.Notes)
	}

	// a cell that is not an INTEGER is no kind: a record with the kind undecided, never a warning, never a NULL
	res = parseAt(t, map[string][]byte{"db": goStore(
		item{1, map[string]any{"entity_type": "3"}},
		item{2, map[string]any{"entity_type": float64(3)}},
		item{3, map[string]any{"entity_type": math.NaN()}},
		item{4, map[string]any{"entity_type": []byte{3}}},
	)}, true)
	recs = byRowid(t, res)
	if res.Err != nil || len(recs) != 4 || len(res.Warnings) != 0 {
		t.Fatalf("wrongly typed kinds: err %v, %d records, warnings %v", res.Err, len(recs), res.Warnings)
	}
	for id, r := range recs {
		raw := rawOf(r)
		if _, has := raw["entity_type"]; has || raw["item_kind_state"] != "undecided" || unknownOf(r)["CalendarItem.entity_type"] != "type-mismatch" {
			t.Errorf("row %d: raw %v", id, raw)
		}
	}
	if _, has := res.Notes["entity_type_values"]; has {
		t.Errorf("wrongly typed kinds: entity_type_values %q, want none (no INTEGER value)", res.Notes["entity_type_values"])
	}

	// past the cap: 1,000 row warnings, one naming the 500 dropped, and the exact total in the notes
	rows := make([]item, 0, 1500)
	for i := int64(1); i <= 1500; i++ {
		rows = append(rows, item{i, map[string]any{"entity_type": int64(3)}})
	}
	res = parseAt(t, map[string][]byte{"db": goStore(rows...)}, true)
	n, summary := 0, 0
	for _, w := range res.Warnings {
		switch {
		case w.Reason == reasonNotEvent:
			n++
		case strings.Contains(w.Reason, "500") && strings.Contains(w.Reason, "dropped"):
			summary++
		}
	}
	if res.Err != nil || len(res.Records) != 0 || n != 1000 || summary != 1 || len(res.Warnings) != 1001 {
		t.Errorf("1,500 tasks: err %v, %d records, %d row warnings, %d summaries, %d warnings", res.Err, len(res.Records), n, summary, len(res.Warnings))
	}
	if res.Notes["warnings_dropped"] != "500" || res.Notes["non_event_rows"] != "1500" || res.Notes["entity_type_values"] != "3:1500" {
		t.Errorf("1,500 tasks: notes %v", res.Notes)
	}

	// at most 16 distinct values are named, 2 and 3 always among them; the rest is "other"
	rows = rows[:0]
	for i := int64(1); i <= 20; i++ {
		rows = append(rows, item{i, map[string]any{"entity_type": 10 + i}})
	}
	rows = append(rows, item{21, map[string]any{"entity_type": int64(3)}}, item{22, nil})
	res = parseAt(t, map[string][]byte{"db": goStore(rows...)}, true)
	want := "2:1,3:1,11:1,12:1,13:1,14:1,15:1,16:1,17:1,18:1,19:1,20:1,21:1,22:1,23:1,24:1,other:6"
	if res.Notes["entity_type_values"] != want || res.Notes["non_event_rows"] != "1" {
		t.Errorf("22 kinds: entity_type_values %q, non_event_rows %q; want %q and 1", res.Notes["entity_type_values"], res.Notes["non_event_rows"], want)
	}
}
