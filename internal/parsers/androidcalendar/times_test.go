package androidcalendar

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The constants of the CORE scenario (tools/fixtures/calendar_android.py).
const (
	tD      = int64(1700000000000) // 2023-11-14T22:13:20Z
	tM      = int64(1700006400000) // 2023-11-15T00:00:00Z
	tH      = int64(3600000)
	tDay    = int64(86400000)
	ms1955  = int64(-457747200000) // 1955-07-01T00:00:00Z
	ms2100m = int64(4102444800000) - tDay
)

// timesDDL is the part of the 601 Events table readTimes reads (Go-built, user version 601, F05).
const timesDDL = `CREATE TABLE Events (_id INTEGER PRIMARY KEY AUTOINCREMENT, calendar_id INTEGER NOT NULL, eventTimezone TEXT,
dtstart INTEGER, dtend INTEGER, duration TEXT, allDay INTEGER NOT NULL DEFAULT 0, originalInstanceTime INTEGER,
originalAllDay INTEGER, deleted INTEGER NOT NULL DEFAULT 0)`

// timesCols is the column order of timesDDL after _id.
var timesCols = []string{"calendar_id", "eventTimezone", "dtstart", "dtend", "duration", "allDay", "originalInstanceTime", "originalAllDay", "deleted"}

// evRow is one Events row: the CORE defaults of ev() (a timed hour from D in Europe/Berlin), overridden by kw (a nil
// value stores NULL).
type evRow struct {
	id int64
	kw map[string]any
}

func (r evRow) vals() []any {
	def := map[string]any{"calendar_id": int64(1), "allDay": int64(0), "dtstart": tD, "dtend": tD + tH, "eventTimezone": "Europe/Berlin", "deleted": int64(0)}
	out := []any{nil} // _id: the rowid alias stores NULL
	for _, c := range timesCols {
		v, ok := r.kw[c]
		if !ok {
			v = def[c]
		}
		out = append(out, v)
	}
	return out
}

// readOut is what readTimes made of one row.
type readOut struct {
	et           eventTimes
	raw, unknown map[string]any
}

// readRows builds the rows into a database and runs readTimes over each Events row.
func readRows(t *testing.T, rows ...evRow) map[int64]readOut {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 601})
	tb := b.CreateTable("Events", timesDDL)
	for _, r := range rows {
		tb.Insert(r.id, r.vals()...)
	}
	img := b.Bytes()
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(img), DBSize: int64(len(img))}, parse.NewBudget(256<<20).View())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	tab, err := db.Table(t.Context(), "Events", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := rowrun.Cols{}
	for _, n := range timesCols {
		c[n] = tab.Col(n)
	}
	out := map[int64]readOut{}
	err = tab.Scan(t.Context(), func(row sqlitedb.Row) error {
		id, _ := row.Rowid()
		o := readOut{raw: map[string]any{}, unknown: map[string]any{}}
		o.et = readTimes(row, c, o.raw, o.unknown)
		out[id] = o
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(rows) {
		t.Fatalf("%d rows read, want %d", len(out), len(rows))
	}
	return out
}

func utcAt(ms int64) *records.Time {
	return &records.Time{T: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond), Basis: records.BasisUTC}
}

func localAt(ms int64) *records.Time {
	return &records.Time{T: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond), Basis: records.BasisLocalUnknown}
}

func sameTime(a, b *records.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.T.Equal(b.T) && a.Basis == b.Basis && a.OffsetMin == b.OffsetMin
}

func fmtTime(t *records.Time) string {
	if t == nil {
		return "<nil>"
	}
	return t.T.UTC().Format(time.RFC3339Nano) + " " + string(t.Basis)
}

