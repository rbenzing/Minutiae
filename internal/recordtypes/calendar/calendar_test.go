package calendar_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/calendar"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

func ptr[T any](v T) *T { return &v }

// minimal is the smallest event the contract accepts: a source and an undecided timing.
func minimal() calendar.Event {
	return calendar.Event{Source: "android_calendar", Timing: calendar.TimingUnknown}
}

// allDay is an all-day event of one day: end_date is exclusive.
func allDay() calendar.Event {
	return calendar.Event{
		Source: "android_calendar", Timing: calendar.TimingAllDay, Title: "Holiday",
		StartDate: "2023-11-15", EndDate: "2023-11-16", StartTimezone: "UTC",
	}
}

// floating is a wall-clock event with no zone.
func floating() calendar.Event {
	return calendar.Event{Source: "ios_calendar", Timing: calendar.TimingFloating, Title: "Call", SourceID: "4"}
}

var (
	roles    = []string{"attendee", "organizer", "performer", "speaker", "none", "unknown"}
	types    = []string{"required", "optional", "resource", "none", "unknown"}
	statuses = []string{"accepted", "declined", "invited", "tentative", "none", "unknown"}
	methods  = []string{"default", "alert", "email", "sms", "alarm", "unknown"}
)

// full sets every field: 1,000 attendees (every enum value of role, type and
// status among them) of 1,500, reminders of every method, a recurrence, an
// exception, a calendar with its account, the body facts and a raw object at depth 4.
func full() calendar.Event {
	att := make([]calendar.Attendee, 1000)
	for i := range att {
		att[i] = calendar.Attendee{Role: roles[i%len(roles)], Type: types[i%len(types)], Status: statuses[i%len(statuses)]}
		switch i % 3 {
		case 0:
			att[i].Address = "a" + strings.Repeat("x", i%7) + "@example.test"
		case 1:
			att[i].Name = "Person"
		default:
			att[i].Address, att[i].Name = "b@example.test", "Bea"
		}
	}
	rem := []calendar.Reminder{{BeforeS: ptr(int64(600))}}
	for i, m := range methods {
		rem = append(rem, calendar.Reminder{BeforeS: ptr(int64(i * 60)), Method: m})
	}
	rem = append(rem, calendar.Reminder{Method: "email"}, calendar.Reminder{BeforeS: ptr(int64(-300))})
	return calendar.Event{
		Source: "android_calendar", Timing: calendar.TimingTimed, Title: "Standup", Status: "confirmed",
		StartTimezone: "Europe/Berlin", EndTimezone: "America/New_York", Duration: "P1H", URL: "https://example.test/e/1",
		Location:  &calendar.Location{Text: "Room 4", Address: "1 Main St", Latitude: ptr(52.5), Longitude: ptr(-13.25)},
		Organizer: &calendar.Person{Address: "org@example.test", Name: "Org"},
		Attendees: att, AttendeesTotal: ptr(int64(1500)), AttendeesTruncated: true,
		SelfStatus: "accepted", Availability: "busy", Privacy: "private",
		Reminders: rem, RemindersTotal: ptr(int64(len(rem))), RemindersTruncated: true,
		Recurrence: &calendar.Recurrence{
			RRule: "FREQ=WEEKLY;BYDAY=TU", RDate: "20231121T090000Z", ExRule: "FREQ=MONTHLY", ExDate: "20231128T090000Z",
			StoredRules: ptr(int64(0)),
		},
		ExceptionOf: &calendar.ExceptionOf{EventID: "7", SyncID: "s7"},
		Calendar: &calendar.CalendarRef{
			ID: "1", Title: "Work", Owner: "me@example.test",
			Account: &calendar.Account{Type: "com.google", Name: "me@example.test"},
		},
		UID: "uid-1@example.test", SyncID: "s12", SourceID: "12",
		BodyFlags: []string{common.FlagInvalidUTF8, common.FlagTruncated}, BodyTruncated: ptr(true), BodyTotalBytes: ptr(int64(300000)),
		BodyRawB64: "AAEC", BodyRawSHA256: strings.Repeat("ab", 32), BodyRawLen: ptr(int64(3)),
		TruncatedFields: []string{"title"},
		Deleted:         map[string]any{"source": "Events.lastSynced", "relation": "last-synced-copy"},
		Recovery:        map[string]any{"relation": "absent-from-live", "via": "wal"},
		Snapshot:        map[string]any{"name": "snap", "xid": json.Number("3")},
		Raw:             map[string]any{"a": map[string]any{"b": map[string]any{"c": json.Number("1")}}, "eventStatus": json.Number("1")},
	}
}

