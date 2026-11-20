package ioscalendar_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records"
)

// recordedVariants are the fixtures whose oracle has records.
func recordedVariants() []string {
	return []string{"calendar-ios", "calendar-ios-deleted", "calendar-ios-extract", "calendar-ios-noentitytype"}
}

// at is a record time: an RFC 3339 instant ("...Z", basis utc) or a wall-clock reading without a zone (basis
// local-unknown, the reading as if UTC), the encoding of the oracles.
func at(t *testing.T, s string) *records.Time {
	t.Helper()
	if strings.HasSuffix(s, "Z") {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return &records.Time{T: v, Basis: records.BasisUTC}
	}
	v, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		t.Fatal(err)
	}
	return &records.Time{T: v, Basis: records.BasisLocalUnknown}
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

// want states what one record must hold: the timing, the start and end ("" for none), the dates, and raw keys (a
// nil value means "absent"; use isNull for a stated null).
type want struct {
	timing, start, end, sdate, edate string
	raw                              map[string]any
}

// isNull marks a raw key that must be present with the value null.
type isNull struct{}

func check(t *testing.T, label string, r records.Record, w want) {
	t.Helper()
	if got := payloadStr(r, "timing"); got != w.timing {
		t.Errorf("%s: timing %q, want %q", label, got, w.timing)
	}
	var start, end *records.Time
	if w.start != "" {
		start = at(t, w.start)
	}
	if w.end != "" {
		end = at(t, w.end)
	}
	if !sameTime(r.Time, start) {
		t.Errorf("%s: Time %s, want %s", label, fmtTime(r.Time), fmtTime(start))
	}
	if !sameTime(r.TimeEnd, end) {
		t.Errorf("%s: TimeEnd %s, want %s", label, fmtTime(r.TimeEnd), fmtTime(end))
	}
	if sd, ed := payloadStr(r, "start_date"), payloadStr(r, "end_date"); sd != w.sdate || ed != w.edate {
		t.Errorf("%s: dates %q..%q, want %q..%q", label, sd, ed, w.sdate, w.edate)
	}
	raw := rawOf(r)
	for k, v := range w.raw {
		g, has := raw[k]
		switch v.(type) {
		case nil:
			if has {
				t.Errorf("%s: raw.%s = %#v, want absent", label, k, g)
			}
		case isNull:
			if !has || g != nil {
				t.Errorf("%s: raw.%s = %#v (present %v), want null", label, k, g, has)
			}
		default:
			if !has || !sameValue(g, v) {
				t.Errorf("%s: raw.%s = %#v (present %v), want %#v", label, k, g, has, v)
			}
		}
	}
}