// expect checks one read row: timing, start, end, dates, and the raw keys (a nil want value means "absent").
func expect(t *testing.T, label string, o readOut, timing string, start, end *records.Time, sdate, edate string, raw map[string]any) {
	t.Helper()
	if o.et.Timing != timing {
		t.Errorf("%s: timing %q, want %q", label, o.et.Timing, timing)
	}
	if !sameTime(o.et.Start, start) {
		t.Errorf("%s: start %s, want %s", label, fmtTime(o.et.Start), fmtTime(start))
	}
	if !sameTime(o.et.End, end) {
		t.Errorf("%s: end %s, want %s", label, fmtTime(o.et.End), fmtTime(end))
	}
	if o.et.StartDate != sdate || o.et.EndDate != edate {
		t.Errorf("%s: dates %q..%q, want %q..%q", label, o.et.StartDate, o.et.EndDate, sdate, edate)
	}
	for k, w := range raw {
		g, has := o.raw[k]
		switch {
		case w == nil && has:
			t.Errorf("%s: raw.%s = %v, want absent", label, k, g)
		case w != nil && (!has || g != w):
			t.Errorf("%s: raw.%s = %v (present %v), want %v", label, k, g, has, w)
		}
	}
}

// Spec 5.2 (CORE rows 4, 6, 7, 41 and 31): an all-day value at an exact UTC midnight is a date, its Time the date at
// 00:00 with basis local-unknown and never a utc instant; end_date is exclusive; a 1955 date is invalid (Q5) and has
// no date; allDay other than 0 or 1 is unknown timing with no time (spec 7.3).
func TestAndroidCalendarAllDayIsADateNotAnInstant(t *testing.T) {
	got := readRows(t,
		evRow{4, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": nil, "duration": "P1D", "eventTimezone": "UTC"}},
		evRow{6, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + tDay, "eventTimezone": "UTC"}},
		evRow{7, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + 3*tDay, "eventTimezone": "UTC"}},
		evRow{41, map[string]any{"allDay": int64(1), "dtstart": ms1955, "dtend": ms1955 + tDay, "eventTimezone": "UTC"}},
		evRow{31, map[string]any{"allDay": int64(2)}},
	)
	expect(t, "row 4", got[4], "all_day", localAt(tM), localAt(tM+tDay), "2023-11-15", "2023-11-16",
		map[string]any{"time_end_basis": "dtstart+duration", "all_day_zone_state": nil, "start_state": nil, "end_state": nil})
	expect(t, "row 6", got[6], "all_day", localAt(tM), localAt(tM+tDay), "2023-11-15", "2023-11-16", map[string]any{"time_end_basis": "dtend"})
	expect(t, "row 7", got[7], "all_day", localAt(tM), localAt(tM+3*tDay), "2023-11-15", "2023-11-18", map[string]any{"time_end_basis": "dtend"})
	expect(t, "row 41", got[41], "all_day", nil, nil, "", "", map[string]any{"dtstart": ms1955, "dtstart_status": "invalid", "end_state": "no-start", "start_state": nil})
	expect(t, "row 31", got[31], "unknown", nil, nil, "", "", map[string]any{"timing_state": "undecided", "allDay": int64(2), "end_state": nil, "time_end_basis": nil})
}

// Spec 5.2 and B5 (CORE row 8): an all-day start off midnight gives no date and no time, stated, and then no end; an
// all-day end off midnight gives no end_date and no TimeEnd, stated (B1 for Android): nothing is repaired by a zone.
func TestCalendarAllDayNotMidnightIsStatedNotRepaired(t *testing.T) {
	got := readRows(t,
		evRow{8, map[string]any{"allDay": int64(1), "dtstart": tM + tH, "dtend": tM + tDay + tH, "eventTimezone": "UTC"}},
		evRow{108, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + tDay + tH, "eventTimezone": "UTC"}},
		evRow{109, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": nil, "duration": "PT1H", "eventTimezone": "UTC"}},
	)
	expect(t, "row 8", got[8], "all_day", nil, nil, "", "", map[string]any{"start_state": "all-day-not-midnight", "end_state": "no-start", "dtstart": tM + tH})
	expect(t, "end off midnight", got[108], "all_day", localAt(tM), nil, "2023-11-15", "", map[string]any{"end_state": "all-day-not-midnight", "time_end_basis": nil})
	expect(t, "duration off midnight", got[109], "all_day", localAt(tM), nil, "2023-11-15", "", map[string]any{"end_state": "all-day-not-midnight", "time_end_basis": nil})
}

