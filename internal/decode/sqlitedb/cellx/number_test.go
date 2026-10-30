package cellx_test

import (
	"math"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
)

func TestNumberCellKinds(t *testing.T) {
	row := fakeRow{cells: []cell{
		{kind: "int", i: 5}, {kind: "real", f: 1.5}, {kind: "null"}, {kind: "text"}, {kind: "blob"},
	}}
	tests := []struct {
		name string
		col  int
		want cellx.Number
		kind rowrun.CellKind
	}{
		{"integer", 0, cellx.Number{I: 5, IsInt: true}, rowrun.CellValue},
		{"real", 1, cellx.Number{F: 1.5}, rowrun.CellValue},
		{"null", 2, cellx.Number{}, rowrun.CellNull},
		{"text", 3, cellx.Number{}, rowrun.CellOther},
		{"blob", 4, cellx.Number{}, rowrun.CellOther},
		{"missing column", -1, cellx.Number{}, rowrun.CellMissing},
	}
	for _, tc := range tests {
		got, kind := cellx.NumberCell(row, tc.col)
		if got != tc.want || kind != tc.kind {
			t.Errorf("%s: NumberCell = %+v, %d; want %+v, %d", tc.name, got, kind, tc.want, tc.kind)
		}
	}
	if got, kind := cellx.NumberCell(unreadRow(t), 2); kind != rowrun.CellUnknown || got != (cellx.Number{}) {
		t.Errorf("an unread column: NumberCell = %+v, %d; want CellUnknown", got, kind)
	}
}

func TestNumberFloatIsExactOrRefused(t *testing.T) {
	tests := []struct {
		name string
		n    cellx.Number
		want float64
		ok   bool
	}{
		{"2^53", cellx.Number{I: 1 << 53, IsInt: true}, 9007199254740992, true},
		{"-2^53", cellx.Number{I: -(1 << 53), IsInt: true}, -9007199254740992, true},
		{"2^53+1", cellx.Number{I: 1<<53 + 1, IsInt: true}, 0, false},
		{"-2^53-1", cellx.Number{I: -(1<<53 + 1), IsInt: true}, 0, false},
		{"MaxInt64", cellx.Number{I: math.MaxInt64, IsInt: true}, 0, false},
		{"MinInt64", cellx.Number{I: math.MinInt64, IsInt: true}, 0, false},
		{"real", cellx.Number{F: 1.25}, 1.25, true},
	}
	for _, tc := range tests {
		got, ok := tc.n.Float()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: Float = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNumberRawKeepsTheStoredValue(t *testing.T) {
	tests := []struct {
		name string
		n    cellx.Number
		want any
	}{
		{"integer", cellx.Number{I: 7, IsInt: true}, int64(7)},
		{"real", cellx.Number{F: 2.5}, float64(2.5)},
		{"+Inf", cellx.Number{F: math.Inf(1)}, "+Inf"},
		{"-Inf", cellx.Number{F: math.Inf(-1)}, "-Inf"},
		{"NaN", cellx.Number{F: math.NaN()}, "NaN"},
	}
	for _, tc := range tests {
		if got := tc.n.Raw(); got != tc.want {
			t.Errorf("%s: Raw = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}
