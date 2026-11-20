package androidcalendar_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/parsers/androidcalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// The hostile rows of spec 9 that earlier tasks do not cover. Each test builds its input here (or reads a committed
// fixture), runs the parser through the harness and requires: no panic, no invented value, and a stated warning or
// raw marker. A bound is a typed error or a counted warning, never a silent cut.

// hostileOK says err is a documented answer to hostile input: a typed decoder error or one of the parser's own.
func hostileOK(err error) bool {
	if err == nil || parsertest.HostileOK(err) {
		return true
	}
	for _, typed := range []error{androidcalendar.ErrNoEventsTable, androidcalendar.ErrSchemaIncomplete, androidcalendar.ErrUnsupportedVersion} {
		if errors.Is(err, typed) {
			return true
		}
	}
	return false
}

// Spec 9 (damage): a database cut inside Events states the pages it lost; a cut never gives an empty result that
// claims to be whole, and no record names a row the database did not hold.
func TestAndroidCalendarTruncatedDatabaseIsStated(t *testing.T) {
	const rows = 300
	var evs []row
	for i := int64(1); i <= rows; i++ {
		evs = append(evs, row{i, map[string]any{"title": fmt.Sprintf("E%d", i), "description": strings.Repeat("d", 200)}})
	}
	full, tables := buildDB(t, dbSpec{events: evs, calendars: []row{{1, nil}}})
	const ps = 4096
	pages := len(full) / ps
	root := int(tables["Events"].Root())
	var stated int
	for cut := pages - 1; cut > root; cut-- {
		res := parseAt(t, remoteProvider, map[string][]byte{"db": slices.Clone(full[:cut*ps])})
		if !hostileOK(res.Err) {
			t.Fatalf("cut at page %d: untyped error %v", cut, res.Err)
		}
		if res.Refused != 0 || len(res.Violations) != 0 {
			t.Fatalf("cut at page %d: refused %d, violations %v", cut, res.Refused, res.Violations)
		}
		for _, r := range res.Records {
			id, err := strconv.ParseInt(strings.TrimPrefix(fmt.Sprint(r.Payload["title"]), "E"), 10, 64)
			if err != nil || id < 1 || id > rows || r.Payload["source_id"] != strconv.FormatInt(id, 10) {
				t.Fatalf("cut at page %d: record %s (title %v) was not in the database", cut, r.Locator, r.Payload["title"])
			}
		}
		if res.Err == nil && len(res.Records) < rows {
			if len(res.Warnings) == 0 {
				t.Errorf("cut at page %d: %d of %d records and no warning: a cut result is stated, never complete", cut, len(res.Records), rows)
			}
			stated++
		}
	}
	if stated == 0 {
		t.Error("no cut lost Events rows and parsed: the check proves nothing")
	}
}

// A hot journal is rolled back (calendar-600): row 6 keeps its committed title, "Holiday" (the oracle's, from the
// engine's view of the same files), and the interrupted rewrite is never a record. The reader's journal-hot warning is
// a warning of the oracle, held by TestAndroidCalendarMatchesOracle.
func TestAndroidCalendarHotJournalIsRolledBack(t *testing.T) {
	p := parseVariant(t, "calendar-600")
	if title := p.rec(t, 6).Payload["title"]; title != "Holiday" {
		t.Errorf("row 6: title %.40q, want its committed title \"Holiday\"", title)
	}
	for _, rec := range p.res.Records {
		if title, _ := rec.Payload["title"].(string); strings.HasPrefix(title, "Interrupted") {
			t.Errorf("%s: the rolled-back rewrite is a record", rec.Locator)
		}
	}
}

// calendar-601: row 42, committed only in the WAL, is a record whose place names the WAL artifact; row 43, in a frame
// that was never committed, is no live record.
func TestAndroidCalendarWALRowsPointIntoTheWAL(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "calendar-601").Bundle(h, remoteProvider)
	p := androidcalendar.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	recs := byRowid(t, res).recs
	r, ok := recs[42]
	if !ok {
		t.Fatal("no record for row 42")
	}
	if wal, ok := b.ByRole["wal"]; !ok || r.ArtifactID != wal.ID || !strings.Contains(r.Locator, "wal_frame=") {
		t.Errorf("row 42: artifact %q, locator %q; want the WAL artifact", r.ArtifactID, r.Locator)
	}
	if _, live := recs[43]; live {
		t.Error("row 43 (an uncommitted WAL frame) is a live record")
	}
}

