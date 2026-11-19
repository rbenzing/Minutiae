// Package calendar defines the payload of the "calendar_event" record type,
// version 1: the builder parsers use to produce it (Event.Payload), the validator
// the records writer runs on every event (Validate, installed at init), the typed
// reader for stored payloads (Decode), the all-day date helper (DateOfMidnight)
// and the record Summary. It is a pure package: it imports only internal/records
// (SetValidator, in init, and the Time data type) and internal/recordtypes/common.
//
// The payload follows the contract of plan 4I3 (spec 4.1): required source (the
// store family, a token such as android_calendar or ios_calendar) and timing;
// everything else optional and absent (never an empty string, object or list)
// when the parser does not know it (C3). Names, addresses, zone names, rules and
// identifiers are the source strings, never normalized (C5). Unknown fields in a
// stored payload are ignored by Decode; the provenance objects recovery, snapshot
// and deleted are never ignored. A non-live copy kept by the store's own
// structure (the Android provider's last-synced copy) is deleted with a relation,
// {source: "Events.lastSynced", relation: "last-synced-copy"}, and never live.
//
// Timing and times. The event's instants live in Record.Time (the start) and
// Record.TimeEnd (the end, same basis), not in the payload. timing says how to
// read them: "timed" is a true instant (basis utc); "all_day" is a date, stated
// as start_date/end_date and as Time/TimeEnd at 00:00 "as if UTC" with basis
// local-unknown; "floating" is a wall-clock reading with no zone (basis
// local-unknown); "unknown" means the timing is undecidable and the record has no
// Time. end_date is exclusive: the first day the event does not cover (RFC 5545
// DTEND;VALUE=DATE and the Android midnight rule), so a one-day event on
// 2023-11-15 has end_date 2023-11-16. A date is taken only from a stored value
// that is exactly a UTC midnight (DateOfMidnight); nothing here repairs another
// value. The stored zone names (start_timezone, end_timezone) are stated as
// stored and never applied: applying one needs the host's zone database, and a
// zone's rules at the event's date are not the rules of today.
//
// Time keys. An exception row of a recurring series replaces one occurrence; the
// start of the occurrence it replaces (Android originalInstanceTime, its basis
// set by originalAllDay as for the event's own start) is stated under the
// Record.Times kind OriginalStart ("original_start"), an amendment of the parent
// C7 vocabulary. exception_of names the series the row belongs to.
//
// Recurrence is stored, never expanded: recurrence holds the rule texts as
// stored (or, on iOS, the number of stored rules), and the record is the series
// anchor.
//
// A parser that has to sanitise text the contract refuses (invalid UTF-8, NUL)
// keeps the original bytes in raw (base64): the payload stores only what the
// writer accepts, and the typed value does not flag the replacement.
package calendar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// Type is the record type name and PayloadVersion the payload version this
// package writes and reads.
const (
	Type           = "calendar_event"
	PayloadVersion = 1
)

// OriginalStart is the Record.Times kind of the start of the occurrence an
// exception row replaces (see the package documentation, "Time keys").
const OriginalStart = "original_start"

// Timings: how the record's times are to be read (see the package documentation).
const (
	TimingTimed    = "timed"
	TimingAllDay   = "all_day"
	TimingFloating = "floating"
	TimingUnknown  = "unknown"
)

// Caps of the contract.
const (
	maxList       = 1000  // attendees and reminders
	maxZone       = 256   // a stored zone name
	maxDuration   = 64    // the stored duration text
	maxIdentifier = 1024  // uid and sync_id
	maxRule       = 16384 // each recurrence text
)

// ErrUnsupportedPayloadVersion is returned by Decode for a payload version this
// package does not read.
var ErrUnsupportedPayloadVersion = errors.New("calendar: unsupported payload version")

// Location is where the event takes place: the stored text, a structured address
// and coordinates (only when finite and in range).
type Location struct {
	Text, Address       string
	Latitude, Longitude *float64
}

