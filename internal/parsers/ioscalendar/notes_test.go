package ioscalendar_test

import (
	"testing"
)

// Spec 8.2 "Notes", 6.1, 6.4, Q6 and the plan's "Notes and warnings": the store is stated before the mapping runs:
// the base tables present, those the mapping never reads, the occurrence cache's row count and the change-tracking
// tables with their row counts; a job stays under the host's MaxNotes of 64.
func TestIOSCalendarNotesStateTheStore(t *testing.T) {
	res := parseVariant(t, "calendar-ios")
	want := map[string]string{
		"tables":                "Alarm,Calendar,CalendarItem,CalendarItemChanges,ExceptionDate,Identity,Location,OccurrenceCache,Participant,Recurrence,Store",
		"unmapped_tables":       "CalendarItemChanges,OccurrenceCache",
		"change_tables":         "CalendarItemChanges:2",
		"occurrence_cache_rows": "3",
	}
	for k, v := range want {
		if res.Notes[k] != v {
			t.Errorf("note %s = %q, want %q", k, res.Notes[k], v)
		}
	}
	if len(res.Notes) > 64 {
		t.Errorf("%d notes, over MaxNotes 64", len(res.Notes))
	}

	// another change table, an empty occurrence cache and a table the mapping never reads
	s := newBuilt()
	s.table("AttendeeChanges", "record INTEGER", true)
	s.table("Sidecar", "v TEXT", true)
	s.add("CalendarItemChanges", 1, map[string]any{"record": int64(1)})
	s.event(1, nil)
	res = parseBuilt(t, s.bytes(), false)
	for k, v := range map[string]string{
		"change_tables":         "AttendeeChanges:0,CalendarItemChanges:1",
		"occurrence_cache_rows": "0",
		"unmapped_tables":       "AttendeeChanges,CalendarItemChanges,OccurrenceCache,Sidecar",
	} {
		if res.Notes[k] != v {
			t.Errorf("Go-built: note %s = %q, want %q", k, res.Notes[k], v)
		}
	}

	// no occurrence cache and no change table: neither note is stated (an absent table has no count)
	s = newBuilt("OccurrenceCache", "CalendarItemChanges")
	s.event(1, nil)
	res = parseBuilt(t, s.bytes(), false)
	for _, k := range []string{"occurrence_cache_rows", "change_tables", "unmapped_tables"} {
		if v, has := res.Notes[k]; has {
			t.Errorf("no such table: note %s = %q, want none", k, v)
		}
	}
}