// Spec 5.3, 4I3-P7 and B5 (CORE rows 1-3, 5, 24-26, 28-30): duration first, then dtend, else none; both stored and
// disagreeing is a stated conflict with duration winning; an unparsed or out-of-range duration gives no end and no
// fallback to dtend; no readable start gives no end (B5); a computed end outside the decoder's range is no end.
func TestAndroidCalendarEndFollowsProviderRule(t *testing.T) {
	got := readRows(t,
		evRow{1, nil},
		evRow{2, map[string]any{"dtend": nil, "duration": "PT1H"}},
		evRow{3, map[string]any{"dtend": nil, "duration": "P3600S"}},
		evRow{5, map[string]any{"dtend": nil, "duration": "P2W"}},
		evRow{24, map[string]any{"dtend": tD + 2*tH, "duration": "PT1H"}},
		evRow{25, map[string]any{"duration": "P1Y"}},
		evRow{26, map[string]any{"dtend": nil, "duration": "P9999999999S"}},
		evRow{28, map[string]any{"dtstart": nil, "dtend": tD}},
		evRow{29, map[string]any{"dtstart": int64(0)}},
		evRow{30, map[string]any{"dtstart": int64(5000000000000)}},
		evRow{100, map[string]any{"dtend": nil}},
		evRow{101, map[string]any{"dtend": nil, "duration": "P" + string(bytes.Repeat([]byte("1"), 70)) + "S"}},
		evRow{102, map[string]any{"dtstart": ms2100m, "dtend": nil, "duration": "P2D"}},
		evRow{103, map[string]any{"dtend": tD + tH, "duration": "PT1H"}},
		evRow{104, map[string]any{"dtend": int64(0), "duration": "PT1H"}},
	)
	expect(t, "row 1", got[1], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"time_end_basis": "dtend", "dtstart": tD, "dtend": tD + tH, "end_state": nil})
	expect(t, "row 2", got[2], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"time_end_basis": "dtstart+duration", "end_conflict": nil})
	expect(t, "row 3", got[3], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"time_end_basis": "dtstart+duration"})
	expect(t, "row 5", got[5], "timed", utcAt(tD), utcAt(tD+14*tDay), "", "", map[string]any{"time_end_basis": "dtstart+duration"})
	expect(t, "row 24", got[24], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"time_end_basis": "dtstart+duration", "end_conflict": true})
	expect(t, "row 25", got[25], "timed", utcAt(tD), nil, "", "", map[string]any{"duration_state": "unparsed", "time_end_basis": nil, "end_state": nil})
	expect(t, "row 26", got[26], "timed", utcAt(tD), nil, "", "", map[string]any{"duration_state": "out-of-range", "time_end_basis": nil})
	// B5: dtstart NULL with dtend set: no end, end_state no-start; the NULL is raw null (a stored NULL, not an unknown)
	expect(t, "row 28 (B5)", got[28], "timed", nil, nil, "", "", map[string]any{"end_state": "no-start", "time_end_basis": nil, "dtend": tD})
	if v, has := got[28].raw["dtstart"]; !has || v != nil {
		t.Errorf("row 28: raw.dtstart = %v (present %v), want a stored null", v, has)
	}
	expect(t, "row 29", got[29], "timed", nil, nil, "", "", map[string]any{"dtstart_status": "sentinel", "end_state": "no-start"})
	expect(t, "row 30", got[30], "timed", nil, nil, "", "", map[string]any{"dtstart_status": "invalid", "end_state": "no-start"})
	expect(t, "none stored", got[100], "timed", utcAt(tD), nil, "", "", map[string]any{"end_state": "none-stored", "time_end_basis": nil})
	// a duration over 64 bytes is unparsed (coordinator ruling on the oracle: duration_state takes only unparsed or out-of-range)
	expect(t, "duration too long", got[101], "timed", utcAt(tD), nil, "", "", map[string]any{"duration_state": "unparsed", "end_state": nil})
	// a computed end at or past 2100 is out-of-range (coordinator ruling D1 on Task 3): no TimeEnd, no new key
	expect(t, "end beyond 2100", got[102], "timed", utcAt(ms2100m), nil, "", "", map[string]any{"end_state": "out-of-range", "end_status": nil, "time_end_basis": nil})
	// both stored and agreeing: no conflict
	expect(t, "agreeing", got[103], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"time_end_basis": "dtstart+duration", "end_conflict": nil})
	// spec 5.3 "both are stored and disagree": a stored dtend that is no time (a sentinel) still disagrees (fix round 1, M2)
	expect(t, "dtend sentinel", got[104], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"time_end_basis": "dtstart+duration", "end_conflict": true, "dtend_status": "sentinel"})
}

