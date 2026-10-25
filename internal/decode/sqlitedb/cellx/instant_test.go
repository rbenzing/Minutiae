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
	if raw["d"] != "+Inf" || raw["d_status"] != "invalid" {
		t.Errorf("+Inf: %v", raw)
	}

	raw = map[string]any{}
	cellx.CocoaAuto(ival(99_999_999_999)).Record(raw, "date")
	if raw["date_unit"] != "seconds" || raw["date_status"] != "invalid" {
		t.Errorf("an invalid auto value keeps its unit: %v", raw)
	}
}
