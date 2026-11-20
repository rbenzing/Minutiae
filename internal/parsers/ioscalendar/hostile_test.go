package ioscalendar_test

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
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
	return errors.Is(err, ioscalendar.ErrNoCalendarItemTable) || errors.Is(err, ioscalendar.ErrSchemaIncomplete)
}

// Spec 9 (damage): a database cut inside CalendarItem states the pages it lost; a cut never gives an empty result that
// claims to be whole, and no record names a row the database did not hold.
func TestIOSCalendarTruncatedDatabaseIsStated(t *testing.T) {
	const rows = 300
	s := newBuilt()
	for i := int64(1); i <= rows; i++ {
		s.event(i, map[string]any{"summary": fmt.Sprintf("E%d", i), "description": strings.Repeat("d", 200)})
	}
	full := s.bytes()
	const ps = 4096
	pages := len(full) / ps
	root := int(s.tabs["CalendarItem"].Root())
	var stated int
	for cut := pages - 1; cut > root; cut-- {
		res := parseAt(t, map[string][]byte{"db": slices.Clone(full[:cut*ps])}, true)
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
		t.Error("no cut lost CalendarItem rows and parsed: the check proves nothing")
	}
}

// calendar-ios: row 27, committed only in the WAL, is a record whose place names the WAL artifact; row 28, in a frame
// that was never committed, is no live record (tools/fixtures/calendar_ios.py).
func TestIOSCalendarWALRowsPointIntoTheWAL(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "calendar-ios"), "calendar-ios")
	p := ioscalendar.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	recs := byRowid(t, res)
	r, ok := recs[27]
	if !ok {
		t.Fatal("no record for row 27")
	}
	if r.Payload["title"] != "Only in the WAL, committed" {
		t.Errorf("row 27: title %v", r.Payload["title"])
	}
	if wal, ok := b.ByRole["wal"]; !ok || r.ArtifactID != wal.ID || !strings.Contains(r.Locator, "wal_frame=") {
		t.Errorf("row 27: artifact %q, locator %q; want the WAL artifact", r.ArtifactID, r.Locator)
	}
	if _, live := recs[28]; live {
		t.Error("row 28 (an uncommitted WAL frame) is a live record")
	}
}

// Spec 9 and the 4I addendum 6.2: a Participant table whose pages are damaged is the note Participant = "unreadable",
// and every join through it (the organizer by ROWID, the attendees by item_id) is undecided (raw.join_undecided),
// never "no-Participant-row" and never "no attendees"; one counted warning states it and the job does not fail.
func TestIOSCalendarUnreadableJoinTableIsNotEmpty(t *testing.T) {
	s := newBuilt()
	for i := int64(1); i <= 200; i++ {
		s.add("Participant", i, map[string]any{"item_id": int64(1), "email": strings.Repeat("p", 100) + "@example.test"})
	}
	s.event(1, nil)
	s.event(2, map[string]any{"organizer_id": int64(150)})
	db := s.bytes()
	const ps = 4096
	root := int(s.tabs["Participant"].Root())
	if db[(root-1)*ps] != 0x05 {
		t.Fatalf("the Participant root (page %d) is not an interior page (%#x): the scenario needs leaves to damage", root, db[(root-1)*ps])
	}
	db[(root-1)*ps] = 0x00 // not a b-tree page: the table cannot be read
	res := parseAt(t, map[string][]byte{"db": db}, true)
	if res.Err != nil || len(res.Violations) != 0 {
		t.Fatalf("Parse: err %v, violations %v", res.Err, res.Violations)
	}
	if res.Notes["Participant"] != "unreadable" {
		t.Errorf("note Participant = %q, want unreadable (notes %v)", res.Notes["Participant"], res.Notes)
	}
	recs := byRowid(t, res)
	for id := int64(1); id <= 2; id++ {
		r, ok := recs[id]
		if !ok {
			t.Fatalf("no record for row %d", id)
		}
		raw := rawOf(r)
		if !sameJSON(t, raw["join_undecided"], []any{"Participant"}) {
			t.Errorf("row %d: raw.join_undecided %v, want [Participant]", id, raw["join_undecided"])
		}
		if raw["organizer_state"] != nil || raw["participants_state"] != nil || r.Payload["organizer"] != nil || r.Payload["attendees"] != nil || raw["attendees_unnamed"] != nil {
			t.Errorf("row %d: organizer_state %v, participants_state %v, organizer %v, attendees %v: an unreadable table is never empty",
				id, raw["organizer_state"], raw["participants_state"], r.Payload["organizer"], r.Payload["attendees"])
		}
	}
	var joinWarn []string
	for _, w := range res.Warnings {
		if strings.HasPrefix(w.Reason, "calendar-join-undecided") {
			joinWarn = append(joinWarn, w.Locator+" "+w.Reason)
		}
	}
	if !slices.Contains(joinWarn, "sqlite:table=Participant calendar-join-undecided: Participant: 2 records") {
		t.Errorf("join warnings %q: want the counted Participant warning", joinWarn)
	}
}

