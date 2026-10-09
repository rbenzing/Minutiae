package ts

import (
	"math"
	"testing"
)

// FB-1: ClassifyAuto and CocoaAuto agree with ClassifyFloat for every input, and the sentinel
// is told apart from NaN and the infinities.
func TestClassifyAutoAgreesWithClassifyFloat(t *testing.T) {
	values := []float64{
		0, math.Copysign(0, -1), -1, 1, -2, 0.5, 5e9, -2e9, 7.2e8, 3124137600, 99999999999.5,
		1e11 - 1, 1e11, 1e11 + 0.5, 1.5e11, 7.2e17, 5e18, -2e18, 1 << 62, math.Nextafter(1<<63, 0),
		1 << 63, -(1 << 63), math.Nextafter(1<<63, 1<<64), 1 << 64, -(1 << 64), 1e30, -1e30,
		math.NaN(), math.Inf(1), math.Inf(-1), math.MaxInt64, math.MinInt64,
	}
	for _, v := range values {
		st, unit := ClassifyAuto(v)
		var want Status
		var wantUnit Unit
		switch {
		case math.IsNaN(v) || math.IsInf(v, 0):
			want, wantUnit = StatusInvalid, UnitNone
		case math.Abs(v) < 1e11:
			want, wantUnit = ClassifyFloat(KindCocoaSeconds, v), UnitSeconds
		default:
			want, wantUnit = ClassifyFloat(KindCocoaNanos, v), UnitNanoseconds
		}
		if want == StatusSentinel {
			wantUnit = UnitNone
		}
		if st != want || unit != wantUnit {
			t.Errorf("ClassifyAuto(%v) = %v, %v; want %v, %v", v, st, unit, want, wantUnit)
		}
		tm, cu, ok := CocoaAuto(v)
		if cu != unit || ok != (st == StatusValid) || (!ok && !tm.IsZero()) || (ok && tm.IsZero()) {
			t.Errorf("CocoaAuto(%v) = %v, %v, %v disagrees with ClassifyAuto %v, %v", v, tm, cu, ok, st, unit)
		}
	}
	for _, tc := range []struct {
		v    float64
		st   Status
		unit Unit
	}{
		{math.NaN(), StatusInvalid, UnitNone},
		{math.Inf(1), StatusInvalid, UnitNone},
		{-1, StatusSentinel, UnitNone},
		{0, StatusSentinel, UnitNone},
		{1 << 63, StatusInvalid, UnitNanoseconds},
		{-(1 << 63), StatusSentinel, UnitNone},
	} {
		if st, unit := ClassifyAuto(tc.v); st != tc.st || unit != tc.unit {
			t.Errorf("ClassifyAuto(%v) = %v, %v; want %v, %v", tc.v, st, unit, tc.st, tc.unit)
		}
	}
}