// Person is the organizer as stored.
type Person struct{ Address, Name string }

// Attendee is one attendee as stored, with the mapped role, type and status.
type Attendee struct{ Address, Name, Role, Type, Status string }

// Reminder is one reminder: seconds before the start, as the source states it,
// and its method.
type Reminder struct {
	BeforeS *int64
	Method  string
}

// Recurrence is the stored recurrence, never expanded: the four rule texts as
// stored, or the number of stored rule rows.
type Recurrence struct {
	RRule, RDate, ExRule, ExDate string
	StoredRules                  *int64
}

// ExceptionOf names the series an exception row belongs to.
type ExceptionOf struct{ EventID, SyncID string }

// Account is the account a calendar belongs to.
type Account struct{ Type, Name string }

// CalendarRef is the calendar the event belongs to, from the join.
type CalendarRef struct { //nolint:revive // the name is part of the plan's interface; Ref alone would not say what it refers to
	ID, Title, Owner string
	Account          *Account
}

// Event is the typed form of a calendar_event payload. A zero value (empty
// string, nil pointer, empty list, false truncation flag) means "not known" and
// is absent from the payload. Deleted, Recovery and Snapshot are provenance: a
// typed reader must not present an event that carries them as a live one.
type Event struct {
	Source, Timing, Title, Status, StartDate, EndDate, StartTimezone, EndTimezone, Duration, URL string
	Location                                                                                     *Location
	Organizer                                                                                    *Person
	Attendees                                                                                    []Attendee
	AttendeesTotal                                                                               *int64
	AttendeesTruncated                                                                           bool
	SelfStatus, Availability, Privacy                                                            string
	Reminders                                                                                    []Reminder
	RemindersTotal                                                                               *int64
	RemindersTruncated                                                                           bool
	Recurrence                                                                                   *Recurrence
	ExceptionOf                                                                                  *ExceptionOf
	Calendar                                                                                     *CalendarRef
	UID, SyncID, SourceID                                                                        string
	BodyFlags                                                                                    []string
	BodyTruncated                                                                                *bool
	BodyTotalBytes                                                                               *int64
	BodyRawB64, BodyRawSHA256                                                                    string
	BodyRawLen                                                                                   *int64
	TruncatedFields                                                                              []string
	Deleted, Recovery, Snapshot, Raw                                                             map[string]any
}