// Spec 9 (free text and identifiers): NUL and invalid UTF-8 are cleaned and flagged, never dropped, the stored bytes
// kept in raw.title_raw_b64; RTL text and a bidi override are kept as stored, and the summary shows the override
// (common.Summarize); a 1 MiB url is never cut (absent, url_state too-long, its length and SHA-256); a 1 MiB summary
// is cut at 4 KiB and named in truncated_fields.
func TestIOSCalendarHostileTexts(t *testing.T) {
	const rtl = "\xe2\x80\xaeRTL \xd7\xa9\xd7\x9c\xd7\x95\xd7\x9d" // U+202E (right-to-left override), then Hebrew
	big := strings.Repeat("R", 1<<20)
	s := newBuilt()
	s.event(1, map[string]any{"summary": "a\x00b"})
	s.event(2, map[string]any{"summary": string([]byte{'x', 0xff, 0xfe, 'y'})})
	s.event(3, map[string]any{"summary": rtl})
	s.event(4, map[string]any{"url": big})
	s.event(5, map[string]any{"summary": big})
	recs := byRowid(t, parseBuilt(t, s.bytes(), false))
	if r := recs[3]; r.Payload["title"] != rtl || !strings.Contains(r.Summary, "<U+202E>") || rawOf(r)["text_flags"] != nil {
		t.Errorf("row 3: title %q, summary %q, text_flags %v; want the RTL text kept as stored and the override shown in the summary", r.Payload["title"], r.Summary, rawOf(r)["text_flags"])
	}
	for _, c := range []struct {
		id         int64
		title, b64 string
	}{
		{1, "a�b", "YQBi"},
		{2, "x��y", "eP/+eQ=="},
	} {
		r := recs[c.id]
		raw := rawOf(r)
		if r.Payload["title"] != c.title || raw["text_flags"] != "title" || raw["title_raw_b64"] != c.b64 {
			t.Errorf("row %d: title %q, text_flags %v, title_raw_b64 %v; want %q, title, %q", c.id, r.Payload["title"], raw["text_flags"], raw["title_raw_b64"], c.title, c.b64)
		}
	}
	r4 := recs[4]
	raw4 := rawOf(r4)
	if _, has := r4.Payload["url"]; has || raw4["url_state"] != "too-long" || raw4["url_len"] != int64(1<<20) || raw4["url_sha256"] != sha(big) {
		t.Errorf("row 4: url %.40v, url_state %v, url_len %v, url_sha256 %v", r4.Payload["url"], raw4["url_state"], raw4["url_len"], raw4["url_sha256"])
	}
	r5 := recs[5]
	if title, _ := r5.Payload["title"].(string); title != big[:4<<10] || !sameJSON(t, r5.Payload["truncated_fields"], []any{"title"}) || rawOf(r5)["text_flags"] != "title" {
		t.Errorf("row 5: title of %d bytes, truncated_fields %v, text_flags %v; want 4096 R, [title], title", len(title), r5.Payload["truncated_fields"], rawOf(r5)["text_flags"])
	}
}

// Spec 9: a coordinate is stated only when finite and within +-90 / +-180; NaN, +Inf, -Inf and 1e308 are not stated,
// each kept in raw with <col>_state "out-of-range" (the infinities in their display form, a NaN as "NaN" and also
// listed in unknown_columns as type-mismatch, because the reader reads a stored NaN as NULL, 4I2-A6); the other
// coordinate of the same row is kept.
func TestIOSCalendarHostileCoordinates(t *testing.T) {
	s := newBuilt()
	s.add("Location", 1, map[string]any{"title": "N", "latitude": math.NaN(), "longitude": 13.4})
	s.add("Location", 2, map[string]any{"title": "P", "latitude": math.Inf(1), "longitude": 13.4})
	s.add("Location", 3, map[string]any{"title": "M", "latitude": 52.5, "longitude": math.Inf(-1)})
	s.add("Location", 4, map[string]any{"title": "B", "latitude": 1e308, "longitude": 13.4})
	for i := int64(1); i <= 4; i++ {
		s.event(i, map[string]any{"location_id": i})
	}
	recs := byRowid(t, parseBuilt(t, s.bytes(), false))
	for _, c := range []struct {
		id       int64
		col      string
		raw      any
		location map[string]any
		unknown  map[string]any
	}{
		{1, "latitude", "NaN", map[string]any{"text": "N", "longitude": 13.4}, map[string]any{"Location.latitude": "type-mismatch"}},
		{2, "latitude", "+Inf", map[string]any{"text": "P", "longitude": 13.4}, nil},
		{3, "longitude", "-Inf", map[string]any{"text": "M", "latitude": 52.5}, nil},
		{4, "latitude", 1e308, map[string]any{"text": "B", "longitude": 13.4}, nil},
	} {
		r := recs[c.id]
		raw := rawOf(r)
		if !sameJSON(t, r.Payload["location"], c.location) || !sameValue(raw[c.col], c.raw) || raw[c.col+"_state"] != "out-of-range" {
			t.Errorf("row %d: location %v, raw.%s %#v, %s_state %v; want %v, %v, out-of-range", c.id, r.Payload["location"], c.col, raw[c.col], c.col, raw[c.col+"_state"], c.location, c.raw)
		}
		if got := unknownOf(r); !sameJSON(t, got, c.unknown) {
			t.Errorf("row %d: unknown_columns %v, want %v", c.id, got, c.unknown)
		}
	}
}
