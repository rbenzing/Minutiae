package ioscalendar_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// snapshotTables are the tables of the from-memory snapshot tools/fixtures/schemas/calendar-ios/calendar.sql,
// restated for Go-built stores: the name and the column definitions after the leading "ROWID INTEGER PRIMARY KEY"
// (OccurrenceCache has no ROWID column).
func snapshotTables() [][2]string {
	return [][2]string{
		{"CalendarItem", "summary TEXT, location_id INTEGER, description TEXT, start_date REAL, start_tz TEXT, end_date REAL, end_tz TEXT, all_day INTEGER, calendar_id INTEGER, orig_item_id INTEGER, orig_date REAL, organizer_id INTEGER, status INTEGER, availability INTEGER, privacy_level INTEGER, url TEXT, creation_date REAL, last_modified REAL, sequence_num INTEGER, birthday_id INTEGER, external_id TEXT, unique_identifier TEXT, external_rep BLOB, hidden INTEGER, has_recurrences INTEGER, has_attendees INTEGER, invitation_status INTEGER, entity_type INTEGER"},
		{"Location", "title TEXT, address TEXT, latitude REAL, longitude REAL"},
		{"Participant", "item_id INTEGER, identity_id INTEGER, email TEXT, role INTEGER, type INTEGER, status INTEGER"},
		{"Identity", "display_name TEXT, address TEXT"},
		{"Calendar", "title TEXT, store_id INTEGER"},
		{"Store", "name TEXT, type INTEGER"},
		{"Recurrence", "owner_id INTEGER, frequency INTEGER, interval INTEGER, count INTEGER, end_date REAL, specifier TEXT"},
		{"ExceptionDate", "owner_id INTEGER, date REAL"},
		{"Alarm", "calendaritem_owner_id INTEGER, trigger_interval INTEGER, trigger_date REAL, type INTEGER"},
		{"OccurrenceCache", "day REAL, event_id INTEGER, occurrence_date REAL"},
		{"CalendarItemChanges", "record INTEGER, type INTEGER, calendar_id INTEGER"},
	}
}

// built is a Go-built Calendar.sqlitedb of the snapshot's tables (all empty until rows are added).
type built struct {
	b     *sqlitetest.Builder
	tabs  map[string]*sqlitetest.Table
	cols  map[string][]string
	alias map[string]bool
}

// newBuilt creates every table of the snapshot except those named in skip.
func newBuilt(skip ...string) *built {
	s := &built{b: sqlitetest.New(sqlitetest.Options{PageSize: 4096}), tabs: map[string]*sqlitetest.Table{}, cols: map[string][]string{}, alias: map[string]bool{}}
	for _, td := range snapshotTables() {
		if slices.Contains(skip, td[0]) {
			continue
		}
		s.table(td[0], td[1], td[0] != "OccurrenceCache")
	}
	return s
}

// table creates a table of the given column definitions, after a ROWID rowid alias when alias is set.
func (s *built) table(name, defs string, alias bool) {
	var cols []string
	for _, d := range strings.Split(defs, ", ") {
		cols = append(cols, strings.Fields(d)[0])
	}
	ddl := "CREATE TABLE " + name + " (" + defs + ")"
	if alias {
		ddl = "CREATE TABLE " + name + " (ROWID INTEGER PRIMARY KEY, " + defs + ")"
	}
	s.tabs[name], s.cols[name], s.alias[name] = s.b.CreateTable(name, ddl), cols, alias
}

// add inserts one row; a column missing from kv stores NULL.
func (s *built) add(table string, id int64, kv map[string]any) {
	var vals []any
	if s.alias[table] {
		vals = append(vals, nil) // the rowid alias stores NULL
	}
	for _, c := range s.cols[table] {
		vals = append(vals, kv[c])
	}
	s.tabs[table].Insert(id, vals...)
}