func put(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

func putInt(m map[string]any, key string, v *int64) {
	if v != nil {
		m[key] = *v
	}
}

func putFloat(m map[string]any, key string, v *float64) {
	if v != nil {
		m[key] = *v
	}
}

func putTrue(m map[string]any, key string, v bool) {
	if v {
		m[key] = true
	}
}

func putStrings(m map[string]any, key string, in []string) {
	if len(in) == 0 {
		return
	}
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	m[key] = out
}

func putMap(m map[string]any, key string, v map[string]any) {
	if v != nil {
		m[key] = common.CopyMap(v)
	}
}

// Payload returns the payload map in the canonical value types of the records
// package (string, bool, int64, float64, []any, map[string]any). Empty strings,
// nil pointers, empty lists and false truncation flags are omitted; a pointer
// that is set is written even when 0. The free-form objects are deep-copied, so
// the payload never aliases the Event.
func (e Event) Payload() map[string]any {
	p := map[string]any{}
	put(p, "source", e.Source)
	put(p, "timing", e.Timing)
	put(p, "title", e.Title)
	put(p, "status", e.Status)
	put(p, "start_date", e.StartDate)
	put(p, "end_date", e.EndDate)
	put(p, "start_timezone", e.StartTimezone)
	put(p, "end_timezone", e.EndTimezone)
	put(p, "duration", e.Duration)
	put(p, "url", e.URL)
	if l := e.Location; l != nil {
		m := map[string]any{}
		put(m, "text", l.Text)
		put(m, "address", l.Address)
		putFloat(m, "latitude", l.Latitude)
		putFloat(m, "longitude", l.Longitude)
		p["location"] = m
	}
	if o := e.Organizer; o != nil {
		m := map[string]any{}
		put(m, "address", o.Address)
		put(m, "name", o.Name)
		p["organizer"] = m
	}
	if len(e.Attendees) > 0 {
		list := make([]any, len(e.Attendees))
		for i, a := range e.Attendees {
			m := map[string]any{}
			put(m, "address", a.Address)
			put(m, "name", a.Name)
			put(m, "role", a.Role)
			put(m, "type", a.Type)
			put(m, "status", a.Status)
			list[i] = m
		}
		p["attendees"] = list
	}
	putInt(p, "attendees_total", e.AttendeesTotal)
	putTrue(p, "attendees_truncated", e.AttendeesTruncated)
	put(p, "self_status", e.SelfStatus)
	put(p, "availability", e.Availability)
	put(p, "privacy", e.Privacy)
	if len(e.Reminders) > 0 {
		list := make([]any, len(e.Reminders))
		for i, r := range e.Reminders {
			m := map[string]any{}
			putInt(m, "before_s", r.BeforeS)
			put(m, "method", r.Method)
			list[i] = m
		}
		p["reminders"] = list
	}
	putInt(p, "reminders_total", e.RemindersTotal)
	putTrue(p, "reminders_truncated", e.RemindersTruncated)
	if r := e.Recurrence; r != nil {
		m := map[string]any{}
		put(m, "rrule", r.RRule)
		put(m, "rdate", r.RDate)
		put(m, "exrule", r.ExRule)
		put(m, "exdate", r.ExDate)
		putInt(m, "stored_rules", r.StoredRules)
		p["recurrence"] = m
	}
	if x := e.ExceptionOf; x != nil {
		m := map[string]any{}
		put(m, "event_id", x.EventID)
		put(m, "sync_id", x.SyncID)
		p["exception_of"] = m
	}
	if c := e.Calendar; c != nil {
		m := map[string]any{}
		put(m, "id", c.ID)
		put(m, "title", c.Title)
		put(m, "owner", c.Owner)
		if a := c.Account; a != nil {
			am := map[string]any{}
			put(am, "type", a.Type)
			put(am, "name", a.Name)
			m["account"] = am
		}
		p["calendar"] = m
	}
	put(p, "uid", e.UID)
	put(p, "sync_id", e.SyncID)
	put(p, "source_id", e.SourceID)
	putStrings(p, "body_flags", e.BodyFlags)
	if e.BodyTruncated != nil {
		p["body_truncated"] = *e.BodyTruncated
	}
	putInt(p, "body_total_bytes", e.BodyTotalBytes)
	put(p, "body_raw_b64", e.BodyRawB64)
	put(p, "body_raw_sha256", e.BodyRawSHA256)
	putInt(p, "body_raw_len", e.BodyRawLen)
	putStrings(p, "truncated_fields", e.TruncatedFields)
	putMap(p, "deleted", e.Deleted)
	putMap(p, "recovery", e.Recovery)
	putMap(p, "snapshot", e.Snapshot)
	putMap(p, "raw", e.Raw)
	return p
}

// The payload contract. The schema is an unexported package variable used only as
// the receiver of Validate (the purity rules accept nothing else).
var schema = common.Schema{
	Fields: append([]common.Field{
		{Name: "source", Kind: common.KToken, Required: true},
		{Name: "timing", Kind: common.KEnum, Required: true, Enum: []string{TimingTimed, TimingAllDay, TimingFloating}},
		{Name: "title", Kind: common.KString, NonEmpty: true},
		{Name: "status", Kind: common.KEnum, Enum: []string{"confirmed", "tentative", "cancelled"}},
		{Name: "start_date", Kind: common.KDate},
		{Name: "end_date", Kind: common.KDate},
		{Name: "start_timezone", Kind: common.KString, NonEmpty: true, MaxLen: maxZone},
		{Name: "end_timezone", Kind: common.KString, NonEmpty: true, MaxLen: maxZone},
		{Name: "duration", Kind: common.KString, NonEmpty: true, MaxLen: maxDuration},
		{Name: "url", Kind: common.KString, NonEmpty: true},
		{Name: "location", Kind: common.KObject, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "text", Kind: common.KString, NonEmpty: true},
				{Name: "address", Kind: common.KString, NonEmpty: true},
				{Name: "latitude", Kind: common.KNumber},
				{Name: "longitude", Kind: common.KNumber},
			},
			AtLeastOne: []string{"text", "address", "latitude", "longitude"},
			Cross:      coordinatesInRange,
		}},
		{Name: "organizer", Kind: common.KObject, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "address", Kind: common.KString, NonEmpty: true},
				{Name: "name", Kind: common.KString, NonEmpty: true},
			},
			AtLeastOne: []string{"address", "name"},
		}},
		{Name: "attendees", Kind: common.KArray, MaxLen: maxList, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "address", Kind: common.KString, NonEmpty: true},
				{Name: "name", Kind: common.KString, NonEmpty: true},
				{Name: "role", Kind: common.KEnum, Enum: []string{"attendee", "organizer", "performer", "speaker", "none"}},
				{Name: "type", Kind: common.KEnum, Enum: []string{"required", "optional", "resource", "none"}},
				{Name: "status", Kind: common.KEnum, Enum: []string{"accepted", "declined", "invited", "tentative", "none"}},
			},
			AtLeastOne: []string{"address", "name"},
		}},
		{Name: "attendees_total", Kind: common.KInt, Min: new(int64)},
		{Name: "attendees_truncated", Kind: common.KBool},
		{Name: "self_status", Kind: common.KEnum, Enum: []string{"accepted", "declined", "invited", "tentative", "none"}},
		{Name: "availability", Kind: common.KEnum, Enum: []string{"busy", "free", "tentative"}},
		{Name: "privacy", Kind: common.KEnum, Enum: []string{"default", "confidential", "private", "public"}},
		{Name: "reminders", Kind: common.KArray, MaxLen: maxList, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "before_s", Kind: common.KInt},
				{Name: "method", Kind: common.KEnum, Enum: []string{"default", "alert", "email", "sms", "alarm"}},
			},
			AtLeastOne: []string{"before_s", "method"},
		}},
		{Name: "reminders_total", Kind: common.KInt, Min: new(int64)},
		{Name: "reminders_truncated", Kind: common.KBool},
		{Name: "recurrence", Kind: common.KObject, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "rrule", Kind: common.KString, NonEmpty: true, MaxLen: maxRule},
				{Name: "rdate", Kind: common.KString, NonEmpty: true, MaxLen: maxRule},
				{Name: "exrule", Kind: common.KString, NonEmpty: true, MaxLen: maxRule},
				{Name: "exdate", Kind: common.KString, NonEmpty: true, MaxLen: maxRule},
				{Name: "stored_rules", Kind: common.KInt, Min: new(int64)},
			},
			AtLeastOne: []string{"rrule", "rdate", "exrule", "exdate", "stored_rules"},
		}},
		{Name: "exception_of", Kind: common.KObject, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "event_id", Kind: common.KIDString},
				{Name: "sync_id", Kind: common.KIDString},
			},
			AtLeastOne: []string{"event_id", "sync_id"},
		}},
		{Name: "calendar", Kind: common.KObject, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "id", Kind: common.KIDString},
				{Name: "title", Kind: common.KString, NonEmpty: true},
				{Name: "owner", Kind: common.KString, NonEmpty: true},
				{Name: "account", Kind: common.KObject, Obj: &common.Schema{
					Fields: []common.Field{
						{Name: "type", Kind: common.KString, NonEmpty: true},
						{Name: "name", Kind: common.KString, NonEmpty: true},
					},
					AtLeastOne: []string{"type", "name"},
				}},
			},
			AtLeastOne: []string{"id", "title", "owner", "account"},
		}},
		{Name: "uid", Kind: common.KString, NonEmpty: true, MaxLen: maxIdentifier},
		{Name: "sync_id", Kind: common.KString, NonEmpty: true, MaxLen: maxIdentifier},
		{Name: "source_id", Kind: common.KIDString},
		{Name: "body_flags", Kind: common.KStringArray},
		{Name: "body_truncated", Kind: common.KBool},
		{Name: "body_total_bytes", Kind: common.KInt, Min: new(int64)},
		{Name: "body_raw_b64", Kind: common.KString, NonEmpty: true},
		{Name: "body_raw_sha256", Kind: common.KHex64},
		{Name: "body_raw_len", Kind: common.KInt, Min: new(int64)},
		{Name: "truncated_fields", Kind: common.KStringArray},
	}, common.CommonFields()...),
	Cross: crossRules,
}

