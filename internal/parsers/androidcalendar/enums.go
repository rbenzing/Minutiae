package androidcalendar

// enumEntry is one constant of the AOSP platform contract (spec 2, writer's call "enum bar, Android"): the stored
// value of a column and the label the payload states for it. Source cites the line of CalendarContract.java at
// android-14.0.0_r1 (CROSSCHECK.md lists it).
type enumEntry struct {
	Column string
	Value  int64
	Label  string
	Source string
}

// labelUnknown is the label of a stored code the contract does not define (the code stays in raw).
const labelUnknown = "unknown"

// enumTable is the binding table of plan 4I3 Task 4 (spec 2): eventStatus 2 is spelled "cancelled" in the payload
// (the contract's STATUS_CANCELED); the child-table codes are named by their table.
var enumTable = []enumEntry{
	{"eventStatus", 0, "tentative", "CalendarContract.java:1060"},
	{"eventStatus", 1, "confirmed", "CalendarContract.java:1061"},
	{"eventStatus", 2, "cancelled", "CalendarContract.java:1062"},
	{"availability", 0, "busy", "CalendarContract.java:1221"},
	{"availability", 1, "free", "CalendarContract.java:1226"},
	{"availability", 2, "tentative", "CalendarContract.java:1231"},
	{"accessLevel", 0, "default", "CalendarContract.java:1192"},
	{"accessLevel", 1, "confidential", "CalendarContract.java:1196"},
	{"accessLevel", 2, "private", "CalendarContract.java:1200"},
	{"accessLevel", 3, "public", "CalendarContract.java:1205"},
	{"attendeeRelationship", 0, "none", "CalendarContract.java:891"},
	{"attendeeRelationship", 1, "attendee", "CalendarContract.java:892"},
	{"attendeeRelationship", 2, "organizer", "CalendarContract.java:893"},
	{"attendeeRelationship", 3, "performer", "CalendarContract.java:894"},
	{"attendeeRelationship", 4, "speaker", "CalendarContract.java:895"},
	{"attendeeType", 0, "none", "CalendarContract.java:906"},
	{"attendeeType", 1, "required", "CalendarContract.java:907"},
	{"attendeeType", 2, "optional", "CalendarContract.java:908"},
	{"attendeeType", 3, "resource", "CalendarContract.java:913"},
	{"attendeeStatus", 0, "none", "CalendarContract.java:921"},
	{"attendeeStatus", 1, "accepted", "CalendarContract.java:922"},
	{"attendeeStatus", 2, "declined", "CalendarContract.java:923"},
	{"attendeeStatus", 3, "invited", "CalendarContract.java:924"},
	{"attendeeStatus", 4, "tentative", "CalendarContract.java:925"},
	{"selfAttendeeStatus", 0, "none", "CalendarContract.java:921"},
	{"selfAttendeeStatus", 1, "accepted", "CalendarContract.java:922"},
	{"selfAttendeeStatus", 2, "declined", "CalendarContract.java:923"},
	{"selfAttendeeStatus", 3, "invited", "CalendarContract.java:924"},
	{"selfAttendeeStatus", 4, "tentative", "CalendarContract.java:925"},
	{"Reminders.method", 0, "default", "CalendarContract.java:2301"},
	{"Reminders.method", 1, "alert", "CalendarContract.java:2302"},
	{"Reminders.method", 2, "email", "CalendarContract.java:2303"},
	{"Reminders.method", 3, "sms", "CalendarContract.java:2304"},
	{"Reminders.method", 4, "alarm", "CalendarContract.java:2305"},
	{"CalendarAlerts.state", 0, "scheduled", "CalendarContract.java:2405"},
	{"CalendarAlerts.state", 1, "fired", "CalendarContract.java:2410"},
	{"CalendarAlerts.state", 2, "dismissed", "CalendarContract.java:2415"},
}

// lookupEnum returns the label of a stored code of column, and false for a value the contract does not define.
func lookupEnum(column string, v int64) (string, bool) {
	for _, e := range enumTable {
		if e.Column == column && e.Value == v {
			return e.Label, true
		}
	}
	return "", false
}