// event inserts a CalendarItem row: a timed event an hour from D in Europe/Berlin of calendar 1, overridden by kw (a
// nil value stores NULL).
func (s *built) event(id int64, kw map[string]any) {
	row := map[string]any{
		"summary": "Event " + strconv.FormatInt(id, 10), "start_date": float64(cD), "start_tz": "Europe/Berlin",
		"end_date": float64(cD + cH), "end_tz": "Europe/Berlin", "all_day": int64(0), "calendar_id": int64(1), "entity_type": int64(2),
	}
	for k, v := range kw {
		row[k] = v
	}
	s.add("CalendarItem", id, row)
}

func (s *built) bytes() []byte { return s.b.Bytes() }

// parseBuilt parses a Go-built store at the system path (with an empty -wal when deferred, so Probe's checks fall to
// Parse) and fails on a job error or a contract violation.
func parseBuilt(t *testing.T, db []byte, deferred bool) parsertest.Result {
	t.Helper()
	files := map[string][]byte{"db": db}
	if deferred {
		files["wal"] = []byte{}
	}
	res := parseAt(t, files, true)
	if res.Err != nil || len(res.Violations) != 0 {
		t.Fatalf("Go-built store: err %v, violations %v", res.Err, res.Violations)
	}
	return res
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// norm round-trips a value through the record's JSON form, so that literals compare with what a payload stores.
func norm(t *testing.T, v any) any {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	dec := json.NewDecoder(bytes.NewReader(bs))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameJSON compares two payload values by their JSON form.
func sameJSON(t *testing.T, got, want any) bool {
	t.Helper()
	return reflect.DeepEqual(norm(t, got), norm(t, want))
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

func objOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Spec 8.2, Q1, F27, F29: the attendees come from Participant by item_id (Match), the organizer from Participant by
// organizer_id and Identity by identity_id (Get); the address is Participant.email, else Identity.address, the name
// Identity.display_name; the codes are never labelled.
func TestIOSCalendarParticipants(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	r9 := recs[9]
	wantAtt := []any{
		map[string]any{"address": "c@example.test", "role": "unknown", "type": "unknown", "status": "unknown"},
		map[string]any{"address": "mailto:bob@example.test", "name": "Bob", "role": "unknown", "type": "unknown", "status": "unknown"},
	}
	if !sameJSON(t, r9.Payload["attendees"], wantAtt) {
		t.Errorf("row 9: attendees %v, want %v", r9.Payload["attendees"], wantAtt)
	}
	raw9 := rawOf(r9)
	if !sameJSON(t, raw9["attendees"], []any{map[string]any{"role": 1, "type": 1, "status": 2}, map[string]any{"role": 0, "type": 2, "status": 7}}) {
		t.Errorf("row 9: raw.attendees %v (the stored codes, aligned with attendees)", raw9["attendees"])
	}
	// the identity-99 participant has neither address nor name: not an attendee, counted, its dangling key kept
	if raw9["attendees_unnamed"] != int64(1) || !sameJSON(t, raw9["attendees_unnamed_entries"], []any{map[string]any{"identity_state": "no-Identity-row"}}) {
		t.Errorf("row 9: attendees_unnamed %#v, entries %v", raw9["attendees_unnamed"], raw9["attendees_unnamed_entries"])
	}
	// F29: Participant 4 is row 1's organizer (Identity 1) and, through Match(Participant.item_id), its one attendee
	r1 := recs[1]
	if !sameJSON(t, r1.Payload["organizer"], map[string]any{"address": "mailto:ann@example.test", "name": "Ann"}) {
		t.Errorf("row 1: organizer %v", r1.Payload["organizer"])
	}
	if !sameJSON(t, r1.Payload["attendees"], []any{map[string]any{"address": "mailto:ann@example.test", "name": "Ann", "role": "unknown", "type": "unknown", "status": "unknown"}}) ||
		!sameJSON(t, rawOf(r1)["attendees"], []any{map[string]any{"role": 2, "type": 1, "status": 1}}) {
		t.Errorf("row 1: attendees %v, raw %v", r1.Payload["attendees"], rawOf(r1)["attendees"])
	}

	// Go-built: the email wins over Identity.address; an over-long email is no address (F27); a wrongly typed email
	// is undecided, never "no address"; a dangling organizer key and a dangling organizer identity are stated
	longMail := strings.Repeat("m", 1100) + "@example.test"
	s := newBuilt()
	s.add("Identity", 1, map[string]any{"display_name": "Ann", "address": "mailto:ann@example.test"})
	s.event(1, map[string]any{"organizer_id": int64(77)})
	s.add("Participant", 1, map[string]any{"item_id": int64(1), "identity_id": int64(1), "email": "ann@work.example.test"})
	s.add("Participant", 2, map[string]any{"item_id": int64(1), "email": longMail, "role": int64(3)})
	s.add("Participant", 3, map[string]any{"item_id": int64(1), "email": int64(42)})
	s.event(2, map[string]any{"organizer_id": int64(4)})
	s.add("Participant", 4, map[string]any{"item_id": int64(9), "identity_id": int64(55), "email": "org@example.test"})
	s.event(3, map[string]any{"organizer_id": int64(5)})
	s.add("Participant", 5, map[string]any{"item_id": int64(9), "identity_id": int64(56)})
	got := byRowid(t, parseBuilt(t, s.bytes(), false))
	if !sameJSON(t, got[1].Payload["attendees"], []any{map[string]any{"address": "ann@work.example.test", "name": "Ann"}}) {
		t.Errorf("email over Identity.address: attendees %v", got[1].Payload["attendees"])
	}
	raw1 := rawOf(got[1])
	wantUnnamed := []any{
		map[string]any{"role": 3, "address_state": "too-long", "address_len": len(longMail), "address_sha256": sha(longMail)},
		map[string]any{"email": 42, "address_state": "undecided"},
	}
	if raw1["attendees_unnamed"] != int64(1) || !sameJSON(t, raw1["attendees_unnamed_entries"], wantUnnamed) {
		t.Errorf("over-long and undecided emails: attendees_unnamed %#v, entries %v; want 1 (the undecided one is not counted) and %v", raw1["attendees_unnamed"], raw1["attendees_unnamed_entries"], wantUnnamed)
	}
	if unknownOf(got[1])["Participant.email"] != "type-mismatch" || raw1["organizer_state"] != "no-Participant-row" || got[1].Payload["organizer"] != nil {
		t.Errorf("row 1: unknown_columns %v, organizer_state %#v, organizer %v", unknownOf(got[1]), raw1["organizer_state"], got[1].Payload["organizer"])
	}
	if !sameJSON(t, got[2].Payload["organizer"], map[string]any{"address": "org@example.test"}) || rawOf(got[2])["organizer_state"] != "no-Identity-row" {
		t.Errorf("organizer with a dangling identity: organizer %v, organizer_state %#v", got[2].Payload["organizer"], rawOf(got[2])["organizer_state"])
	}
	if got[3].Payload["organizer"] != nil || rawOf(got[3])["organizer_state"] != "no-Identity-row" {
		t.Errorf("organizer with neither: organizer %v, organizer_state %#v", got[3].Payload["organizer"], rawOf(got[3])["organizer_state"])
	}
}

// Spec 8.2, 9: location from Location by location_id (title is the text, coordinates only when finite and in range),
// calendar from Calendar by calendar_id and its store from Store by store_id (Store.name the account name, Store.type
// raw); a key naming no row is stated, never "no location" or "no calendar"; a key that cannot be read is undecided.
func TestIOSCalendarLocationAndCalendar(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	r1 := recs[1]
	if !sameJSON(t, r1.Payload["location"], map[string]any{"text": "Office", "address": "1 Main St", "latitude": 52.52, "longitude": 13.405}) {
		t.Errorf("row 1: location %v", r1.Payload["location"])
	}
	if !sameJSON(t, r1.Payload["calendar"], map[string]any{"id": "1", "title": "Home", "account": map[string]any{"name": "iCloud"}}) || rawOf(r1)["store_type"] != int64(2) {
		t.Errorf("row 1: calendar %v, raw.store_type %#v", r1.Payload["calendar"], rawOf(r1)["store_type"])
	}
	r16 := recs[16]
	if !sameJSON(t, r16.Payload["location"], map[string]any{"text": "Nowhere", "longitude": 13.4}) || !sameValue(rawOf(r16)["latitude"], float64(200)) || rawOf(r16)["latitude_state"] != "out-of-range" {
		t.Errorf("row 16: location %v, raw %v", r16.Payload["location"], rawOf(r16))
	}
	r17 := recs[17]
	if r17.Payload["location"] != nil || r17.Payload["calendar"] != nil || rawOf(r17)["location_state"] != "no-Location-row" || rawOf(r17)["calendar_state"] != "no-Calendar-row" {
		t.Errorf("row 17: location %v, calendar %v, raw %v", r17.Payload["location"], r17.Payload["calendar"], rawOf(r17))
	}

	s := newBuilt()
	s.add("Calendar", 1, map[string]any{"title": "Work", "store_id": int64(9)})
	s.event(1, nil)
	s.event(2, map[string]any{"location_id": "1", "calendar_id": nil})
	got := byRowid(t, parseBuilt(t, s.bytes(), false))
	if !sameJSON(t, got[1].Payload["calendar"], map[string]any{"id": "1", "title": "Work"}) || rawOf(got[1])["store_state"] != "no-Store-row" {
		t.Errorf("a calendar of a missing store: calendar %v, raw %v", got[1].Payload["calendar"], rawOf(got[1]))
	}
	raw2 := rawOf(got[2])
	if got[2].Payload["location"] != nil || unknownOf(got[2])["CalendarItem.location_id"] != "type-mismatch" || !sameJSON(t, raw2["join_undecided"], []any{"Location"}) || raw2["location_state"] != nil {
		t.Errorf("a TEXT location_id: location %v, raw %v", got[2].Payload["location"], raw2)
	}
	if got[2].Payload["calendar"] != nil || raw2["calendar_state"] != nil {
		t.Errorf("a NULL calendar_id joins nothing and states nothing: calendar %v, calendar_state %#v", got[2].Payload["calendar"], raw2["calendar_state"])
	}
}

// Spec 6.2: orig_item_id links an exception to its series (0 or NULL is none); orig_date is raw only, its basis
// unestablished, so no original_start.
func TestIOSCalendarExceptionLinksItsSeries(t *testing.T) {
	r7 := byRowid(t, parseVariant(t, "calendar-ios"))[7]
	raw := rawOf(r7)
	if !sameJSON(t, r7.Payload["exception_of"], map[string]any{"event_id": "8"}) || !sameValue(raw["orig_date"], float64(722297600)) || raw["orig_date_basis"] != "unestablished" {
		t.Errorf("row 7: exception_of %v, raw %v", r7.Payload["exception_of"], raw)
	}
	for _, nt := range r7.Times {
		if nt.Kind == "original_start" {
			t.Errorf("row 7: Times original_start %v: the basis of orig_date is not established", nt.Time)
		}
	}
	if !strings.HasPrefix(r7.Summary, "calendar exception ") {
		t.Errorf("row 7: summary %q", r7.Summary)
	}
	s := newBuilt()
	s.event(1, map[string]any{"orig_item_id": int64(0)})
	s.event(2, map[string]any{"orig_item_id": nil})
	got := byRowid(t, parseBuilt(t, s.bytes(), false))
	for id, r := range got {
		if r.Payload["exception_of"] != nil {
			t.Errorf("row %d: orig_item_id 0 or NULL is none, got exception_of %v", id, r.Payload["exception_of"])
		}
	}
}

// Spec 6.1, 8.2: one record per event row of CalendarItem (the engine's live view without entity_type 3, which the
// oracle counts), a series is one record with recurrence.stored_rules and its rules in raw; the occurrence cache is
// never a record source, only a note.
func TestCalendarRecurrenceIsNeverExpanded(t *testing.T) {
	for _, v := range recordedVariants() {
		fx := parsertest.LoadFixture(t, "testdata", v)
		res := parseVariant(t, v)
		if len(res.Records) != len(fx.Expect.Records) {
			t.Errorf("%s: %d records, the engine's live view holds %d event rows", v, len(res.Records), len(fx.Expect.Records))
		}
		for _, r := range res.Records {
			if !strings.HasPrefix(r.Locator, "sqlite:table=CalendarItem;") {
				t.Errorf("%s: a record from %s", v, r.Locator)
			}
		}
	}
	res := parseVariant(t, "calendar-ios")
	n8 := 0
	for _, r := range res.Records {
		if payloadStr(r, "source_id") == "8" {
			n8++
		}
	}
	r8 := byRowid(t, res)[8]
	if n8 != 1 || !sameJSON(t, r8.Payload["recurrence"], map[string]any{"stored_rules": 2}) || len(listOf(rawOf(r8)["recurrence"])) != 2 || !strings.HasPrefix(r8.Summary, "calendar series ") {
		t.Errorf("row 8: %d records, recurrence %v, raw.recurrence %v, summary %q", n8, r8.Payload["recurrence"], rawOf(r8)["recurrence"], r8.Summary)
	}
	if res.Notes["occurrence_cache_rows"] != "3" {
		t.Errorf("note occurrence_cache_rows %q, want 3", res.Notes["occurrence_cache_rows"])
	}
}

// Plan 4I3 Task 9 (guard.go), spec 9: a join table that is absent or whose key column or alias the mapping cannot
// trust makes every join through it undecided: a note per table, raw.join_undecided on each record that needed it,
// one counted warning per table after the run; never "no row" and never a job failure.
func TestIOSCalendarUndecidedJoinsAreStated(t *testing.T) {
	s := newBuilt("Participant", "Calendar")
	s.table("Calendar", "ROWID INTEGER, title TEXT, store_id INTEGER", false) // a ROWID that is not the rowid alias
	s.add("Calendar", 1, map[string]any{"ROWID": int64(1), "title": "Shadow"})
	s.event(1, nil)
	s.event(2, nil)
	res := parseBuilt(t, s.bytes(), true) // deferred: Parse checks what Probe would have refused
	recs := byRowid(t, res)
	for id, r := range recs {
		if r.Payload["calendar"] != nil || r.Payload["attendees"] != nil || !sameJSON(t, rawOf(r)["join_undecided"], []any{"Calendar", "Participant"}) {
			t.Errorf("row %d: calendar %v, attendees %v, join_undecided %v", id, r.Payload["calendar"], r.Payload["attendees"], rawOf(r)["join_undecided"])
		}
	}
	if res.Notes["join_undecided.Participant"] != "table absent" || !strings.Contains(res.Notes["join_undecided.Calendar"], "rowid alias") {
		t.Errorf("notes %v", res.Notes)
	}
	var joinWarn []string
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Reason, "calendar-join-undecided") {
			joinWarn = append(joinWarn, w.Locator+" "+w.Reason)
		}
	}
	want := []string{
		"sqlite:schema calendar-join-undecided: table Calendar: column ROWID is not the rowid alias; every join through it is undecided",
		"sqlite:table=Calendar calendar-join-undecided: Calendar: 2 records",
		"sqlite:table=Participant calendar-join-undecided: Participant: 2 records",
	}
	if !reflect.DeepEqual(joinWarn, want) {
		t.Errorf("join warnings %q, want %q", joinWarn, want)
	}
}