// sameValue compares raw values; two numbers compare by value, because the store may keep an integral REAL as an
// INTEGER on disk (the engine's REAL-affinity optimisation) and the reader states the class it finds.
func sameValue(a, b any) bool {
	fa, aok := numberOf(a)
	fb, bok := numberOf(b)
	if aok && bok {
		return fa == fb && math.Signbit(fa) == math.Signbit(fb)
	}
	return a == b
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// goRecords parses Go-built rows at the system path and indexes the records by ROWID.
func goRecords(t *testing.T, rows ...item) map[int64]records.Record {
	t.Helper()
	res := parseAt(t, map[string][]byte{"db": goStore(rows...)}, true)
	if res.Err != nil || res.Refused != 0 || len(res.Records) != len(rows) {
		t.Fatalf("Go-built rows: err %v, %d refused, %d records, warnings %v", res.Err, res.Refused, len(res.Records), res.Warnings)
	}
	return byRowid(t, res)
}

// Spec 5.1: the timing decides the basis of every record time: timed is utc, all-day and floating local-unknown,
// unknown has no time; no time states an offset (no zone is ever applied).
func TestCalendarTimingDecidesTimeBasis(t *testing.T) {
	n := 0
	for _, v := range recordedVariants() {
		res := parseVariant(t, v)
		if res.Err != nil || res.Refused != 0 {
			t.Fatalf("%s: err %v, %d refused", v, res.Err, res.Refused)
		}
		for _, r := range res.Records {
			n++
			timing := payloadStr(r, "timing")
			want := map[string]records.Basis{"timed": records.BasisUTC, "all_day": records.BasisLocalUnknown, "floating": records.BasisLocalUnknown}[timing]
			ts := []*records.Time{r.Time, r.TimeEnd}
			for i := range r.Times {
				ts = append(ts, &r.Times[i].Time)
			}
			if timing == "unknown" && (r.Time != nil || r.TimeEnd != nil) {
				t.Errorf("%s %s: unknown timing with a time", v, r.Locator)
			}
			for i, tm := range ts {
				if tm == nil {
					continue
				}
				if tm.OffsetMin != 0 || tm.Basis == records.BasisLocalOffset {
					t.Errorf("%s %s: time %d states an offset: %+v", v, r.Locator, i, tm)
				}
				if i < 2 && tm.Basis != want {
					t.Errorf("%s %s: timing %q, basis %q", v, r.Locator, timing, tm.Basis)
				}
				if i >= 2 && tm.Basis != records.BasisUTC {
					t.Errorf("%s %s: %s basis %q, want utc (a Cocoa instant)", v, r.Locator, r.Times[i-2].Kind, tm.Basis)
				}
			}
			if timing == "all_day" && r.Time != nil && r.Time.T.UTC().Format("2006-01-02T15:04:05.999999999") != payloadStr(r, "start_date")+"T00:00:00" {
				t.Errorf("%s %s: all-day Time %s is not the midnight of start_date %q", v, r.Locator, fmtTime(r.Time), payloadStr(r, "start_date"))
			}
		}
	}
	if n == 0 {
		t.Fatal("no record")
	}
}

// Spec 5.1 and 8.2 (4I3 state vocabulary): all_day 1 is all-day whatever the zone; all_day 0 is floating with
// start_tz _float (raw only) and timed with any other non-empty zone of at most 256 bytes (as cleaned: an invalid byte
// counts as the three bytes of U+FFFD); anything else is unknown timing with no time and raw.timing_state.
func TestIOSCalendarTimingClassFollowsAllDayAndStartZone(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	check(t, "row 1", recs[1], want{timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-14T23:13:20Z", raw: map[string]any{"timing_state": nil}})
	if payloadStr(recs[1], "start_timezone") != "Europe/Berlin" || payloadStr(recs[1], "end_timezone") != "Europe/Berlin" {
		t.Errorf("row 1: zones %q, %q, want Europe/Berlin as stored", payloadStr(recs[1], "start_timezone"), payloadStr(recs[1], "end_timezone"))
	}
	check(t, "row 2", recs[2], want{timing: "floating", start: "2023-11-14T22:13:20", end: "2023-11-14T22:43:20", raw: map[string]any{"start_tz": "_float", "end_tz": "_float"}})
	if payloadStr(recs[2], "start_timezone") != "" || payloadStr(recs[2], "end_timezone") != "" {
		t.Error("row 2: the _float marker is stated as a zone name")
	}
	check(t, "row 6", recs[6], want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-absent", "end_state": nil, "start_date": int64(cD)}})
	check(t, "row 22", recs[22], want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-too-long"}})
	if payloadStr(recs[22], "start_timezone") != "" {
		t.Error("row 22: a zone over 256 bytes is stated")
	}
	check(t, "row 23", recs[23], want{timing: "unknown", raw: map[string]any{"timing_state": "all-day-undecided", "all_day": int64(2)}})

	z256 := strings.Repeat("z", 256)
	got := goRecords(t,
		item{1, map[string]any{"start_tz": ""}},
		item{2, map[string]any{"all_day": nil}},
		item{3, map[string]any{"all_day": int64(1), "start_tz": nil, "start_date": float64(cM), "end_date": float64(cM + cDay)}},
		item{4, map[string]any{"start_tz": z256}},
		item{5, map[string]any{"start_tz": z256 + "z"}},
		item{6, map[string]any{"start_tz": strings.Repeat("z", 254) + "\xff"}},
		item{7, map[string]any{"all_day": "0"}},
	)
	check(t, "empty start_tz", got[1], want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-absent"}})
	check(t, "all_day NULL", got[2], want{timing: "unknown", raw: map[string]any{"timing_state": "all-day-undecided", "all_day": isNull{}}})
	check(t, "all-day without a zone", got[3], want{timing: "all_day", start: "2023-11-15T00:00:00", end: "2023-11-16T00:00:00", sdate: "2023-11-15", edate: "2023-11-16", raw: map[string]any{"timing_state": nil}})
	check(t, "zone of 256 bytes", got[4], want{timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-14T23:13:20Z"})
	check(t, "zone of 257 bytes", got[5], want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-too-long"}})
	check(t, "zone growing past 256 bytes as cleaned", got[6], want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-too-long"}})
	check(t, "all_day TEXT", got[7], want{timing: "unknown", raw: map[string]any{"timing_state": "all-day-undecided", "all_day": "0"}})
	if unknownOf(got[7])["CalendarItem.all_day"] != "type-mismatch" {
		t.Errorf("all_day TEXT: unknown_columns %v", unknownOf(got[7]))
	}
}

// Spec 5.2, B1 and B5 (CORE rows 3-5): an all-day start or end is a date only at an exact UTC midnight; off midnight
// there is no date and no time, stated, never a zone-shifted repair (a 23:59:59 end included).
func TestCalendarAllDayNotMidnightIsStatedNotRepaired(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	check(t, "row 3", recs[3], want{timing: "all_day", start: "2023-11-15T00:00:00", end: "2023-11-16T00:00:00", sdate: "2023-11-15", edate: "2023-11-16", raw: map[string]any{"end_state": nil}})
	check(t, "row 4", recs[4], want{timing: "all_day", raw: map[string]any{"start_state": "all-day-not-midnight", "end_state": "no-start", "start_date": int64(cM - cH)}})
	check(t, "row 5", recs[5], want{timing: "all_day", start: "2023-11-15T00:00:00", sdate: "2023-11-15", raw: map[string]any{"end_state": "all-day-not-midnight", "end_date": int64(cM + cDay - 1)}})
}

// Spec 5.3, 5.4 (CORE row 24): an end before the start is not stated; the stored value stays raw.
func TestCalendarEndBeforeStartIsNotStated(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	check(t, "row 24", recs[24], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "end-before-start", "end_date": int64(cD - 60)}})
	got := goRecords(t,
		item{1, map[string]any{"all_day": int64(1), "start_date": float64(cM), "end_date": float64(cM - cDay)}},
		item{2, map[string]any{"start_tz": "_float", "end_tz": "_float", "end_date": float64(cD - 1)}},
		item{3, map[string]any{"end_date": float64(cD)}},
	)
	check(t, "all-day end before start", got[1], want{timing: "all_day", start: "2023-11-15T00:00:00", sdate: "2023-11-15", raw: map[string]any{"end_state": "end-before-start"}})
	check(t, "floating end before start", got[2], want{timing: "floating", start: "2023-11-14T22:13:20", raw: map[string]any{"end_state": "end-before-start"}})
	check(t, "end at the start", got[3], want{timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": nil}})
}

// B5 (CORE rows 18, 26): no readable start means no TimeEnd and no end_date, raw.end_state "no-start", before every
// other end rule; a start with no stored end is "none-stored".
func TestIOSCalendarNoStartMeansNoEnd(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	check(t, "row 26", recs[26], want{timing: "timed", raw: map[string]any{"end_state": "no-start", "start_date": isNull{}, "end_date": int64(cD)}})
	check(t, "row 18", recs[18], want{timing: "timed", raw: map[string]any{"end_state": "no-start", "start_date_status": "sentinel"}})
	got := goRecords(t,
		item{1, map[string]any{"end_date": nil}},
		item{2, map[string]any{"start_date": nil, "end_tz": nil}},
	)
	check(t, "no end stored", got[1], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "none-stored", "end_date": isNull{}}})
	check(t, "no start and a mismatched end zone", got[2], want{timing: "timed", raw: map[string]any{"end_state": "no-start"}})
}

// Review Focus 5 (CORE row 14): two zones of one class (both timed) keep the end; both names as stored, never applied.
func TestIOSCalendarDifferentZonesOfOneClassKeepTheEnd(t *testing.T) {
	r := byRowid(t, parseVariant(t, "calendar-ios"))[14]
	check(t, "row 14", r, want{timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-15T06:13:20Z", raw: map[string]any{"end_state": nil}})
	if payloadStr(r, "start_timezone") != "America/New_York" || payloadStr(r, "end_timezone") != "Europe/London" {
		t.Errorf("row 14: zones %q, %q", payloadStr(r, "start_timezone"), payloadStr(r, "end_timezone"))
	}
}

// B6 (CORE row 15, Task 7 ruling) and B10: an end zone whose timing class differs from the start's gives no end,
// raw.end_state "timing-mismatch"; a NULL or empty end_tz on a timed event is such a mismatch (NULL is a stated
// absence); an end zone that cannot be read or is over 256 bytes is no class at all: "end-zone-undecided" (B10); an
// all-day record's end is all-day whatever its end_tz (B6 applies only when all_day is 0).
func TestIOSCalendarTimingMismatchHasNoEnd(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	check(t, "row 15", recs[15], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "timing-mismatch", "end_tz": "_float"}})
	got := goRecords(t,
		item{1, map[string]any{"end_tz": nil}},
		item{2, map[string]any{"end_tz": ""}},
		item{3, map[string]any{"start_tz": "_float", "end_tz": "Europe/Berlin"}},
		item{4, map[string]any{"all_day": int64(1), "start_date": float64(cM), "end_date": float64(cM + cDay), "end_tz": "Europe/Berlin"}},
		item{5, map[string]any{"end_tz": strings.Repeat("z", 257)}},
	)
	check(t, "timed, end_tz NULL", got[1], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "timing-mismatch"}})
	check(t, "timed, end_tz empty", got[2], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "timing-mismatch"}})
	check(t, "floating, zoned end", got[3], want{timing: "floating", start: "2023-11-14T22:13:20", raw: map[string]any{"end_state": "timing-mismatch"}})
	check(t, "all-day, zoned end", got[4], want{timing: "all_day", start: "2023-11-15T00:00:00", end: "2023-11-16T00:00:00", sdate: "2023-11-15", edate: "2023-11-16", raw: map[string]any{"end_state": nil}})
	check(t, "timed, end zone over 256 bytes", got[5], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "end-zone-undecided", "end_tz_state": "too-long"}})
}

// Spec 5.4 (CORE rows 1, 18, 19): Cocoa seconds are read as stored, REAL or INTEGER, never through the unit guess of
// CocoaAuto: a fraction is kept, a sentinel (0, -0, -1) and a value outside 1970-2100 (a nanosecond-magnitude value
// included) are no time with raw.<col>_status, the stored value in raw, never rescaled. created and modified are utc
// instants.
func TestIOSCalendarCocoaFormsAreReadNotGuessed(t *testing.T) {
	recs := byRowid(t, parseVariant(t, "calendar-ios"))
	check(t, "row 19", recs[19], want{timing: "timed", start: "2023-11-14T22:13:20.5Z", end: "2023-11-14T23:13:20Z", raw: map[string]any{"start_date": 721692800.5}})
	check(t, "row 18", recs[18], want{timing: "timed", raw: map[string]any{"start_date_status": "sentinel"}})
	created := map[string]string{}
	for _, nt := range recs[1].Times {
		created[nt.Kind] = fmtTime(&nt.Time)
	}
	if created["created"] != fmtTime(at(t, "2023-11-13T22:13:20Z")) || created["modified"] != fmtTime(at(t, "2023-11-14T21:13:20Z")) || len(created) != 2 {
		t.Errorf("row 1: Times %v", created)
	}

	const nanos = int64(721692800000000000)
	got := goRecords(t,
		item{1, map[string]any{"start_date": math.Copysign(0, -1)}},
		item{2, map[string]any{"start_date": int64(-1)}},
		item{3, map[string]any{"start_date": float64(-1)}},
		item{4, map[string]any{"start_date": nanos}},
		item{5, map[string]any{"start_date": int64(cD), "end_date": int64(cD + cH)}},
		item{6, map[string]any{"creation_date": int64(0), "last_modified": nanos}},
		item{7, map[string]any{"end_date": nanos}},
	)
	for id, st := range map[int64]string{1: "sentinel", 2: "sentinel", 3: "sentinel", 4: "invalid"} {
		check(t, "start "+st, got[id], want{timing: "timed", raw: map[string]any{"start_date_status": st, "end_state": "no-start"}})
	}
	if v := rawOf(got[4])["start_date"]; !sameValue(v, nanos) {
		t.Errorf("nanosecond-magnitude start: raw.start_date %#v, want %d as stored", v, nanos)
	}
	check(t, "integral seconds", got[5], want{timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-14T23:13:20Z", raw: map[string]any{"start_date": int64(cD)}})
	check(t, "sentinel and invalid stamps", got[6], want{
		timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-14T23:13:20Z",
		raw: map[string]any{"creation_date_status": "sentinel", "creation_date": int64(0), "last_modified_status": "invalid", "last_modified": nanos},
	})
	if len(got[6].Times) != 0 {
		t.Errorf("sentinel and invalid stamps: Times %v", got[6].Times)
	}
	if len(got[1].Times) != 0 || rawOf(got[1])["creation_date"] != nil {
		t.Errorf("NULL stamps: Times %v, raw %v (a NULL stamp states nothing)", got[1].Times, rawOf(got[1]))
	}
	check(t, "end not a time", got[7], want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_date_status": "invalid", "end_state": nil}})
}

// Spec 5.4, 4I2-A6 (iOS half): a NaN, a BLOB or a TEXT in a time or timing cell is undecided: listed in
// raw.unknown_columns, no time, never raw null, never a state that would claim an absence.
func TestCalendarUnreadableCellsAreUndecided(t *testing.T) {
	got := goRecords(t,
		item{1, map[string]any{"start_date": math.NaN()}},
		item{2, map[string]any{"start_tz": []byte("Europe/Berlin")}},
		item{3, map[string]any{"all_day": math.NaN()}},
		item{4, map[string]any{"end_date": math.NaN()}},
		item{5, map[string]any{"creation_date": math.NaN(), "last_modified": "yesterday"}},
		item{6, map[string]any{"start_date": "721692800"}},
		item{7, map[string]any{"start_tz": math.NaN()}},
		item{8, map[string]any{"end_tz": []byte("Europe/Berlin")}},
	)
	cases := []struct {
		id     int64
		col    string
		w      want
		absent []string
	}{
		{1, "start_date", want{timing: "timed", raw: map[string]any{"end_state": "no-start"}}, []string{"start_date"}},
		{2, "start_tz", want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-unreadable"}}, []string{"start_tz"}},
		{3, "all_day", want{timing: "unknown", raw: map[string]any{"timing_state": "all-day-undecided"}}, []string{"all_day"}},
		{4, "end_date", want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": nil}}, []string{"end_date"}},
		{5, "creation_date", want{timing: "timed", start: "2023-11-14T22:13:20Z", end: "2023-11-14T23:13:20Z"}, []string{"creation_date", "last_modified"}},
		{6, "start_date", want{timing: "timed", raw: map[string]any{"end_state": "no-start"}}, []string{"start_date"}},
		{7, "start_tz", want{timing: "unknown", raw: map[string]any{"timing_state": "start-tz-unreadable"}}, []string{"start_tz"}},
		{8, "end_tz", want{timing: "timed", start: "2023-11-14T22:13:20Z", raw: map[string]any{"end_state": "end-zone-undecided"}}, []string{"end_tz"}}, // B10
	}
	for _, c := range cases {
		r := got[c.id]
		check(t, c.col, r, c.w)
		if unknownOf(r)["CalendarItem."+c.col] != "type-mismatch" {
			t.Errorf("row %d (%s): unknown_columns %v", c.id, c.col, unknownOf(r))
		}
		for _, k := range c.absent {
			if v, has := rawOf(r)[k]; has {
				t.Errorf("row %d (%s): raw.%s = %#v: an unreadable cell is never a stored value or a NULL", c.id, c.col, k, v)
			}
		}
	}
	if unknownOf(got[5])["CalendarItem.last_modified"] != "type-mismatch" || len(got[5].Times) != 0 {
		t.Errorf("unreadable stamps: unknown_columns %v, Times %v", unknownOf(got[5]), got[5].Times)
	}
}
