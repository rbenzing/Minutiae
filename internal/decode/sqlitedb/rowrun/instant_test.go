package rowrun_test

import (
	"math"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
)

func TestInstantStatuses(t *testing.T) {
	cases := []struct {
		name  string
		got   rowrun.Instant
		st    ts.Status
		valid string
	}{
		{"millis", rowrun.Millis(1682935200000), ts.StatusValid, "2023-05-01T10:00:00Z"},
		{"millis zero", rowrun.Millis(0), ts.StatusSentinel, ""},
		{"millis -1", rowrun.Millis(-1), ts.StatusSentinel, ""},
		{"millis max", rowrun.Millis(math.MaxInt64), ts.StatusSentinel, ""},
		{"millis 2100", rowrun.Millis(4102444800000), ts.StatusInvalid, ""},
		{"millis -5", rowrun.Millis(-5), ts.StatusInvalid, ""},
		{"1973 guard: seconds in a millis column", rowrun.MillisFrom1973(1700000000), ts.StatusInvalid, ""},
		{"1973 guard: just below 1e11", rowrun.MillisFrom1973(99_999_999_999), ts.StatusInvalid, ""},
		{"1973 guard: 1e11", rowrun.MillisFrom1973(100_000_000_000), ts.StatusValid, "1973-03-03T09:46:40Z"},
		{"1973 guard keeps sentinels", rowrun.MillisFrom1973(0), ts.StatusSentinel, ""},
		{"seconds", rowrun.Seconds(1700000000), ts.StatusValid, "2023-11-14T22:13:20Z"},
		{"seconds zero", rowrun.Seconds(0), ts.StatusSentinel, ""},
	}
	for _, c := range cases {
		if c.got.Status != c.st || c.got.Valid() != (c.st == ts.StatusValid) {
			t.Errorf("%s: status %v, want %v", c.name, c.got.Status, c.st)
			continue
		}
		if c.valid != "" {
			want, _ := time.Parse(time.RFC3339, c.valid)
			if !c.got.T.Equal(want) || c.got.T.Location() != time.UTC {
				t.Errorf("%s: T = %v, want %v UTC", c.name, c.got.T, want)
			}
		} else if !c.got.T.IsZero() {
			t.Errorf("%s: an invalid or sentinel value carries the time %v", c.name, c.got.T)
		}
	}
}

func TestInstantRecordKeepsTheRawValueAndMarksNonTimes(t *testing.T) {
	raw := map[string]any{}
	rowrun.Millis(1682935200000).Record(raw, "date")
	rowrun.Millis(0).Record(raw, "date_sent")
	rowrun.MillisFrom1973(1700000000).Record(raw, "created")
	want := map[string]any{
		"date": int64(1682935200000), "date_sent": int64(0), "date_sent_status": "sentinel",
		"created": int64(1700000000), "created_status": "invalid",
	}
	if len(raw) != len(want) {
		t.Fatalf("raw = %v, want %v", raw, want)
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("raw[%s] = %v, want %v", k, raw[k], v)
		}
	}
}