// Spec 9 and the 4I addendum 6.2: a Calendars table whose pages are damaged is the note Calendars = "unreadable" and
// every calendar join is undecided (raw.join_undecided), never "no-calendars-row".
func TestAndroidCalendarUnreadableJoinTableIsNotEmpty(t *testing.T) {
	var cals []row
	for i := int64(1); i <= 200; i++ {
		cals = append(cals, row{i, map[string]any{"calendar_displayName": strings.Repeat("c", 100)}})
	}
	db, tables := buildDB(t, dbSpec{events: []row{{1, nil}, {2, map[string]any{"calendar_id": int64(150)}}}, calendars: cals})
	const ps = 4096
	root := int(tables["Calendars"].Root())
	if db[(root-1)*ps] != 0x05 {
		t.Fatalf("the Calendars root (page %d) is not an interior page (%#x): the scenario needs leaves to damage", root, db[(root-1)*ps])
	}
	pg := db[(root-1)*ps : root*ps]
	pg[0] = 0x00 // not a b-tree page: the table cannot be read
	res := parseAt(t, remoteProvider, map[string][]byte{"db": db})
	if res.Err != nil || len(res.Violations) != 0 {
		t.Fatalf("Parse: err %v, violations %v", res.Err, res.Violations)
	}
	if res.Notes["Calendars"] != "unreadable" {
		t.Errorf("note Calendars = %q, want unreadable (notes %v)", res.Notes["Calendars"], res.Notes)
	}
	p := byRowid(t, res)
	for id := int64(1); id <= 2; id++ {
		r := p.rec(t, id)
		raw := rawOf(r)
		if l, _ := raw["join_undecided"].([]any); !slices.Contains(l, any("Calendars")) {
			t.Errorf("row %d: raw.join_undecided %v, want Calendars", id, raw["join_undecided"])
		}
		if raw["calendar_state"] == "no-calendars-row" || r.Payload["calendar"] != nil || r.Deleted {
			t.Errorf("row %d: calendar_state %v, calendar %v, Deleted %v: an unreadable table is never empty", id, raw["calendar_state"], r.Payload["calendar"], r.Deleted)
		}
	}
	if len(res.Warnings) == 0 {
		t.Error("no warning states the damage")
	}
}