// crossRules are the rules that span fields of one payload. They run after the
// field checks, so every present field already has its kind.
func crossRules(m map[string]any) error {
	start, hasStart := m["start_date"].(string)
	end, hasEnd := m["end_date"].(string)
	if (hasStart || hasEnd) && m["timing"] != TimingAllDay {
		return errors.New("start_date and end_date require timing all_day")
	}
	// a date of an event has a year (--MM-DD is a birthday form, never an event's)
	if hasStart && len(start) != len("2006-01-02") {
		return errors.New("start_date must be a full YYYY-MM-DD date")
	}
	if hasEnd && len(end) != len("2006-01-02") {
		return errors.New("end_date must be a full YYYY-MM-DD date")
	}
	if hasStart && hasEnd && end < start { // full dates of one form compare as strings
		return errors.New("end_date is before start_date")
	}
	for _, pair := range [][2]string{
		{"attendees_truncated", "attendees_total"},
		{"reminders_truncated", "reminders_total"},
		{"body_truncated", "body_total_bytes"},
	} {
		if t, ok := m[pair[0]].(bool); ok && t {
			if _, has := m[pair[1]]; !has {
				return errors.New(pair[0] + " requires " + pair[1])
			}
		}
	}
	if flags, ok := m["body_flags"].([]any); ok {
		for _, f := range flags {
			switch f {
			case common.FlagInvalidUTF8, common.FlagNUL, common.FlagTruncated:
			default:
				return errors.New("body_flags: an entry is not one of invalid_utf8,nul,truncated")
			}
		}
	}
	return nil
}

