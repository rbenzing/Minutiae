package androidcalendar_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// Spec 7.3 and Q8: the Instances cache of expanded occurrences is not a record source: no record names it, its row
// count is a note and the table is named among the unmapped tables.
func TestAndroidCalendarInstancesAreNotRecords(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	for _, r := range p.res.Records {
		if strings.Contains(r.Locator, "Instances") {
			t.Errorf("record %s names Instances", r.Locator)
		}
	}
	if got := p.res.Notes["rows.Instances"]; got != "1" {
		t.Errorf("note rows.Instances = %q, want 1", got)
	}
	if got := p.res.Notes["unmapped_tables"]; !strings.Contains(","+got+",", ",Instances,") {
		t.Errorf("note unmapped_tables = %q, want it to name Instances", got)
	}
}

// Spec 7.3 and the plan's "Notes and warnings": the notes state the store (its user version, its tables, the
// provider's metadata and cache values as stored, never applied) and the count notes after Run (one owner each, F08),
// all within the host's MaxNotes; no count of the events of a deleted calendar is stated (T2 ruling M6).
func TestAndroidCalendarNotesStateTheStore(t *testing.T) {
	p := parseVariant(t, "calendar-601")
	n := p.res.Notes
	want := map[string]string{
		"database_version": "601", "meta.localTimezone": "Europe/Berlin", "meta.minInstance": "1698796800000",
		"meta.maxInstance": "1701388800000", "cache.timezoneType": "auto", "cache.timezoneInstances": "Europe/Berlin",
		"cache.timezoneInstancesPrevious": "Europe/Berlin", "soft_deleted": "1", "lastsynced_copies": "2",
		"unmapped_tables": "Instances,Colors",
		"tables": "Attendees,CalendarAlerts,CalendarCache,CalendarMetaData,Calendars,Colors,Events,EventsRawTimes," +
			"ExtendedProperties,Instances,Reminders,sqlite_sequence",
	}
	for k, v := range want {
		if n[k] != v {
			t.Errorf("note %s = %q, want %q", k, n[k], v)
		}
	}
	if _, has := n["deleted_calendar_events"]; has {
		t.Error("a deleted_calendar_events note is stated (spec 7.3 lists no such note)")
	}
	if limit := parse.DefaultLimits().MaxNotes; len(n) >= limit {
		t.Errorf("%d notes: at the cap MaxNotes %d a note may have been dropped", len(n), limit)
	}
	for v, version := range map[string]string{"calendar-600": "600", "calendar-502": "502", "calendar-403": "403", "calendar-403-upgraded-601": "601"} {
		q := parseVariant(t, v)
		if got := q.res.Notes["database_version"]; got != version {
			t.Errorf("%s: note database_version = %q, want %q", v, got, version)
		}
		for _, k := range []string{"soft_deleted", "lastsynced_copies", "meta.localTimezone"} {
			if _, has := q.res.Notes[k]; has {
				t.Errorf("%s: note %s stated, the variant has none", v, k)
			}
		}
	}
}
