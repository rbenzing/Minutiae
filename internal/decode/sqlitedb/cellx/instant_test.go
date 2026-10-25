package cellx_test

import (
	"math"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/ts"
)

var cocoa = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func ival(v int64) cellx.Number   { return cellx.Number{I: v, IsInt: true} }
func rval(v float64) cellx.Number { return cellx.Number{F: v} }

func TestCocoaAutoIntegerNeverPassesThroughAFloat(t *testing.T) {
	got := cellx.CocoaAuto(ival(700000000123456789))
	want := cocoa.Add(700000000*time.Second + 123456789)
	if !got.Valid() || got.Unit != ts.UnitNanoseconds || !got.Auto || !got.T.Equal(want) {
		t.Errorf("CocoaAuto = %+v, want valid nanoseconds at %v", got, want)
	}
	if got.Raw != ival(700000000123456789) {
		t.Errorf("Raw = %+v", got.Raw)
	}
}

func TestCocoaAutoUnits(t *testing.T) {
	tests := []struct {
		name   string
		n      cellx.Number
		status ts.Status
		unit   ts.Unit
		at     time.Time // checked only when valid
	}{
		{"0", ival(0), ts.StatusSentinel, ts.UnitNone, time.Time{}},
		{"-1", ival(-1), ts.StatusSentinel, ts.UnitNone, time.Time{}},
		{"MaxInt64", ival(math.MaxInt64), ts.StatusSentinel, ts.UnitNone, time.Time{}},
		{"MinInt64", ival(math.MinInt64), ts.StatusSentinel, ts.UnitNone, time.Time{}},
		{"-1e11 is nanoseconds (-100 s)", ival(-100_000_000_000), ts.StatusValid, ts.UnitNanoseconds, cocoa.Add(-100 * time.Second)},
		{"seconds", ival(700000000), ts.StatusValid, ts.UnitSeconds, cocoa.Add(700000000 * time.Second)},
		{"99999999999", ival(99_999_999_999), ts.StatusInvalid, ts.UnitSeconds, time.Time{}},
		{"year 2127", ival(4_000_000_000), ts.StatusInvalid, ts.UnitSeconds, time.Time{}},
		{"1e11 is nanoseconds (100 s)", ival(100_000_000_000), ts.StatusValid, ts.UnitNanoseconds, cocoa.Add(100 * time.Second)},
		{"real half second", rval(700000000.5), ts.StatusValid, ts.UnitSeconds, cocoa.Add(700000000*time.Second + 500*time.Millisecond)},
		{"real nanoseconds", rval(7e17), ts.StatusValid, ts.UnitNanoseconds, cocoa.Add(700000000 * time.Second)},
		{"-99999999999", ival(-99_999_999_999), ts.StatusInvalid, ts.UnitSeconds, time.Time{}},
		{"real 1e11 is nanoseconds", rval(1e11), ts.StatusValid, ts.UnitNanoseconds, cocoa.Add(100 * time.Second)},
		{"real 99999999999", rval(99_999_999_999), ts.StatusInvalid, ts.UnitSeconds, time.Time{}},
		{"real +Inf", rval(math.Inf(1)), ts.StatusInvalid, ts.UnitNone, time.Time{}},
		{"real NaN", rval(math.NaN()), ts.StatusInvalid, ts.UnitNone, time.Time{}},
	}
	for _, tc := range tests {
		got := cellx.CocoaAuto(tc.n)
		if got.Status != tc.status || got.Unit != tc.unit {
			t.Errorf("%s: status %v unit %v; want %v %v", tc.name, got.Status, got.Unit, tc.status, tc.unit)
			continue
		}
		if tc.status == ts.StatusValid {
			if !got.T.Equal(tc.at) {
				t.Errorf("%s: T = %v, want %v", tc.name, got.T, tc.at)
			}
		} else if !got.T.IsZero() || got.Valid() {
			t.Errorf("%s: a non-time carries T = %v", tc.name, got.T)
		}
	}
}

func TestCocoaSecondsAndUnixSeconds(t *testing.T) {
	tests := []struct {
		name   string
		got    cellx.Instant
		status ts.Status
		at     time.Time
	}{
		{"cocoa real 0", cellx.CocoaSeconds(rval(0)), ts.StatusSentinel, time.Time{}},
		{"cocoa integer", cellx.CocoaSeconds(ival(600000000)), ts.StatusValid, cocoa.Add(600000000 * time.Second)},
		{"cocoa integer 0", cellx.CocoaSeconds(ival(0)), ts.StatusSentinel, time.Time{}},
		{"unix integer", cellx.UnixSeconds(ival(1700000000)), ts.StatusValid, time.Unix(1700000000, 0).UTC()},
		{"unix whole real", cellx.UnixSeconds(rval(1700000000.0)), ts.StatusValid, time.Unix(1700000000, 0).UTC()},
		{"unix fractional real", cellx.UnixSeconds(rval(1700000000.5)), ts.StatusInvalid, time.Time{}},
		{"unix integer 0", cellx.UnixSeconds(ival(0)), ts.StatusSentinel, time.Time{}},
	}
	for _, tc := range tests {
		if tc.got.Status != tc.status || tc.got.Unit != ts.UnitNone || tc.got.Auto {
			t.Errorf("%s: %+v; want status %v, no unit, not auto", tc.name, tc.got, tc.status)
			continue
		}
		if tc.status == ts.StatusValid && !tc.got.T.Equal(tc.at) {
			t.Errorf("%s: T = %v, want %v", tc.name, tc.got.T, tc.at)
		}
		if tc.status != ts.StatusValid && !tc.got.T.IsZero() {
			t.Errorf("%s: a non-time carries T = %v", tc.name, tc.got.T)
		}
	}
}