// coordinatesInRange bounds the coordinates of a location: latitude to -90..90
// and longitude to -180..180 degrees.
func coordinatesInRange(m map[string]any) error {
	for _, c := range []struct {
		name  string
		limit float64
	}{{"latitude", 90}, {"longitude", 180}} {
		v, has := m[c.name]
		if !has {
			continue
		}
		x, ok := number(v)
		if !ok || x < -c.limit || x > c.limit {
			return fmt.Errorf("%s is outside -%g..%g", c.name, c.limit, c.limit)
		}
	}
	return nil
}

// number reads a finite number of a kind the field check accepted.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

// Validate checks a calendar_event payload against the v1 contract. Its errors
// name field paths and never a payload value; unknown fields are allowed.
func Validate(payload map[string]any) error { return schema.Validate(payload) }

func init() { records.SetValidator(Type, Validate) }

// Decode reads a stored calendar_event payload of the given payload version. Only
// version 1 exists; any other value is ErrUnsupportedPayloadVersion. Numbers are
// read exactly (json.Number): integers are canonical JSON integers, coordinates
// become float64, and the numbers inside the free-form objects stay json.Number,
// so the result equals what Payload produced. An identifier stored as an integer
// (source_id, exception_of, calendar.id) is read as its decimal text. Unknown
// fields are ignored. A payload that does not satisfy the v1 contract is an error
// naming the path, never a value.
func Decode(payloadV int, payload []byte) (Event, error) {
	if payloadV != PayloadVersion {
		return Event{}, fmt.Errorf("%w: %d", ErrUnsupportedPayloadVersion, payloadV)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return Event{}, errors.New("calendar: the payload is not a JSON object")
	}
	if len(bytes.TrimSpace(payload[dec.InputOffset():])) != 0 {
		return Event{}, errors.New("calendar: the payload holds more than one JSON value")
	}
	if m == nil {
		return Event{}, errors.New("calendar: the payload is not a JSON object")
	}
	if err := Validate(m); err != nil {
		return Event{}, fmt.Errorf("calendar: %w", err)
	}
	return fromMap(m), nil
}