// Spec 5.3 (CORE row 27): an end before the start, stored or computed, is not stated.
func TestCalendarEndBeforeStartIsNotStated(t *testing.T) {
	got := readRows(t,
		evRow{27, map[string]any{"dtend": tD - tH}},
		evRow{110, map[string]any{"dtend": nil, "duration": "-PT30M"}},
		evRow{111, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM - tDay, "eventTimezone": "UTC"}},
	)
	expect(t, "row 27", got[27], "timed", utcAt(tD), nil, "", "", map[string]any{"end_state": "end-before-start", "time_end_basis": nil})
	expect(t, "negative duration", got[110], "timed", utcAt(tD), nil, "", "", map[string]any{"end_state": "end-before-start", "time_end_basis": nil})
	expect(t, "all-day", got[111], "all_day", localAt(tM), nil, "2023-11-15", "", map[string]any{"end_state": "end-before-start"})
}

// Review Focus 1, 4I3-P8 (CORE row 20): an all-day row in a zone other than UTC is still read from its UTC midnight;
// the breach of the provider's contract is stated, the zone never applied. A row in UTC states nothing.
func TestAndroidCalendarAllDayWithANonUTCZoneIsStated(t *testing.T) {
	got := readRows(t,
		evRow{20, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + tDay, "eventTimezone": "America/New_York"}},
		evRow{6, map[string]any{"allDay": int64(1), "dtstart": tM, "dtend": tM + tDay, "eventTimezone": "UTC"}},
		evRow{120, map[string]any{"eventTimezone": "America/New_York"}},
	)
	expect(t, "row 20", got[20], "all_day", localAt(tM), localAt(tM+tDay), "2023-11-15", "2023-11-16", map[string]any{"all_day_zone_state": "not-utc"})
	expect(t, "row 6", got[6], "all_day", localAt(tM), localAt(tM+tDay), "2023-11-15", "2023-11-16", map[string]any{"all_day_zone_state": nil})
	expect(t, "timed", got[120], "timed", utcAt(tD), utcAt(tD+tH), "", "", map[string]any{"all_day_zone_state": nil})
}

// Review Focus 2, 4I3-P7 (CORE row 21): a recurring row with dtend and no duration ends at dtend, and says so.
func TestAndroidCalendarRecurringEndFromDtendIsStated(t *testing.T) {
	got := readRows(t, evRow{21, map[string]any{"dtend": tD + 2*tH}})
	expect(t, "row 21", got[21], "timed", utcAt(tD), utcAt(tD+2*tH), "", "", map[string]any{"time_end_basis": "dtend"})
}

