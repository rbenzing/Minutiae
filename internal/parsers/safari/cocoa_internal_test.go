package safari

import (
	"math"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func TestCocoaInstant(t *testing.T) {
	v := cocoa(704628000.5)
	if !v.Valid() || !v.T.Equal(time.Date(2023, 5, 1, 10, 0, 0, 500_000_000, time.UTC)) {
		t.Errorf("704628000.5 = %+v, want 2023-05-01T10:00:00.5Z (Cocoa 704628000 is 2023-05-01T10:00:00Z)", v)
	}
	raw := map[string]any{}
	v.Record(raw, "visit_time")
	if raw["visit_time"] != 704628000.5 || raw["visit_time_status"] != nil {
		t.Errorf("valid record %v", raw)
	}
	for _, tc := range []struct {
		name string
		in   float64
		want string
	}{
		{"zero", 0, "sentinel"}, {"negative zero", math.Copysign(0, -1), "sentinel"}, {"minus one", -1, "sentinel"}, {"after 2100", 4e9, "invalid"},
	} {
		c := cocoa(tc.in)
		r := map[string]any{}
		c.Record(r, "visit_time")
		if c.Valid() || !c.T.IsZero() || r["visit_time_status"] != tc.want {
			t.Errorf("%s: valid %v status %v", tc.name, c.Valid(), r)
		}
		if f, ok := r["visit_time"].(float64); !ok || math.Float64bits(f) != math.Float64bits(tc.in) {
			t.Errorf("%s: raw.visit_time = %v, want the stored value kept", tc.name, r["visit_time"])
		}
	}
	for _, tc := range []struct {
		name string
		in   float64
		hex  string
	}{
		{"+Inf", math.Inf(1), "7ff0000000000000"},
		{"-Inf", math.Inf(-1), "fff0000000000000"},
		{"NaN", math.Float64frombits(0x7ff8000000000001), "7ff8000000000001"},
	} {
		c := cocoa(tc.in)
		r := map[string]any{}
		c.Record(r, "end_time")
		if c.Valid() || r["end_time_ieee"] != tc.hex || r["end_time_status"] != "invalid" || r["end_time"] != nil || len(r) != 2 {
			t.Errorf("%s: %v", tc.name, r)
		}
	}
}

// fakeRow is a row of another implementation: the decoder's state is answered from IsNull only.
type fakeRow struct{ cells []any }

func (fakeRow) Table() string                          { return "t" }
func (fakeRow) Rowid() (int64, bool)                   { return 1, true }
func (r fakeRow) IsNull(c int) bool                    { return r.cells[c] == nil }
func (fakeRow) Int(int) (int64, bool)                  { return 0, false }
func (r fakeRow) Float(c int) (float64, bool)          { f, ok := r.cells[c].(float64); return f, ok }
func (fakeRow) Text(int) ([]byte, bool)                { return nil, false }
func (fakeRow) Blob(int) ([]byte, bool)                { return nil, false }
func (fakeRow) Range() (records.Range, parse.FileRole) { return records.Range{}, "" }

func TestFloatCell(t *testing.T) {
	row := fakeRow{cells: []any{nil, "text", []byte{1}, 2.5}}
	for _, tc := range []struct {
		col  int
		want rowrun.CellKind
		val  float64
	}{
		{0, rowrun.CellNull, 0}, {1, rowrun.CellOther, 0}, {2, rowrun.CellOther, 0}, {3, rowrun.CellValue, 2.5}, {-1, rowrun.CellMissing, 0},
	} {
		if v, k := floatCell(row, tc.col); k != tc.want || v != tc.val {
			t.Errorf("column %d = %v, %v; want %v, %v", tc.col, v, k, tc.val, tc.want)
		}
	}
}