func events() map[string]calendar.Event {
	return map[string]calendar.Event{"minimal": minimal(), "all-day": allDay(), "floating": floating(), "full": full()}
}

// TestPayloadValidatorsAcceptParserOutput (calendar part): what the builder makes
// validates, and a real records.Writer over a temp case accepts it.
func TestPayloadValidatorsAcceptParserOutput(t *testing.T) {
	evs := events()
	for name, e := range evs {
		t.Run(name, func(t *testing.T) {
			if err := calendar.Validate(e.Payload()); err != nil {
				t.Fatalf("Validate(builder output) = %v", err)
			}
		})
	}
	if p := minimal().Payload(); len(p) != 2 || p["source"] != "android_calendar" || p["timing"] != "unknown" {
		t.Errorf("minimal payload = %v", p)
	}

	cs := recordstest.NewCase(t)
	a := recordstest.AddArtifact(t, cs, "calendar.db", make([]byte, 1<<16))
	w, err := records.NewWriter(cs, records.Parser{Name: "calendar-test", Version: "1"}, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for name, e := range evs {
		rec := records.Record{Type: calendar.Type, ArtifactID: a.ID, Summary: calendar.Summary(e, nil), Payload: e.Payload()}
		if err := w.Add(ctx, rec); err != nil {
			t.Errorf("the writer refused the %s event: %v", name, err)
		}
	}
	bad := records.Record{Type: calendar.Type, ArtifactID: a.ID, Summary: "bad", Payload: map[string]any{"timing": "timed"}}
	if err := w.Add(ctx, bad); !errors.Is(err, records.ErrInvalidPayload) {
		t.Errorf("Add(invalid event) = %v, want ErrInvalidPayload", err)
	}
	if res, err := w.End(ctx); err != nil || res.Records != int64(len(evs)) || res.Rejected != 1 {
		t.Fatalf("End = %+v, %v", res, err)
	}
}

func TestCalendarRegisteredValidator(t *testing.T) {
	ti, ok := records.LookupType(calendar.Type)
	if !ok {
		t.Fatal("calendar_event is not a registered type")
	}
	if ti.Validate == nil {
		t.Error("calendar_event has no validator although its package is imported")
	}
	if ti.PayloadVersion != 1 || calendar.PayloadVersion != 1 {
		t.Errorf("registered payload version %d, package says %d, want 1", ti.PayloadVersion, calendar.PayloadVersion)
	}
}

func with(base calendar.Event, fn func(p map[string]any)) map[string]any {
	p := base.Payload()
	fn(p)
	return p
}

// TestPayloadValidatorsRejectViolations (calendar part): one row per rule. Every
// error names the path, and none repeats a value.
func TestPayloadValidatorsRejectViolations(t *testing.T) {
	const marker = "SECRET-VALUE-5521"
	long := func(n int) string { return marker + strings.Repeat("x", n-len(marker)) }
	attendees := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"address": "a@example.test"}
		}
		return out
	}
	rows := []struct {
		name string
		p    map[string]any
		want string
	}{
		{"no source", with(full(), func(p map[string]any) { delete(p, "source") }), "payload.source: required field is missing"},
		{"no timing", with(full(), func(p map[string]any) { delete(p, "timing") }), "payload.timing: required field is missing"},
		{"source not a token", with(full(), func(p map[string]any) { p["source"] = "Android Calendar" }), "payload.source"},
		{"timing daily", with(full(), func(p map[string]any) { p["timing"] = "daily" }), "payload.timing: not one of timed,all_day,floating"},
		{"start_date with timing timed", with(full(), func(p map[string]any) { p["start_date"] = "2023-11-15" }), "start_date"},
		{"end_date with timing floating", with(floating(), func(p map[string]any) { p["end_date"] = "2023-11-15" }), "end_date"},
		{"end_date before start_date", with(allDay(), func(p map[string]any) { p["end_date"] = "2023-11-14" }), "end_date"},
		{"start_date without a year", with(allDay(), func(p map[string]any) { p["start_date"] = "--11-15" }), "start_date"},
		{"end_date without a year", with(allDay(), func(p map[string]any) { p["end_date"] = "--11-16" }), "end_date"},
		{"start_date not a date", with(allDay(), func(p map[string]any) { p["start_date"] = "2023-02-30" }), "payload.start_date"},
		{"attendees_truncated without attendees_total", with(full(), func(p map[string]any) { delete(p, "attendees_total") }), "attendees_truncated requires attendees_total"},
		{"reminders_truncated without reminders_total", with(full(), func(p map[string]any) { delete(p, "reminders_total") }), "reminders_truncated requires reminders_total"},
		{"body_truncated without body_total_bytes", with(full(), func(p map[string]any) { delete(p, "body_total_bytes") }), "body_truncated requires body_total_bytes"},
		{"body_flags entry", with(full(), func(p map[string]any) { p["body_flags"] = []any{marker} }), "body_flags"},
		{"1,001 attendees", with(full(), func(p map[string]any) { p["attendees"] = attendees(1001) }), "payload.attendees: more than 1000 entries"},
		{"1,001 reminders", with(full(), func(p map[string]any) { p["reminders"] = make([]any, 1001) }), "payload.reminders: more than 1000 entries"},
		{"attendee with a role only", with(full(), func(p map[string]any) {
			p["attendees"] = []any{map[string]any{"role": "attendee"}}
		}), "payload.attendees[0]: at least one of address,name"},
		{"attendee role", with(full(), func(p map[string]any) { p["attendees"] = []any{map[string]any{"name": "x", "role": "chair"}} }), "payload.attendees[0].role"},
		{"attendee type", with(full(), func(p map[string]any) { p["attendees"] = []any{map[string]any{"name": "x", "type": "chair"}} }), "payload.attendees[0].type"},
		{"attendee status", with(full(), func(p map[string]any) { p["attendees"] = []any{map[string]any{"name": "x", "status": "maybe"}} }), "payload.attendees[0].status"},
		{"location empty", with(full(), func(p map[string]any) { p["location"] = map[string]any{} }), "payload.location: at least one of text,address,latitude,longitude"},
		{"latitude 91", with(full(), func(p map[string]any) { p["location"] = map[string]any{"latitude": float64(91)} }), "payload.location: latitude"},
		{"latitude -90.5", with(full(), func(p map[string]any) { p["location"] = map[string]any{"latitude": json.Number("-90.5")} }), "payload.location: latitude"},
		{"longitude 181", with(full(), func(p map[string]any) { p["location"] = map[string]any{"longitude": int64(181)} }), "payload.location: longitude"},
		{"latitude a string", with(full(), func(p map[string]any) { p["location"] = map[string]any{"latitude": "1"} }), "payload.location.latitude"},
		{"organizer empty", with(full(), func(p map[string]any) { p["organizer"] = map[string]any{} }), "payload.organizer: at least one of address,name"},
		{"reminder empty", with(full(), func(p map[string]any) { p["reminders"] = []any{map[string]any{}} }), "payload.reminders[0]: at least one of before_s,method"},
		{"reminder method", with(full(), func(p map[string]any) { p["reminders"] = []any{map[string]any{"method": "pager"}} }), "payload.reminders[0].method"},
		{"recurrence empty", with(full(), func(p map[string]any) { p["recurrence"] = map[string]any{} }), "payload.recurrence: at least one of"},
		{"recurrence stored_rules negative", with(full(), func(p map[string]any) { p["recurrence"] = map[string]any{"stored_rules": int64(-1)} }), "payload.recurrence.stored_rules"},
		{"rrule of 16,385 bytes", with(full(), func(p map[string]any) { p["recurrence"] = map[string]any{"rrule": long(16385)} }), "payload.recurrence.rrule: longer than 16384 bytes"},
		{"exception_of empty", with(full(), func(p map[string]any) { p["exception_of"] = map[string]any{} }), "payload.exception_of: at least one of event_id,sync_id"},
		{"calendar empty", with(full(), func(p map[string]any) { p["calendar"] = map[string]any{} }), "payload.calendar: at least one of"},
		{"account empty", with(full(), func(p map[string]any) { p["calendar"] = map[string]any{"account": map[string]any{}} }), "payload.calendar.account: at least one of type,name"},
		{"start_timezone of 257 bytes", with(full(), func(p map[string]any) { p["start_timezone"] = long(257) }), "payload.start_timezone: longer than 256 bytes"},
		{"end_timezone of 257 bytes", with(full(), func(p map[string]any) { p["end_timezone"] = long(257) }), "payload.end_timezone: longer than 256 bytes"},
		{"duration of 65 bytes", with(full(), func(p map[string]any) { p["duration"] = long(65) }), "payload.duration: longer than 64 bytes"},
		{"uid of 1,025 bytes", with(full(), func(p map[string]any) { p["uid"] = long(1025) }), "payload.uid: longer than 1024 bytes"},
		{"sync_id of 1,025 bytes", with(full(), func(p map[string]any) { p["sync_id"] = long(1025) }), "payload.sync_id: longer than 1024 bytes"},
		{"status done", with(full(), func(p map[string]any) { p["status"] = "done" }), "payload.status: not one of confirmed,tentative,cancelled"},
		{"availability", with(full(), func(p map[string]any) { p["availability"] = marker }), "payload.availability"},
		{"privacy", with(full(), func(p map[string]any) { p["privacy"] = "secret" }), "payload.privacy"},
		{"self_status", with(full(), func(p map[string]any) { p["self_status"] = "going" }), "payload.self_status"},
		{"title empty", with(full(), func(p map[string]any) { p["title"] = "" }), "payload.title: must not be empty"},
		{"attendees_total negative", with(full(), func(p map[string]any) { p["attendees_total"] = int64(-1) }), "payload.attendees_total"},
		{"truncated_fields not strings", with(full(), func(p map[string]any) { p["truncated_fields"] = []any{int64(1)} }), "payload.truncated_fields"},
		{"deleted without source", with(full(), func(p map[string]any) { p["deleted"] = map[string]any{"relation": "last-synced-copy"} }), "payload.deleted.source"},
		{"raw nested five levels deep", with(full(), func(p map[string]any) {
			p["raw"] = map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{}}}}
		}), "payload.raw"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			err := calendar.Validate(r.p)
			if err == nil {
				t.Fatal("an invalid payload was accepted")
			}
			if !strings.Contains(err.Error(), r.want) {
				t.Errorf("error %q does not contain %q", err, r.want)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("error repeats a payload value: %q", err)
			}
		})
	}

	// the boundaries that are accepted
	ok := map[string]map[string]any{
		"1,000 attendees":               with(full(), func(p map[string]any) { p["attendees"] = attendees(1000) }),
		"one-day all-day (end = start)": with(allDay(), func(p map[string]any) { p["end_date"] = "2023-11-15" }),
		"start_date only":               with(allDay(), func(p map[string]any) { delete(p, "end_date") }),
		"end_date only":                 with(allDay(), func(p map[string]any) { delete(p, "start_date") }),
		"timezones of 256 bytes": with(full(), func(p map[string]any) {
			p["start_timezone"], p["end_timezone"] = strings.Repeat("z", 256), strings.Repeat("z", 256)
		}),
		"coordinates at the poles": with(full(), func(p map[string]any) {
			p["location"] = map[string]any{"latitude": float64(-90), "longitude": json.Number("180")}
		}),
		"unknown enum words": with(full(), func(p map[string]any) {
			p["status"], p["availability"], p["privacy"], p["self_status"] = "unknown", "unknown", "unknown", "unknown"
		}),
		"truncated false needs no total": with(full(), func(p map[string]any) {
			p["attendees_truncated"], p["reminders_truncated"], p["body_truncated"] = false, false, false
			delete(p, "attendees_total")
			delete(p, "reminders_total")
			delete(p, "body_total_bytes")
		}),
		"integer exception id": with(full(), func(p map[string]any) { p["exception_of"] = map[string]any{"event_id": int64(7)} }),
		"unknown fields":       with(full(), func(p map[string]any) { p["future_field"] = map[string]any{"x": int64(1)} }),
	}
	for name, p := range ok {
		if err := calendar.Validate(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// v1Fixture is a stored v1 payload as the writer canonicalizes it, written out by
// hand so it does not move when the builder changes. It carries a field a future
// version might add (future_field) and an integer exception id.
const v1Fixture = `{
  "attendees": [{"address": "a@example.test", "role": "attendee", "status": "accepted"}, {"name": "Bea", "type": "optional"}],
  "attendees_total": 3,
  "attendees_truncated": true,
  "availability": "busy",
  "calendar": {"id": 1, "title": "Work", "account": {"type": "com.google", "name": "me@example.test"}},
  "deleted": {"source": "Events.lastSynced", "relation": "last-synced-copy"},
  "duration": "P1H",
  "end_timezone": "UTC",
  "exception_of": {"event_id": 7, "sync_id": "s7"},
  "future_field": {"anything": [1, 2, 3]},
  "location": {"text": "Room 4", "latitude": 52.5, "longitude": -13},
  "organizer": {"name": "Org"},
  "raw": {"eventStatus": 1, "ratio": 0.5},
  "recurrence": {"rrule": "FREQ=DAILY", "stored_rules": 1},
  "reminders": [{"before_s": 600, "method": "alert"}],
  "source": "android_calendar",
  "source_id": 12,
  "start_timezone": "Europe/Berlin",
  "status": "tentative",
  "timing": "timed",
  "title": "Standup",
  "truncated_fields": ["title"]
}`

// TestCalendarEventDecodeRoundTrip: the current payload version decodes, as a round
// trip and from a committed v1 fixture; unknown fields are ignored; any other
// version is ErrUnsupportedPayloadVersion; a damaged payload is an error.
func TestCalendarEventDecodeRoundTrip(t *testing.T) {
	t.Run("the version constant is the registered payload version", func(t *testing.T) {
		ty, ok := records.LookupType(calendar.Type)
		if !ok {
			t.Fatal("calendar_event is not a registered type")
		}
		if ty.PayloadVersion != calendar.PayloadVersion {
			t.Errorf("registered payload version %d, package constant %d", ty.PayloadVersion, calendar.PayloadVersion)
		}
	})

	t.Run("round trip equals the input", func(t *testing.T) {
		for name, e := range events() {
			b, err := json.Marshal(e.Payload())
			if err != nil {
				t.Fatal(err)
			}
			got, err := calendar.Decode(calendar.PayloadVersion, b)
			if err != nil {
				t.Fatalf("%s: Decode = %v", name, err)
			}
			if !reflect.DeepEqual(got, e) {
				t.Errorf("%s: round trip\n got  %+v\n want %+v", name, got, e)
			}
			if !reflect.DeepEqual(got.Payload(), e.Payload()) {
				t.Errorf("%s: the decoded event builds another payload", name)
			}
		}
	})

	t.Run("v1 fixture bytes decode", func(t *testing.T) {
		got, err := calendar.Decode(1, []byte(v1Fixture))
		if err != nil {
			t.Fatalf("Decode(v1 fixture) = %v", err)
		}
		want := calendar.Event{
			Source: "android_calendar", Timing: "timed", Title: "Standup", Status: "tentative",
			StartTimezone: "Europe/Berlin", EndTimezone: "UTC", Duration: "P1H",
			Location:  &calendar.Location{Text: "Room 4", Latitude: ptr(52.5), Longitude: ptr(-13.0)},
			Organizer: &calendar.Person{Name: "Org"},
			Attendees: []calendar.Attendee{
				{Address: "a@example.test", Role: "attendee", Status: "accepted"},
				{Name: "Bea", Type: "optional"},
			},
			AttendeesTotal: ptr(int64(3)), AttendeesTruncated: true, Availability: "busy",
			Reminders:   []calendar.Reminder{{BeforeS: ptr(int64(600)), Method: "alert"}},
			Recurrence:  &calendar.Recurrence{RRule: "FREQ=DAILY", StoredRules: ptr(int64(1))},
			ExceptionOf: &calendar.ExceptionOf{EventID: "7", SyncID: "s7"},
			Calendar: &calendar.CalendarRef{
				ID: "1", Title: "Work", Account: &calendar.Account{Type: "com.google", Name: "me@example.test"},
			},
			SourceID:        "12",
			TruncatedFields: []string{"title"},
			Deleted:         map[string]any{"source": "Events.lastSynced", "relation": "last-synced-copy"},
			Raw:             map[string]any{"eventStatus": json.Number("1"), "ratio": json.Number("0.5")},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Decode(v1 fixture)\n got  %+v\n want %+v", got, want)
		}
		if got.Live() {
			t.Error("the fixture's last-synced copy decodes as live")
		}
	})

	t.Run("a future-added unknown field is ignored", func(t *testing.T) {
		got, err := calendar.Decode(1, []byte(`{"source":"ios_calendar","timing":"floating","new_in_v1_1":{"deep":[1,{"x":null}]},"another":"x"}`))
		if err != nil {
			t.Fatalf("Decode = %v", err)
		}
		if !reflect.DeepEqual(got, calendar.Event{Source: "ios_calendar", Timing: "floating"}) {
			t.Errorf("Decode = %+v", got)
		}
	})

	t.Run("other payload versions are unsupported", func(t *testing.T) {
		for _, v := range []int{0, 2, 3, -1, 1 << 30} {
			if _, err := calendar.Decode(v, []byte(v1Fixture)); !errors.Is(err, calendar.ErrUnsupportedPayloadVersion) {
				t.Errorf("Decode(%d) = %v, want ErrUnsupportedPayloadVersion", v, err)
			}
		}
	})

	t.Run("damaged payloads are errors, never a panic and never an unsupported version", func(t *testing.T) {
		const marker = "SECRET-VALUE-7710"
		for name, b := range map[string]string{
			"empty":            ``,
			"not json":         `{"source": `,
			"null":             `null`,
			"array":            `[]`,
			"string":           `"x"`,
			"trailing value":   `{"source":"android_calendar","timing":"timed"} {}`,
			"trailing garbage": `{"source":"android_calendar","timing":"timed"} x`,
			"missing timing":   `{"source":"android_calendar"}`,
			"wrong type":       `{"source":"android_calendar","timing":"timed","title":["` + marker + `"]}`,
			"bad enum":         `{"source":"android_calendar","timing":"` + marker + `"}`,
		} {
			_, err := calendar.Decode(1, []byte(b))
			if err == nil {
				t.Errorf("%s: accepted", name)
				continue
			}
			if errors.Is(err, calendar.ErrUnsupportedPayloadVersion) {
				t.Errorf("%s: reported as an unsupported version: %v", name, err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("%s: error repeats a payload value: %v", name, err)
			}
		}
	})
}

// TestCalendarEventLiveIsFalseWithProvenance: an event that carries recovery,
// snapshot or deleted provenance (even an empty object) is never live.
func TestCalendarEventLiveIsFalseWithProvenance(t *testing.T) {
	if !minimal().Live() || !allDay().Live() {
		t.Error("a plain event is not live")
	}
	for name, e := range map[string]calendar.Event{
		"recovery":       {Recovery: map[string]any{"relation": "absent-from-live"}},
		"snapshot":       {Snapshot: map[string]any{"name": "s", "xid": json.Number("1")}},
		"deleted":        {Deleted: map[string]any{"source": "Events.deleted"}},
		"last-synced":    {Deleted: map[string]any{"source": "Events.lastSynced", "relation": "last-synced-copy"}},
		"empty recovery": {Recovery: map[string]any{}},
		"full":           full(),
	} {
		if e.Live() {
			t.Errorf("%s: Live() = true", name)
		}
	}
}

func TestDateOfMidnight(t *testing.T) {
	mid := time.Date(2023, 11, 15, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		t    time.Time
		want string
		ok   bool
	}{
		"midnight":             {mid, "2023-11-15", true},
		"epoch":                {time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), "1970-01-01", true},
		"leap day":             {time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), "2024-02-29", true},
		"plus 1ns":             {mid.Add(time.Nanosecond), "", false},
		"plus 1s":              {mid.Add(time.Second), "", false},
		"23:59:59":             {mid.Add(24*time.Hour - time.Second), "", false},
		"minus 1ns":            {mid.Add(-time.Nanosecond), "", false},
		"local midnight":       {time.Date(2023, 11, 15, 0, 0, 0, 0, time.FixedZone("x", 3600)), "", false}, // 23:00 UTC
		"zone at UTC midnight": {time.Date(2023, 11, 15, 1, 0, 0, 0, time.FixedZone("x", 3600)), "2023-11-15", true},
	} {
		got, ok := calendar.DateOfMidnight(tc.t)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: DateOfMidnight = %q, %v, want %q, %v", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCalendarSummary(t *testing.T) {
	at := func(y int, mo time.Month, d, h, mi, s int, b records.Basis) *records.Time {
		return &records.Time{T: time.Date(y, mo, d, h, mi, s, 0, time.UTC), Basis: b}
	}
	day := at(2023, 11, 15, 0, 0, 0, records.BasisLocalUnknown)
	instant := at(2023, 11, 14, 22, 13, 20, records.BasisUTC)
	later := at(2023, 11, 21, 22, 43, 20, records.BasisUTC)
	wall := at(2023, 11, 14, 22, 13, 20, records.BasisLocalUnknown)
	title300 := strings.Repeat("t", 300)
	for name, tc := range map[string]struct {
		e    calendar.Event
		t    *records.Time
		want string
	}{
		"all-day": {calendar.Event{Timing: calendar.TimingAllDay, Title: "Holiday"}, day, "calendar 2023-11-15 Holiday"},
		"series": {
			calendar.Event{Timing: calendar.TimingTimed, Title: "Standup", Recurrence: &calendar.Recurrence{RRule: "FREQ=WEEKLY"}, Location: &calendar.Location{Text: "Room 4"}},
			instant, "calendar series 2023-11-14T22:13:20Z Standup @ Room 4",
		},
		"exception cancelled": {
			calendar.Event{Timing: calendar.TimingTimed, Title: "Standup", Status: "cancelled", ExceptionOf: &calendar.ExceptionOf{EventID: "7"}},
			later, "calendar exception 2023-11-21T22:43:20Z Standup [cancelled]",
		},
		"series wins over exception": {
			calendar.Event{Timing: calendar.TimingTimed, Title: "X", Recurrence: &calendar.Recurrence{RRule: "R"}, ExceptionOf: &calendar.ExceptionOf{EventID: "7"}},
			instant, "calendar series 2023-11-14T22:13:20Z X",
		},
		"floating":          {calendar.Event{Timing: calendar.TimingFloating, Title: "Call"}, wall, "calendar 2023-11-14T22:13:20 (floating) Call"},
		"no time":           {calendar.Event{Timing: calendar.TimingUnknown, Title: "Untimed"}, nil, "calendar Untimed"},
		"unknown with time": {calendar.Event{Timing: calendar.TimingUnknown, Title: "Untimed"}, instant, "calendar Untimed"},
		"no title":          {calendar.Event{Timing: calendar.TimingTimed}, instant, "calendar 2023-11-14T22:13:20Z"},
		"location without text": {
			calendar.Event{Timing: calendar.TimingUnknown, Title: "T", Location: &calendar.Location{Address: "1 Main St"}}, nil, "calendar T",
		},
		"long title": {calendar.Event{Timing: calendar.TimingUnknown, Title: title300}, nil, "calendar " + common.Summarize(title300, 120)},
		"long location": {
			calendar.Event{Timing: calendar.TimingUnknown, Title: "T", Location: &calendar.Location{Text: title300}},
			nil,
			"calendar T @ " + common.Summarize(title300, 64),
		},
		"bidi title": {calendar.Event{Timing: calendar.TimingUnknown, Title: "a" + string(rune(0x202e)) + "b"}, nil, "calendar a<U+202E>b"},
	} {
		if got := calendar.Summary(tc.e, tc.t); got != tc.want {
			t.Errorf("%s: Summary = %q, want %q", name, got, tc.want)
		}
	}
	if got := calendar.Summary(calendar.Event{Timing: calendar.TimingUnknown, Title: title300}, nil); len(got) > len("calendar ")+120 {
		t.Errorf("the title is not cut: %d bytes", len(got))
	}
	// When by itself
	for name, tc := range map[string]struct {
		t      *records.Time
		timing string
		want   string
	}{
		"nil":      {nil, calendar.TimingTimed, ""},
		"unknown":  {instant, calendar.TimingUnknown, ""},
		"other":    {instant, "weekly", ""},
		"all-day":  {day, calendar.TimingAllDay, "2023-11-15"},
		"timed":    {instant, calendar.TimingTimed, "2023-11-14T22:13:20Z"},
		"floating": {wall, calendar.TimingFloating, "2023-11-14T22:13:20 (floating)"},
	} {
		if got := calendar.When(tc.t, tc.timing); got != tc.want {
			t.Errorf("When(%s) = %q, want %q", name, got, tc.want)
		}
	}
}