// Spec 5.2 and 6.2: original_start follows originalAllDay: 0 a utc instant, 1 the date rule of the start, anything
// else undecided.
func TestAndroidCalendarOriginalStartBasis(t *testing.T) {
	got := readRows(t,
		evRow{9, map[string]any{"originalInstanceTime": tD + 7*tDay, "originalAllDay": int64(0)}},
		evRow{130, map[string]any{"originalInstanceTime": tM, "originalAllDay": int64(1)}},
		evRow{131, map[string]any{"originalInstanceTime": tM + tH, "originalAllDay": int64(1)}},
		evRow{132, map[string]any{"originalInstanceTime": tD, "originalAllDay": int64(2)}},
		evRow{133, map[string]any{"originalInstanceTime": tD, "originalAllDay": nil}},
		evRow{134, map[string]any{"originalInstanceTime": tD, "originalAllDay": "x"}},
		evRow{135, map[string]any{"originalInstanceTime": tD, "originalAllDay": math.NaN()}},
		evRow{136, nil},
	)
	cases := []struct {
		id    int64
		want  *records.Time
		state any
	}{
		{9, utcAt(tD + 7*tDay), nil},
		{130, localAt(tM), nil},
		{131, nil, "all-day-not-midnight"},
		{132, nil, "undecided"},
		{133, nil, "undecided"},
		{134, nil, "undecided"},
		{135, nil, "undecided"},
		{136, nil, nil},
	}
	for _, c := range cases {
		o := got[c.id]
		if !sameTime(o.et.OriginalStart, c.want) {
			t.Errorf("row %d: original_start %s, want %s", c.id, fmtTime(o.et.OriginalStart), fmtTime(c.want))
		}
		if g := o.raw["original_start_state"]; g != c.state {
			t.Errorf("row %d: raw.original_start_state %v, want %v", c.id, g, c.state)
		}
	}
	for _, id := range []int64{134, 135} {
		if got[id].unknown["Events.originalAllDay"] != "type-mismatch" {
			t.Errorf("row %d: unknown_columns %v, want Events.originalAllDay type-mismatch", id, got[id].unknown)
		}
	}
	// spec 5.4: a stored NULL is raw null (fix round 1, M3)
	if v, has := got[133].raw["originalAllDay"]; !has || v != nil {
		t.Errorf("row 133: raw.originalAllDay = %v (present %v), want a stored null", v, has)
	}
	if _, has := got[136].raw["originalInstanceTime"]; has {
		t.Errorf("row 136: raw.originalInstanceTime stated for a row that is no exception: %v", got[136].raw)
	}
}

