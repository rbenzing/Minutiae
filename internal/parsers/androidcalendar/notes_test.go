package androidcalendar_test

import (
	"slices"
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
		"unmapped_tables": "Instances,Colors", "rows.CalendarMetaData": "1", "unmapped_columns.CalendarAlerts": "minutes",
		"tables": "Attendees,CalendarAlerts,CalendarCache,CalendarMetaData,Calendars,Colors,Events,EventsRawTimes," +
			"ExtendedProperties,Instances,Reminders,sqlite_sequence",
	}
	for k, v := range want {
		if n[k] != v {
			t.Errorf("note %s = %q, want %q", k, n[k], v)
		}
	}
	for _, k := range []string{"unmapped_columns.Attendees", "unmapped_columns.Reminders", "tables_state"} {
		if _, has := n[k]; has {
			t.Errorf("note %s = %q stated (every column is read; the schema was read whole)", k, n[k])
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

// Fix round 1, I4: a schema read in part (a damaged schema cell) lists the tables it saw and says the list is
// incomplete; it never claims that the tables it did not see are absent.
func TestAndroidCalendarPartialSchemaIsStated(t *testing.T) {
	db, _ := buildDB(t, dbSpec{events: []row{{1, nil}}, calendars: []row{{1, nil}}})
	if db[100] != 0x0d {
		t.Fatalf("page 1 is not a table leaf (%#x): the fixture needs a one-page schema", db[100])
	}
	cells := int(db[103])<<8 | int(db[104])
	last := 108 + 2*(cells-1)
	db[last], db[last+1] = 0xff, 0xff // the last schema cell (EventsRawTimes) points outside the page
	res := parseAt(t, remoteProvider, map[string][]byte{"db": db})
	if res.Err != nil {
		t.Fatalf("Parse: %v", res.Err)
	}
	if len(res.Records) != 1 {
		t.Errorf("%d records, want 1", len(res.Records))
	}
	if got := res.Notes["tables_state"]; got != "incomplete" {
		t.Errorf("note tables_state = %q, want incomplete (notes %v)", got, res.Notes)
	}
	if strings.Contains(res.Notes["tables"], "EventsRawTimes") || !strings.Contains(res.Notes["tables"], "Events") {
		t.Errorf("note tables = %q: the tables seen, without the lost one", res.Notes["tables"])
	}
	// coordinator ruling (i): a join table not seen in a schema read in part is undecided, never "table absent" (the
	// decoder reports this one as unreadable; TestCheckJoinSchemaNeverCallsAPartialSchemaAbsent pins the case where it
	// reports no such table)
	if got := res.Notes["join_undecided.EventsRawTimes"]; got == "" || got == "table absent" {
		t.Errorf("note join_undecided.EventsRawTimes = %q, want an unreadable or undecided reason, never absent", got)
	}
	if w := warningsWith(res, "calendar-join-undecided: table EventsRawTimes: "); len(w) != 1 || w[0].Locator != "sqlite:schema" {
		t.Errorf("warnings %v, want one schema warning naming EventsRawTimes", res.Warnings)
	}
	if len(res.Records) == 1 {
		if l, _ := rawOf(res.Records[0])["join_undecided"].([]any); !slices.Contains(l, any("EventsRawTimes")) {
			t.Errorf("raw.join_undecided = %v: the EventsRawTimes join is undecided", l)
		}
	}
}
