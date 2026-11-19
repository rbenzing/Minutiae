package androidcalendar

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Spec 2 and the writer's call "enum bar, Android": the codes follow CalendarContract at android-14.0.0_r1 exactly,
// value by value; each entry cites its line, and CROSSCHECK.md lists that line for the same column, value and label;
// a value the contract does not define has no label.
func TestAndroidCalendarEnumsFollowCalendarContract(t *testing.T) {
	want := map[string][]string{ // the binding table of plan 4I3 Task 4, labels by value
		"eventStatus":          {"tentative", "confirmed", "cancelled"},
		"availability":         {"busy", "free", "tentative"},
		"accessLevel":          {"default", "confidential", "private", "public"},
		"attendeeRelationship": {"none", "attendee", "organizer", "performer", "speaker"},
		"attendeeType":         {"none", "required", "optional", "resource"},
		"attendeeStatus":       {"none", "accepted", "declined", "invited", "tentative"},
		"selfAttendeeStatus":   {"none", "accepted", "declined", "invited", "tentative"},
		"Reminders.method":     {"default", "alert", "email", "sms", "alarm"},
		"CalendarAlerts.state": {"scheduled", "fired", "dismissed"},
	}
	n := 0
	for col, labels := range want {
		n += len(labels)
		for v, l := range labels {
			if got, ok := lookupEnum(col, int64(v)); !ok || got != l {
				t.Errorf("%s %d = %q, %v; want %q", col, v, got, ok, l)
			}
		}
		if got, ok := lookupEnum(col, int64(len(labels))); ok {
			t.Errorf("%s %d (outside the contract) has the label %q", col, len(labels), got)
		}
		if _, ok := lookupEnum(col, -1); ok {
			t.Errorf("%s -1 has a label", col)
		}
	}
	if len(enumTable) != n {
		t.Errorf("enumTable has %d entries, the binding table %d", len(enumTable), n)
	}
	cc, err := os.ReadFile(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "calendar-android", "CROSSCHECK.md"))
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`^CalendarContract\.java:[0-9]+$`)
	for _, e := range enumTable {
		if !src.MatchString(e.Source) {
			t.Errorf("%s %d: source %q", e.Column, e.Value, e.Source)
			continue
		}
		col := e.Column[strings.LastIndex(e.Column, ".")+1:]
		found := false
		for _, l := range strings.Split(string(cc), "\n") {
			found = found || strings.HasSuffix(strings.TrimSpace(l), "| "+e.Source+" |") && strings.Contains(l, col+"`") &&
				strings.Contains(l, "| "+strconv.FormatInt(e.Value, 10)+" | "+e.Label+" |")
		}
		if !found {
			t.Errorf("%s %d %s: CROSSCHECK.md lists no row citing %s", e.Column, e.Value, e.Label, e.Source)
		}
	}
	// the mapping: the unmapped values of the plan give "unknown" with the code in raw
	for _, c := range []struct {
		col string
		v   int64
	}{{"eventStatus", 3}, {"availability", 3}, {"accessLevel", 4}, {"attendeeType", 4}, {"attendeeStatus", 5}, {"Reminders.method", 5}} {
		if _, ok := lookupEnum(c.col, c.v); ok {
			t.Errorf("%s %d has a label", c.col, c.v)
		}
	}
	// the mapping of these values (unknown, the code in raw) is pinned by TestAndroidCalendarStateVocabularyIsPinned
}