// Spec 5.4, 4I2-A6, F02: a NaN, a REAL, a BLOB or a TEXT in a time or timing cell is undecided: no time, the column in
// unknown_columns, never raw.<col> = null. Task 6 adds rows to this table (one function per package, F02).
func TestCalendarUnreadableCellsAreUndecided(t *testing.T) {
	cases := []struct {
		name   string
		kw     map[string]any
		col    string // the unknown column
		timing string
		start  bool // a start is still read (the unreadable cell is not the start)
		raw    any  // the stored value raw keeps for a readable cell of another class (C1, ruling D6); nil: no raw key
	}{
		{"dtstart NaN", map[string]any{"dtstart": math.NaN()}, "dtstart", "timed", false, nil},
		{"dtstart REAL", map[string]any{"dtstart": 1.7e12}, "dtstart", "timed", false, nil},
		{"dtstart BLOB", map[string]any{"dtstart": []byte{1, 2, 3}}, "dtstart", "timed", false, nil},
		{"dtstart TEXT", map[string]any{"dtstart": "1700000000000"}, "dtstart", "timed", false, nil},
		{"allDay NaN", map[string]any{"allDay": math.NaN()}, "allDay", "unknown", false, nil},
		{"allDay TEXT", map[string]any{"allDay": "1"}, "allDay", "unknown", false, "1"},
		{"allDay REAL", map[string]any{"allDay": 1.5}, "allDay", "unknown", false, 1.5},
		{"allDay BLOB", map[string]any{"allDay": []byte{1, 2, 3}}, "allDay", "unknown", false, "AQID"},
		{"allDay BLOB of one byte", map[string]any{"allDay": []byte{0xff}}, "allDay", "unknown", false, "/w=="},
		{"allDay invalid UTF-8", map[string]any{"allDay": string([]byte{'a', 0xff})}, "allDay", "unknown", false, "a�"},
		{"dtend NaN", map[string]any{"dtend": math.NaN()}, "dtend", "timed", true, nil},
		{"dtend TEXT", map[string]any{"dtend": "1700003600000"}, "dtend", "timed", true, nil},
		{"duration NaN", map[string]any{"dtend": nil, "duration": math.NaN()}, "duration", "timed", true, nil},
		{"duration INTEGER", map[string]any{"dtend": nil, "duration": int64(3600)}, "duration", "timed", true, nil},
		{"originalInstanceTime NaN", map[string]any{"originalInstanceTime": math.NaN(), "originalAllDay": int64(0)}, "originalInstanceTime", "timed", true, nil},
	}
	var rows []evRow
	for i, c := range cases {
		rows = append(rows, evRow{int64(200 + i), c.kw})
	}
	got := readRows(t, rows...)
	for i, c := range cases {
		o := got[int64(200+i)]
		if o.unknown["Events."+c.col] != "type-mismatch" {
			t.Errorf("%s: unknown_columns %v, want Events.%s type-mismatch", c.name, o.unknown, c.col)
		}
		v, has := o.raw[c.col]
		switch {
		case c.raw == nil && has:
			t.Errorf("%s: raw.%s = %v: an unreadable cell is never a stored value or null", c.name, c.col, v)
		case c.raw != nil && !rawEqual(v, c.raw):
			t.Errorf("%s: raw.%s = %#v (present %v), want the stored value %#v", c.name, c.col, v, has, c.raw)
		}
		if o.et.Timing != c.timing {
			t.Errorf("%s: timing %q, want %q", c.name, o.et.Timing, c.timing)
		}
		if (o.et.Start != nil) != c.start {
			t.Errorf("%s: start %s", c.name, fmtTime(o.et.Start))
		}
		if c.start && c.col != "originalInstanceTime" {
			// the end depends on the unreadable cell: undecided, so no end and no end_state that would claim absence
			if o.et.End != nil || o.raw["end_state"] != nil || o.raw["time_end_basis"] != nil {
				t.Errorf("%s: end %s, end_state %v, time_end_basis %v: an undecided end is never stated or none-stored",
					c.name, fmtTime(o.et.End), o.raw["end_state"], o.raw["time_end_basis"])
			}
		}
		if c.col == "originalInstanceTime" && (o.et.OriginalStart != nil || o.raw["original_start_state"] != nil) {
			t.Errorf("%s: original_start %s, state %v", c.name, fmtTime(o.et.OriginalStart), o.raw["original_start_state"])
		}
		if c.col == "allDay" && o.raw["timing_state"] != "undecided" {
			t.Errorf("%s: raw.timing_state %v, want undecided", c.name, o.raw["timing_state"])
		}
	}

	// cells beyond the times, mapped by the full Events mapping (plan 4I3 Task 6, F02): a NaN code, a BLOB free text
	// and a NaN reminder offset are listed and give no value: never NULL, never a default, never a guessed label
	recs := mapRows(t, []evRow{
		{1, map[string]any{"eventStatus": math.NaN()}},
		{2, map[string]any{"title": []byte{0xde, 0xad}}},
		{3, nil},
	}, [][]any{{int64(3), math.NaN(), int64(1)}})
	for _, c := range []struct {
		id       int64
		name     string
		col, key string // the unknown column, the payload key
		want     any    // nil: absent
	}{
		{1, "eventStatus NaN", "Events.eventStatus", "status", "unknown"},
		{2, "title BLOB", "Events.title", "title", nil},
	} {
		r := recs[c.id]
		raw, _ := r.Payload["raw"].(map[string]any)
		uc, _ := raw["unknown_columns"].(map[string]any)
		if uc[c.col] != "type-mismatch" {
			t.Errorf("%s: unknown_columns %v, want %s type-mismatch", c.name, uc, c.col)
		}
		if got, has := r.Payload[c.key]; (c.want == nil && has) || (c.want != nil && got != c.want) {
			t.Errorf("%s: payload.%s = %#v (present %v), want %#v", c.name, c.key, got, has, c.want)
		}
	}
	if raw, _ := recs[1].Payload["raw"].(map[string]any); raw["eventStatus"] != nil {
		t.Errorf("eventStatus NaN: raw.eventStatus = %#v: an unreadable cell is never a stored value", raw["eventStatus"])
	}
	r3 := recs[3]
	raw3, _ := r3.Payload["raw"].(map[string]any)
	uc, _ := raw3["unknown_columns"].(map[string]any)
	if uc["Reminders.minutes"] != "type-mismatch" {
		t.Errorf("minutes NaN: unknown_columns %v, want Reminders.minutes type-mismatch", uc)
	}
	if got := fmt.Sprint(r3.Payload["reminders"]); got != "[map[method:alert]]" {
		t.Errorf("minutes NaN: reminders %s, want the method only (no before_s)", got)
	}
	if got := fmt.Sprint(raw3["reminders"]); got != "[map[method:1]]" {
		t.Errorf("minutes NaN: raw.reminders %s, want the stored method only (an unreadable minutes is never a value)", got)
	}
}