// fromMap extracts a validated payload. Every access is by comma-ok type
// assertion, so a value of an unexpected type is skipped rather than trusted.
func fromMap(m map[string]any) Event {
	var e Event
	e.Source = str(m, "source")
	e.Timing = str(m, "timing")
	e.Title = str(m, "title")
	e.Status = str(m, "status")
	e.StartDate = str(m, "start_date")
	e.EndDate = str(m, "end_date")
	e.StartTimezone = str(m, "start_timezone")
	e.EndTimezone = str(m, "end_timezone")
	e.Duration = str(m, "duration")
	e.URL = str(m, "url")
	if l, ok := m["location"].(map[string]any); ok {
		e.Location = &Location{Text: str(l, "text"), Address: str(l, "address"), Latitude: floatPtr(l, "latitude"), Longitude: floatPtr(l, "longitude")}
	}
	if o, ok := m["organizer"].(map[string]any); ok {
		e.Organizer = &Person{Address: str(o, "address"), Name: str(o, "name")}
	}
	for _, v := range list(m, "attendees") {
		if a, ok := v.(map[string]any); ok {
			e.Attendees = append(e.Attendees, Attendee{
				Address: str(a, "address"), Name: str(a, "name"), Role: str(a, "role"), Type: str(a, "type"), Status: str(a, "status"),
			})
		}
	}
	e.AttendeesTotal = intPtr(m, "attendees_total")
	e.AttendeesTruncated, _ = m["attendees_truncated"].(bool)
	e.SelfStatus = str(m, "self_status")
	e.Availability = str(m, "availability")
	e.Privacy = str(m, "privacy")
	for _, v := range list(m, "reminders") {
		if r, ok := v.(map[string]any); ok {
			e.Reminders = append(e.Reminders, Reminder{BeforeS: intPtr(r, "before_s"), Method: str(r, "method")})
		}
	}
	e.RemindersTotal = intPtr(m, "reminders_total")
	e.RemindersTruncated, _ = m["reminders_truncated"].(bool)
	if r, ok := m["recurrence"].(map[string]any); ok {
		e.Recurrence = &Recurrence{
			RRule: str(r, "rrule"), RDate: str(r, "rdate"), ExRule: str(r, "exrule"), ExDate: str(r, "exdate"),
			StoredRules: intPtr(r, "stored_rules"),
		}
	}
	if x, ok := m["exception_of"].(map[string]any); ok {
		e.ExceptionOf = &ExceptionOf{EventID: idString(x["event_id"]), SyncID: idString(x["sync_id"])}
	}
	if c, ok := m["calendar"].(map[string]any); ok {
		e.Calendar = &CalendarRef{ID: idString(c["id"]), Title: str(c, "title"), Owner: str(c, "owner")}
		if a, ok := c["account"].(map[string]any); ok {
			e.Calendar.Account = &Account{Type: str(a, "type"), Name: str(a, "name")}
		}
	}
	e.UID = str(m, "uid")
	e.SyncID = str(m, "sync_id")
	e.SourceID = idString(m["source_id"])
	e.BodyFlags = strs(m["body_flags"])
	if b, ok := m["body_truncated"].(bool); ok {
		e.BodyTruncated = &b
	}
	e.BodyTotalBytes = intPtr(m, "body_total_bytes")
	e.BodyRawB64 = str(m, "body_raw_b64")
	e.BodyRawSHA256 = str(m, "body_raw_sha256")
	e.BodyRawLen = intPtr(m, "body_raw_len")
	e.TruncatedFields = strs(m["truncated_fields"])
	e.Deleted = normMap(m, "deleted")
	e.Recovery = normMap(m, "recovery")
	e.Snapshot = normMap(m, "snapshot")
	e.Raw = normMap(m, "raw")
	return e
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func list(m map[string]any, key string) []any {
	l, _ := m[key].([]any)
	return l
}

func strs(v any) []string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// idString renders an identifier that may be a string or an integer (KIDString).
func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

func intPtr(m map[string]any, key string) *int64 {
	if n, ok := m[key].(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return &i
		}
	}
	return nil
}