func TestInstantRecordMarksUnitsAndNonTimes(t *testing.T) {
	has := func(m map[string]any, k string) bool { _, ok := m[k]; return ok }

	raw := map[string]any{}
	cellx.CocoaAuto(ival(700000000123456789)).Record(raw, "date")
	if raw["date"] != int64(700000000123456789) || raw["date_unit"] != "nanoseconds" || has(raw, "date_status") {
		t.Errorf("valid nanoseconds: %v", raw)
	}

	raw = map[string]any{}
	cellx.CocoaAuto(ival(0)).Record(raw, "date")
	if raw["date"] != int64(0) || raw["date_status"] != "sentinel" || has(raw, "date_unit") {
		t.Errorf("sentinel: %v", raw)
	}

	raw = map[string]any{}
	cellx.CocoaSeconds(ival(600000000)).Record(raw, "date")
	if raw["date"] != int64(600000000) || has(raw, "date_unit") || has(raw, "date_status") {
		t.Errorf("CocoaSeconds: %v", raw)
	}

	raw = map[string]any{}
	cellx.CocoaSeconds(rval(math.Inf(1))).Record(raw, "d")
	if _, has := raw["d"]; has || raw["d_ieee"] != "7ff0000000000000" || raw["d_status"] != "invalid" {
		t.Errorf("+Inf: %v", raw)
	}

	raw = map[string]any{}
	cellx.CocoaAuto(ival(99_999_999_999)).Record(raw, "date")
	if raw["date_unit"] != "seconds" || raw["date_status"] != "invalid" {
		t.Errorf("an invalid auto value keeps its unit: %v", raw)
	}
}

func TestInstantRecordStatesANonFiniteRealAsIEEEBits(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		hex  string
	}{
		{"+Inf", math.Inf(1), "7ff0000000000000"},
		{"-Inf", math.Inf(-1), "fff0000000000000"},
		{"NaN payload", math.Float64frombits(0x7ff8000000000001), "7ff8000000000001"},
		{"NaN with the smallest payload", math.Float64frombits(0x7ff0000000000001), "7ff0000000000001"},
	}
	for _, tc := range tests {
		for name, in := range map[string]cellx.Instant{
			"CocoaAuto": cellx.CocoaAuto(rval(tc.v)), "CocoaSeconds": cellx.CocoaSeconds(rval(tc.v)), "UnixSeconds": cellx.UnixSeconds(rval(tc.v)),
		} {
			raw := map[string]any{}
			in.Record(raw, "d")
			if len(raw) != 2 || raw["d_ieee"] != tc.hex || raw["d_status"] != "invalid" {
				t.Errorf("%s %s: raw = %v, want only d_ieee=%s and d_status=invalid", tc.name, name, raw, tc.hex)
			}
		}
	}
	// a finite REAL stays a plain number
	raw := map[string]any{}
	cellx.CocoaSeconds(rval(2.5)).Record(raw, "d")
	if raw["d"] != 2.5 || len(raw) != 1 {
		t.Errorf("finite REAL: %v", raw)
	}
}

func TestCocoaAutoBoundaryAgreesForIntegerAndReal(t *testing.T) {
	for _, v := range []int64{99_999_999_999, 100_000_000_000, -99_999_999_999, -100_000_000_000} {
		i, r := cellx.CocoaAuto(ival(v)), cellx.CocoaAuto(rval(float64(v)))
		if i.Unit != r.Unit || i.Status != r.Status || !i.T.Equal(r.T) {
			t.Errorf("%d: INTEGER %+v, REAL %+v", v, i, r)
		}
		_, unit, _ := ts.CocoaAuto(float64(v))
		if i.Unit != unit {
			t.Errorf("%d: cellx unit %v, ts unit %v", v, i.Unit, unit)
		}
	}
	if u := cellx.CocoaAuto(ival(99_999_999_999)).Unit; u != ts.UnitSeconds {
		t.Errorf("99999999999 unit %v, want seconds", u)
	}
	if u := cellx.CocoaAuto(rval(1e11)).Unit; u != ts.UnitNanoseconds {
		t.Errorf("REAL 1e11 unit %v, want nanoseconds", u)
	}
}
