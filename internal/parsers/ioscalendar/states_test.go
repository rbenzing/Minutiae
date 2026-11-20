package ioscalendar_test

import (
	"bytes"
	"encoding/base64"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalendar"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// absent marks a key that must not be present.
type absent struct{}

// The plan's state vocabulary (ios.calendar) and spec B10: one Go-built row per value, each asserted with a
// hard-coded expected value. Task 10 extends this function (F02).
func TestIOSCalendarStateVocabularyIsPinned(t *testing.T) {
	longZone := "Z/" + strings.Repeat("z", 300)
	longEnd := "E/" + strings.Repeat("e", 300)
	longMail := strings.Repeat("m", 1100) + "@example.test"
	longURL := "https://example.test/" + strings.Repeat("u", 1100)
	const d = float64(cD)
	longTitle := strings.Repeat("T", 5000) + "\x00"
	longStore := strings.Repeat("S", 1100)
	longAddress := strings.Repeat("A", 5000) + "\x00"
	bigText := strings.Repeat("h", 17<<10)
	s := newBuilt("ExceptionDate")
	// a DEFAULT that is no literal value: a short record (no stored date) reads as default-undecided, an unknown cell
	s.table("ExceptionDate", "owner_id INTEGER, date REAL DEFAULT (julianday('now'))", true)
	s.add("Store", 1, map[string]any{"name": "iCloud", "type": int64(2)})
	s.add("Calendar", 1, map[string]any{"title": "Home", "store_id": int64(1)})
	s.add("Calendar", 2, map[string]any{"title": "Orphan", "store_id": int64(99)})
	s.add("Location", 1, map[string]any{"title": "Far", "latitude": float64(95), "longitude": float64(-181)})
	s.add("Identity", 3, map[string]any{"display_name": []byte("N")})
	s.add("Participant", 30, map[string]any{"item_id": int64(999), "identity_id": int64(77)})
	s.add("Participant", 40, map[string]any{"item_id": int64(22), "email": "x@example.test", "role": int64(1), "type": int64(2), "status": int64(3)})
	s.add("Participant", 41, map[string]any{"item_id": int64(22), "identity_id": int64(77)})
	s.add("Participant", 42, map[string]any{"item_id": int64(22), "email": longMail})
	s.add("Participant", 43, map[string]any{"item_id": int64(22), "email": int64(7)})
	s.add("Participant", 44, map[string]any{"item_id": int64(22), "identity_id": int64(3)})
	s.add("Recurrence", 1, map[string]any{"owner_id": int64(23), "frequency": int64(1), "interval": int64(2), "count": int64(3), "end_date": d, "specifier": "BYDAY=TU"})
	s.add("ExceptionDate", 1, map[string]any{"owner_id": int64(24), "date": d})
	s.add("Alarm", 1, map[string]any{"calendaritem_owner_id": int64(25), "trigger_interval": int64(-900), "type": int64(0)})
	// Task 9 fix round 1 (I1, I2, I3, m1-m3)
	s.add("Calendar", 3, map[string]any{"title": longTitle})
	s.add("Store", 3, map[string]any{"name": longStore})
	s.add("Calendar", 4, map[string]any{"title": "Long account", "store_id": int64(3)})
	s.add("Location", 2, map[string]any{"title": "Nan", "latitude": math.NaN(), "longitude": math.Inf(1)})
	s.add("Location", 3, map[string]any{"title": "Neg", "address": longAddress, "latitude": math.Inf(-1)})
	s.add("Participant", 50, map[string]any{"item_id": int64(36), "identity_id": "x"})
	s.add("Participant", 51, map[string]any{"item_id": int64(999), "email": int64(5)})
	s.add("Participant", 52, map[string]any{"item_id": int64(999), "identity_id": int64(3)})
	s.add("Participant", 53, map[string]any{"item_id": int64(999), "identity_id": "y"})
	s.add("Recurrence", 2, map[string]any{"owner_id": int64(40), "frequency": int64(1), "end_date": math.Inf(-1)})
	s.add("ExceptionDate", 2, map[string]any{"owner_id": int64(41), "date": nil})
	s.tabs["ExceptionDate"].Insert(3, nil, int64(41)) // a short record: date not stored
	s.add("ExceptionDate", 4, map[string]any{"owner_id": int64(41), "date": math.Inf(1)})
	s.add("Alarm", 2, map[string]any{"calendaritem_owner_id": int64(42), "trigger_date": math.NaN()})
	cases := []struct {
		id   int64
		kw   map[string]any
		raw  map[string]any // raw key -> value; absent{} for a key that must not be present
		pay  map[string]any // payload key -> value
		note string
	}{
		{1, map[string]any{"entity_type": int64(9)}, map[string]any{"item_kind_state": "undecided", "entity_type": 9, "deletion_state": "not-evaluated"}, nil, "kind"},
		{2, map[string]any{"all_day": int64(5)}, map[string]any{"timing_state": "all-day-undecided", "all_day": 5}, map[string]any{"timing": "unknown"}, "all_day"},
		{3, map[string]any{"start_tz": nil}, map[string]any{"timing_state": "start-tz-absent"}, map[string]any{"timing": "unknown"}, "start-tz-absent"},
		{4, map[string]any{"start_tz": longZone}, map[string]any{"timing_state": "start-tz-too-long", "start_tz_state": "too-long", "start_tz_len": 302, "start_tz_sha256": sha(longZone)}, map[string]any{"start_timezone": absent{}}, "start-tz-too-long"},
		{5, map[string]any{"start_tz": "_float", "end_tz": "_float"}, map[string]any{"start_tz": "_float", "end_tz": "_float"}, map[string]any{"timing": "floating", "start_timezone": absent{}, "end_timezone": absent{}}, "floating"},
		{6, map[string]any{"orig_item_id": int64(1), "orig_date": d}, map[string]any{"orig_date": d, "orig_date_basis": "unestablished"}, map[string]any{"exception_of": map[string]any{"event_id": "1"}}, "orig"},
		{7, map[string]any{"end_date": nil}, map[string]any{"end_state": "none-stored"}, nil, "none-stored"},
		{8, map[string]any{"end_date": d - 60}, map[string]any{"end_state": "end-before-start"}, nil, "end-before-start"},
		{9, map[string]any{"start_date": nil}, map[string]any{"end_state": "no-start"}, nil, "no-start"},
		{10, map[string]any{"all_day": int64(1), "start_date": float64(cM + cH)}, map[string]any{"start_state": "all-day-not-midnight", "end_state": "no-start"}, nil, "start not midnight"},
		{11, map[string]any{"all_day": int64(1), "start_date": float64(cM), "end_date": float64(cM + cDay - 1)}, map[string]any{"end_state": "all-day-not-midnight"}, map[string]any{"start_date": "2023-11-15", "end_date": absent{}}, "end not midnight"},
		{12, map[string]any{"end_tz": "_float"}, map[string]any{"end_state": "timing-mismatch", "end_tz": "_float"}, nil, "timing-mismatch"},
		{13, map[string]any{"end_tz": []byte("Europe/Berlin")}, map[string]any{"end_state": "end-zone-undecided", "unknown_columns": map[string]any{"CalendarItem.end_tz": "type-mismatch"}}, nil, "B10 unreadable"},
		{14, map[string]any{"end_tz": longEnd}, map[string]any{"end_state": "end-zone-undecided", "end_tz_state": "too-long", "end_tz_len": 302, "end_tz_sha256": sha(longEnd), "unknown_columns": absent{}}, map[string]any{"end_timezone": absent{}}, "B10 too long"},
		{15, map[string]any{"summary": "Bad\x00Title"}, map[string]any{"text_flags": "title", "title_raw_b64": "QmFkAFRpdGxl"}, nil, "text_flags"},
		{16, map[string]any{"location_id": int64(99)}, map[string]any{"location_state": "no-Location-row"}, map[string]any{"location": absent{}}, "no-Location-row"},
		{17, map[string]any{"calendar_id": int64(99)}, map[string]any{"calendar_state": "no-Calendar-row"}, map[string]any{"calendar": absent{}}, "no-Calendar-row"},
		{18, map[string]any{"organizer_id": int64(99)}, map[string]any{"organizer_state": "no-Participant-row"}, map[string]any{"organizer": absent{}}, "no-Participant-row"},
		{19, map[string]any{"calendar_id": int64(2)}, map[string]any{"store_state": "no-Store-row", "store_type": absent{}}, map[string]any{"calendar": map[string]any{"id": "2", "title": "Orphan"}}, "no-Store-row"},
		{20, nil, map[string]any{"store_type": 2, "calendar_state": absent{}}, map[string]any{"calendar": map[string]any{"id": "1", "title": "Home", "account": map[string]any{"name": "iCloud"}}}, "store_type"},
		{21, map[string]any{"organizer_id": int64(30)}, map[string]any{"organizer_state": "no-Identity-row"}, map[string]any{"organizer": absent{}}, "no-Identity-row"},
		{
			22, nil,
			map[string]any{
				"attendees":         []any{map[string]any{"role": 1, "type": 2, "status": 3}},
				"attendees_unnamed": 2,
				"attendees_unnamed_entries": []any{
					map[string]any{"identity_state": "no-Identity-row"},
					map[string]any{"address_state": "too-long", "address_len": len(longMail), "address_sha256": sha(longMail)},
					map[string]any{"address_state": "undecided", "email": 7},
					map[string]any{"name_state": "undecided", "display_name_blob_b64": "Tg=="},
				},
				"unknown_columns": map[string]any{"Participant.email": "type-mismatch", "Identity.display_name": "type-mismatch"},
			},
			map[string]any{"attendees": []any{map[string]any{"address": "x@example.test", "role": "unknown", "type": "unknown", "status": "unknown"}}},
			"attendees",
		},
		{23, nil, map[string]any{"recurrence": []any{map[string]any{"frequency": 1, "interval": 2, "count": 3, "end_date": d, "specifier": "BYDAY=TU"}}}, map[string]any{"recurrence": map[string]any{"stored_rules": 1}}, "recurrence"},
		{24, nil, map[string]any{"exception_dates": []any{d}}, nil, "exception_dates"},
		{25, nil, map[string]any{"alarms": []any{map[string]any{"trigger_interval": -900, "type": 0}}}, map[string]any{"reminders": absent{}}, "alarms"},
		{26, map[string]any{"external_rep": []byte("rep")}, map[string]any{"external_rep_len": 3, "external_rep_sha256": sha("rep"), "external_rep": absent{}}, nil, "external_rep"},
		{
			27,
			map[string]any{"status": int64(1), "availability": int64(2), "privacy_level": int64(3), "hidden": int64(1), "has_recurrences": int64(0), "has_attendees": int64(1), "invitation_status": int64(4), "sequence_num": int64(5), "birthday_id": int64(6)},
			map[string]any{"status": 1, "availability": 2, "privacy_level": 3, "hidden": 1, "has_recurrences": 0, "has_attendees": 1, "invitation_status": 4, "sequence_num": 5, "birthday_id": 6},
			map[string]any{"status": "unknown", "availability": "unknown", "privacy": "unknown"},
			"as stored",
		},
		{28, map[string]any{"location_id": int64(1)}, map[string]any{"latitude": 95, "latitude_state": "out-of-range", "longitude": -181, "longitude_state": "out-of-range"}, map[string]any{"location": map[string]any{"text": "Far"}}, "coordinates"},
		{29, map[string]any{"location_id": "1"}, map[string]any{"join_undecided": []any{"Location"}, "location_state": absent{}, "unknown_columns": map[string]any{"CalendarItem.location_id": "type-mismatch"}}, nil, "join_undecided"},
		{30, map[string]any{"url": longURL}, map[string]any{"url_state": "too-long", "url_len": len(longURL), "url_sha256": sha(longURL)}, map[string]any{"url": absent{}}, "url"},
		// I1: the calendar title is a free text (cut and named, never silently); the Store name is an identifier
		{
			31,
			map[string]any{"calendar_id": int64(3)},
			map[string]any{"text_flags": "calendar_title", "calendar_title_raw_b64": base64.StdEncoding.EncodeToString([]byte(longTitle))},
			map[string]any{"calendar": map[string]any{"id": "3", "title": strings.Repeat("T", 4<<10)}, "truncated_fields": []any{"calendar_title"}},
			"calendar title",
		},
		{
			32,
			map[string]any{"calendar_id": int64(4)},
			map[string]any{"calendar_account_state": "too-long", "calendar_account_len": 1100, "calendar_account_sha256": sha(longStore)},
			map[string]any{"calendar": map[string]any{"id": "4", "title": "Long account"}},
			"store name",
		},
		// I3, m1: a non-finite coordinate is stated in its display form, out of range; a stored NaN is also listed
		{
			33,
			map[string]any{"location_id": int64(2)},
			map[string]any{
				"latitude": "NaN", "latitude_state": "out-of-range", "longitude": "+Inf", "longitude_state": "out-of-range",
				"unknown_columns": map[string]any{"Location.latitude": "type-mismatch"},
			},
			map[string]any{"location": map[string]any{"text": "Nan"}},
			"NaN and +Inf coordinates",
		},
		// I3: the location address is a free text of its own (location_address)
		{
			34,
			map[string]any{"location_id": int64(3)},
			map[string]any{"latitude": "-Inf", "latitude_state": "out-of-range", "text_flags": "location_address", "location_address_raw_b64": base64.StdEncoding.EncodeToString([]byte(longAddress))},
			map[string]any{"location": map[string]any{"text": "Neg", "address": strings.Repeat("A", 4<<10)}, "truncated_fields": []any{"location_address"}},
			"-Inf coordinate, location_address",
		},
		// I3: a raw-only value over 16 KiB is never kept nor cut: its length and SHA-256
		{35, map[string]any{"hidden": bigText}, map[string]any{"hidden": absent{}, "hidden_state": "too-long", "hidden_len": 17 << 10, "hidden_sha256": sha(bigText)}, nil, "maxStored"},
		// I3: an identity key that cannot be read is undecided: never "no address and no name", so not counted
		{
			36, nil,
			map[string]any{
				"attendees_unnamed": absent{}, "attendees_unnamed_entries": []any{map[string]any{"identity_state": "undecided"}},
				"join_undecided": []any{"Identity"}, "unknown_columns": map[string]any{"Participant.identity_id": "type-mismatch"},
			},
			nil,
			"identity_state undecided",
		},
		// I3, m2: the organizer's undecided address, name and identity are stated under organizer_*
		{37, map[string]any{"organizer_id": int64(51)}, map[string]any{"organizer_email": 5, "organizer_address_state": "undecided"}, map[string]any{"organizer": absent{}}, "organizer email"},
		{38, map[string]any{"organizer_id": int64(52)}, map[string]any{"organizer_name_state": "undecided", "organizer_display_name_blob_b64": "Tg=="}, map[string]any{"organizer": absent{}}, "organizer name"},
		{
			39,
			map[string]any{"organizer_id": int64(53)},
			map[string]any{"organizer_identity_state": "undecided", "organizer_state": absent{}, "join_undecided": []any{"Identity"}},
			map[string]any{"organizer": absent{}},
			"organizer identity",
		},
		// I2: a non-finite REAL in a raw-only column is kept in its display form and listed, never dropped
		{
			40,
			map[string]any{"orig_date": math.Inf(1)},
			map[string]any{
				"orig_date": "+Inf", "orig_date_basis": "unestablished",
				"recurrence":      []any{map[string]any{"frequency": 1, "end_date": "-Inf"}},
				"unknown_columns": map[string]any{"CalendarItem.orig_date": "type-mismatch", "Recurrence.end_date": "type-mismatch"},
			},
			nil,
			"non-finite orig_date and Recurrence.end_date",
		},
		// I3, m3, I2: a stored NULL date is a null element; an unknown one {"date_state": "undecided"}; +Inf its display form
		{
			41, nil,
			map[string]any{
				"exception_dates": []any{nil, map[string]any{"date_state": "undecided"}, "+Inf"},
				"unknown_columns": map[string]any{"ExceptionDate.date": "default-undecided,type-mismatch"}, // the kinds of its rows, joined
			},
			nil,
			"exception dates",
		},
		{42, nil, map[string]any{"alarms": []any{map[string]any{"trigger_date": "NaN"}}, "unknown_columns": map[string]any{"Alarm.trigger_date": "type-mismatch"}}, nil, "NaN trigger_date"},
	}
	for _, c := range cases {
		s.event(c.id, c.kw)
	}
	recs := byRowid(t, parseBuilt(t, s.bytes(), false))
	check := func(label string, m map[string]any, want map[string]any) {
		t.Helper()
		for k, w := range want {
			g, has := m[k]
			if _, no := w.(absent); no {
				if has {
					t.Errorf("%s: %s = %#v, want absent", label, k, g)
				}
				continue
			}
			if !has || !sameJSON(t, g, w) {
				t.Errorf("%s: %s = %#v (present %v), want %#v", label, k, g, has, w)
			}
		}
	}
	for _, c := range cases {
		r, ok := recs[c.id]
		if !ok {
			t.Errorf("row %d (%s): no record", c.id, c.note)
			continue
		}
		check(c.note+" raw", rawOf(r), c.raw)
		check(c.note+" payload", r.Payload, c.pay)
		if rawOf(r)["deletion_state"] != "not-evaluated" {
			t.Errorf("row %d (%s): deletion_state %#v", c.id, c.note, rawOf(r)["deletion_state"])
		}
	}

	// the typed pass (nil env, 4I3-P4, B2): a row joins nothing; each Get join whose key is stored and each Match join
	// by owner is "not-joined-recovered"; a NULL key states nothing
	typed := newBuilt()
	typed.add("Location", 1, map[string]any{"title": "Office"})
	typed.add("Calendar", 1, map[string]any{"title": "Home"})
	typed.add("Participant", 1, map[string]any{"item_id": int64(1), "email": "a@example.test"})
	typed.event(1, map[string]any{"location_id": int64(1), "organizer_id": int64(1)})
	typed.event(2, nil)
	got := mapTyped(t, typed.bytes())
	all := map[string]any{
		"location_state": "not-joined-recovered", "calendar_state": "not-joined-recovered", "organizer_state": "not-joined-recovered",
		"participants_state": "not-joined-recovered", "recurrence_state": "not-joined-recovered", "exception_dates_state": "not-joined-recovered",
		"alarms_state": "not-joined-recovered", "deletion_state": "not-evaluated",
	}
	check("typed row 1 raw", rawOf(got[1]), all)
	check("typed row 1 payload", got[1].Payload, map[string]any{"location": absent{}, "calendar": absent{}, "organizer": absent{}, "attendees": absent{}})
	check("typed row 2 raw", rawOf(got[2]), map[string]any{"location_state": absent{}, "organizer_state": absent{}, "calendar_state": "not-joined-recovered", "participants_state": "not-joined-recovered"})
}

// noteSink is the Notes of a mapping Context in a test: the typed pass's mapping writes none.
type noteSink struct{}

func (noteSink) Note(string, string) {}

// mapTyped runs the typed pass's mapping (Mappings()[0], no Parse context, 4I3-P4) over every live CalendarItem row
// of a Go-built store at the system path, and returns the records by rowid.
func mapTyped(t *testing.T, db []byte) map[int64]records.Record {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := ioscalendar.Parser{}
	in := bundleAt(h, map[string][]byte{"db": db}, true).Input(h, p, false)
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Release()
	mc := sqlitedb.NewContext(t.Context(), d, in, noteSink{})
	defer mc.Close()
	ms := p.Mappings()
	if len(ms) != 1 {
		t.Fatalf("Mappings = %+v, want the one CalendarItem mapping", ms)
	}
	m := ms[0]
	tab, err := d.Table(t.Context(), m.Table, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]records.Record{}
	err = tab.Scan(t.Context(), func(row sqlitedb.Row) error {
		recs, err := m.Map(mc, row)
		if err != nil {
			return err
		}
		id, _ := row.Rowid()
		for _, r := range recs {
			out[id] = r
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