func floatPtr(m map[string]any, key string) *float64 {
	if n, ok := m[key].(json.Number); ok {
		if f, err := n.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return &f
		}
	}
	return nil
}

func normMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return common.NormMap(v)
	}
	return nil
}

// Live reports whether the event is a plain live one: it carries no recovery,
// snapshot or deleted provenance (see common.IsLive). A last-synced copy, a
// soft-deleted event and an event of a deleted calendar are therefore never live.
func (e Event) Live() bool { return common.IsLive(e.Recovery, e.Snapshot, e.Deleted) }

// DateOfMidnight returns the date "YYYY-MM-DD" of t read in UTC when t is exactly
// a UTC midnight (00:00:00.000000000), and false otherwise. Only that reading is
// a stored all-day date: a value off midnight is not repaired, and no zone is
// tried (a local midnight in another zone is not a UTC midnight).
func DateOfMidnight(t time.Time) (date string, ok bool) {
	u := t.UTC()
	if u.Hour() != 0 || u.Minute() != 0 || u.Second() != 0 || u.Nanosecond() != 0 {
		return "", false
	}
	return u.Format("2006-01-02"), true
}

// When renders a record time for the summary by the event's timing: the date of
// an all-day event, the UTC instant of a timed one ("Z"), the wall-clock reading
// of a floating one (marked "(floating)"), and "" for no time or another timing.
func When(t *records.Time, timing string) string {
	if t == nil {
		return ""
	}
	u := t.T.UTC()
	switch timing {
	case TimingAllDay:
		return u.Format("2006-01-02")
	case TimingTimed:
		return u.Format("2006-01-02T15:04:05Z")
	case TimingFloating:
		return u.Format("2006-01-02T15:04:05") + " (floating)"
	}
	return ""
}

// Summary builds the record summary "calendar[ series| exception] <when>
// <title>[ @ <location text>][ [cancelled]]", one line, through common.Summarize
// (format and bidi characters are shown as <U+XXXX>). series marks an event with
// a recurrence, exception one that replaces an occurrence of a series; <when> is
// When(t, e.Timing) and is left out when empty. The title is bounded to 120 bytes
// and the location text to 64.
func Summary(e Event, t *records.Time) string {
	parts := []string{"calendar"}
	switch {
	case e.Recurrence != nil:
		parts = append(parts, "series")
	case e.ExceptionOf != nil:
		parts = append(parts, "exception")
	}
	if w := When(t, e.Timing); w != "" {
		parts = append(parts, w)
	}
	if s := common.Summarize(e.Title, 120); s != "" {
		parts = append(parts, s)
	}
	if e.Location != nil {
		if s := common.Summarize(e.Location.Text, 64); s != "" {
			parts = append(parts, "@", s)
		}
	}
	if e.Status == "cancelled" {
		parts = append(parts, "[cancelled]")
	}
	return strings.Join(parts, " ")
}