// mapDDL is the part of the 601 Events table mapRows reads beyond timesDDL, and remindersDDL the 601 Reminders table.
const (
	mapDDL = `CREATE TABLE Events (_id INTEGER PRIMARY KEY AUTOINCREMENT, calendar_id INTEGER NOT NULL, eventTimezone TEXT,
dtstart INTEGER, dtend INTEGER, duration TEXT, allDay INTEGER NOT NULL DEFAULT 0, originalInstanceTime INTEGER,
originalAllDay INTEGER, deleted INTEGER NOT NULL DEFAULT 0, eventStatus INTEGER, title TEXT)`
	remindersDDL = `CREATE TABLE Reminders (_id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER, minutes INTEGER, method INTEGER)`
)

// mapRows builds Events rows (timesCols, then eventStatus and title from kw) and Reminders rows (event_id, minutes,
// method) into a database and maps every Events row with the live mapping (an empty guard), by rowid.
func mapRows(t *testing.T, rows []evRow, reminders [][]any) map[int64]records.Record {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 601})
	tb := b.CreateTable("Events", mapDDL)
	for _, r := range rows {
		tb.Insert(r.id, append(r.vals(), r.kw["eventStatus"], r.kw["title"])...)
	}
	rt := b.CreateTable("Reminders", remindersDDL)
	for i, r := range reminders {
		rt.Insert(int64(i+1), append([]any{nil}, r...)...)
	}
	img := b.Bytes()
	ctx := t.Context()
	in := &parse.Input{
		Primary:   parse.Artifact{ID: "a1", Size: int64(len(img)), Logical: "android:/data/data/com.android.providers.calendar/databases/calendar.db"},
		Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: int64(len(img))}},
		Budget:    parse.NewBudget(256 << 20).View(),
	}
	db, err := sqlitedb.Open(ctx, sqlitedb.Files{DB: bytes.NewReader(img), DBSize: int64(len(img))}, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Release()
	mc := sqlitedb.NewContext(ctx, db, in, nopNotes{})
	defer mc.Close()
	m := eventMappingWith(&env{})
	tab, err := db.Table(ctx, "Events", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]records.Record{}
	err = tab.Scan(ctx, func(row sqlitedb.Row) error {
		recs, err := m.Map(mc, row)
		if err != nil || len(recs) != 1 {
			return fmt.Errorf("%d records: %w", len(recs), err)
		}
		id, _ := row.Rowid()
		out[id] = recs[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type nopNotes struct{}

func (nopNotes) Note(string, string) {}

// rawEqual compares a raw value with the expected one; a BLOB is expected as its base64 text ({"blob_b64": ...}).
func rawEqual(got, want any) bool {
	if m, ok := got.(map[string]any); ok {
		return len(m) == 1 && m["blob_b64"] == want
	}
	return got == want
}