// Spec 9 (free text and identifiers): NUL and invalid UTF-8 are cleaned and flagged, never dropped; RTL text and a
// bidi override are kept as stored, and the summary shows the override (common.Summarize); a 1 MiB
// rrule is never cut (absent, too-long, length and SHA-256); a 1 MiB duration is never read (its state, length and
// SHA-256; raw.duration_state takes "unparsed" for an over-long text, the coordinator's ruling in duration.go); a 1 MiB
// location is cut at 4 KiB and named in truncated_fields.
func TestAndroidCalendarHostileTexts(t *testing.T) {
	const rtl = "\xe2\x80\xaeRTL \xd7\xa9\xd7\x9c\xd7\x95\xd7\x9d" // U+202E (right-to-left override), then Hebrew
	big := strings.Repeat("R", 1<<20)
	db, _ := buildDB(t, dbSpec{events: []row{
		{1, map[string]any{"title": "a\x00b"}},
		{2, map[string]any{"title": string([]byte{'x', 0xff, 0xfe, 'y'})}},
		{3, map[string]any{"title": rtl}},
		{4, map[string]any{"rrule": big}},
		{5, map[string]any{"dtend": nil, "duration": "P" + strings.Repeat("1", 1<<20) + "S"}},
		{6, map[string]any{"eventLocation": big}},
	}, calendars: []row{{1, nil}}})
	p := parseBuilt(t, db, false)
	if r := p.rec(t, 3); r.Payload["title"] != rtl || !strings.Contains(r.Summary, "<U+202E>") {
		t.Errorf("row 3: title %q, summary %q; want the RTL text kept as stored and the override shown in the summary", r.Payload["title"], r.Summary)
	}
	for id := int64(1); id <= 2; id++ {
		r := p.rec(t, id)
		title, _ := r.Payload["title"].(string)
		if title == "" || !utf8.ValidString(title) || strings.ContainsRune(title, 0) {
			t.Errorf("row %d: title %q is not a cleaned text", id, title)
		}
		if f := fmt.Sprint(rawOf(r)["text_flags"]); !strings.Contains(f, "title") {
			t.Errorf("row %d: raw.text_flags %v does not name the altered title", id, rawOf(r)["text_flags"])
		}
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	r4 := rawOf(p.rec(t, 4))
	if rec, _ := p.rec(t, 4).Payload["recurrence"].(map[string]any); rec["rrule"] != nil {
		t.Errorf("row 4: a 1 MiB rrule was kept or cut: %.40v", rec["rrule"])
	}
	if r4["rrule_state"] != "too-long" || r4["rrule_len"] != int64(1<<20) || r4["rrule_sha256"] != sum(big) {
		t.Errorf("row 4: rrule_state %v, len %v, sha %v", r4["rrule_state"], r4["rrule_len"], r4["rrule_sha256"])
	}
	dur := "P" + strings.Repeat("1", 1<<20) + "S"
	r5 := p.rec(t, 5)
	if raw := rawOf(r5); r5.Payload["duration"] != nil || r5.TimeEnd != nil || raw["duration_state"] != "unparsed" || raw["duration_len"] != int64(len(dur)) || raw["duration_sha256"] != sum(dur) {
		t.Errorf("row 5: duration %.20v, end %v, state %v, len %v", r5.Payload["duration"], r5.TimeEnd, raw["duration_state"], raw["duration_len"])
	}
	r6 := p.rec(t, 6)
	loc, _ := r6.Payload["location"].(map[string]any)
	if text, _ := loc["text"].(string); len(text) != 4<<10 || !strings.Contains(fmt.Sprint(r6.Payload["truncated_fields"]), "location") {
		t.Errorf("row 6: location of %d bytes, truncated_fields %v; want 4096 and location", len(text), r6.Payload["truncated_fields"])
	}
}

// Spec 2: a code the contract does not define (a future provider's) is "unknown" with the stored code in raw; never
// mapped to a neighbour, never dropped.
func TestAndroidCalendarFutureCodes(t *testing.T) {
	db, _ := buildDB(t, dbSpec{events: []row{
		{1, map[string]any{"eventStatus": int64(4242), "availability": int64(-1), "accessLevel": int64(1 << 40), "selfAttendeeStatus": int64(99)}},
	}, calendars: []row{{1, nil}}, children: map[string][]row{
		"Attendees": {{1, map[string]any{"event_id": int64(1), "attendeeName": "A", "attendeeRelationship": int64(4242), "attendeeType": int64(-3), "attendeeStatus": int64(77)}}},
		"Reminders": {{1, map[string]any{"event_id": int64(1), "minutes": int64(5), "method": int64(4242)}}},
	}})
	r := parseBuilt(t, db, false).rec(t, 1)
	raw := rawOf(r)
	for field, c := range map[string]struct {
		col    string
		stored int64
	}{"status": {"eventStatus", 4242}, "availability": {"availability", -1}, "privacy": {"accessLevel", 1 << 40}, "self_status": {"selfAttendeeStatus", 99}} {
		if r.Payload[field] != "unknown" || raw[c.col] != c.stored {
			t.Errorf("%s: %v, raw.%s %#v; want unknown and %d", field, r.Payload[field], c.col, raw[c.col], c.stored)
		}
	}
	att, _ := r.Payload["attendees"].([]any)
	a0, _ := att[0].(map[string]any)
	if a0["role"] != "unknown" || a0["type"] != "unknown" || a0["status"] != "unknown" {
		t.Errorf("attendee %v, want role, type and status unknown", a0)
	}
	ra, _ := raw["attendees"].([]any)
	if e, _ := ra[0].(map[string]any); e["attendeeRelationship"] != int64(4242) || e["attendeeType"] != int64(-3) || e["attendeeStatus"] != int64(77) {
		t.Errorf("raw.attendees[0] %v, want the stored codes", ra[0])
	}
	if rem, _ := r.Payload["reminders"].([]any); len(rem) != 1 || rem[0].(map[string]any)["method"] != "unknown" {
		t.Errorf("reminders %v, want the method unknown", r.Payload["reminders"])
	}
}
