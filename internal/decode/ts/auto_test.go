package ts

import (
	"math"
	"testing"
	"time"
)

func TestCocoaAutoBoundary(t *testing.T) {
	// Just below the boundary: seconds, 99999999999 s after 2001 is far past 2100.
	_, unit, ok := CocoaAuto(99999999999.0)
	if unit != UnitSeconds || ok {
		t.Fatalf("99999999999: unit %v ok %v, want seconds, not ok", unit, ok)
	}
	// At the boundary: nanoseconds, 1e11 ns = 100 s after 2001.
	got, unit, ok := CocoaAuto(1e11)
	want := time.Date(2001, 1, 1, 0, 1, 40, 0, time.UTC)
	if unit != UnitNanoseconds || !ok || !got.Equal(want) {
		t.Fatalf("1e11: %v %v %v, want %v nanoseconds ok", got, unit, ok, want)
	}
	// Negative mirror: -99999999999 is seconds (out of range), -1e11 is nanoseconds, 1970 + ... = 2000-12-31T23:58:20Z.
	if _, unit, ok = CocoaAuto(-99999999999.0); unit != UnitSeconds || ok {
		t.Fatalf("-99999999999: unit %v ok %v", unit, ok)
	}
	got, unit, ok = CocoaAuto(-1e11)
	want = time.Date(2000, 12, 31, 23, 58, 20, 0, time.UTC)
	if unit != UnitNanoseconds || !ok || !got.Equal(want) {
		t.Fatalf("-1e11: %v %v %v, want %v", got, unit, ok, want)
	}
	// A realistic nanosecond value, 2023-11-14T22:13:20Z is 721692800 s after 2001.
	got, unit, ok = CocoaAuto(7.216928e17)
	want = time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	if unit != UnitNanoseconds || !ok || got.Sub(want).Abs() > time.Microsecond {
		t.Fatalf("7.216928e17: %v %v %v, want %v", got, unit, ok, want)
	}
	// A realistic seconds value.
	got, unit, ok = CocoaAuto(721692800.5)
	want = time.Date(2023, 11, 14, 22, 13, 20, 500_000_000, time.UTC)
	if unit != UnitSeconds || !ok || !got.Equal(want) {
		t.Fatalf("721692800.5: %v %v %v, want %v", got, unit, ok, want)
	}
}

func TestCocoaAutoUnitReturnedWhenOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		unit Unit
	}{
		{5e9, UnitSeconds},              // 2159
		{-2e9, UnitSeconds},             // before 1970
		{5e18, UnitNanoseconds},         // after 2100
		{-2e18, UnitNanoseconds},        // before 1970
		{1.5e11 + 0.5, UnitNanoseconds}, // not an integer
		{1e19, UnitNanoseconds},         // beyond int64
		{-1e19, UnitNanoseconds},
	} {
		tm, unit, ok := CocoaAuto(tc.v)
		if ok || unit != tc.unit || !tm.IsZero() {
			t.Errorf("CocoaAuto(%v) = %v %v %v, want zero, %v, not ok", tc.v, tm, unit, ok, tc.unit)
		}
	}
}

func TestCocoaAutoSentinels(t *testing.T) {
	for _, v := range []float64{
		0, math.Copysign(0, -1), -1, math.NaN(), math.Inf(1), math.Inf(-1),
		math.MaxInt64, math.MinInt64,
	} { // float64(MaxInt64) is 2^63, a sentinel of integer columns
		tm, unit, ok := CocoaAuto(v)
		if ok || unit != UnitNone || !tm.IsZero() {
			t.Errorf("CocoaAuto(%v) = %v %v %v, want zero, none, not ok", v, tm, unit, ok)
		}
	}
}

func TestCocoaAutoGapIsUnambiguous(t *testing.T) {
	secLimit := float64(Max().Unix() - unixToCocoaSeconds) // exclusive, as Max is
	secFloor := -float64(unixToCocoaSeconds)               // inclusive, as Min is
	const n = 5000
	for i := 0; i < n; i++ {
		mag := math.Pow(10, -3+22*float64(i)/float64(n-1)) // 1e-3 .. 1e19
		for _, v := range []float64{mag, -mag} {
			tm, unit, ok := CocoaAuto(v)
			if tm.Before(Min()) && ok || ok && !tm.Before(Max()) || ok && tm.Location() != time.UTC {
				t.Fatalf("%v: ok time %v out of bounds", v, tm)
			}
			if !ok {
				continue
			}
			switch unit {
			case UnitSeconds:
				if v >= secLimit || v < secFloor {
					t.Fatalf("%v ok as seconds beyond the seconds range", v)
				}
			case UnitNanoseconds:
				if math.Abs(v) < 1e11 {
					t.Fatalf("%v ok as nanoseconds below the boundary", v)
				}
			default:
				t.Fatalf("%v ok with unit %v", v, unit)
			}
			if a := math.Abs(v); a >= secLimit && a < 1e11 {
				t.Fatalf("%v is in the gap and must never be ok", v)
			}
		}
	}
}

func TestUnitString(t *testing.T) {
	for u, want := range map[Unit]string{UnitNone: "", UnitSeconds: "seconds", UnitNanoseconds: "nanoseconds", Unit(9): ""} {
		if got := u.String(); got != want {
			t.Errorf("Unit(%d) = %q, want %q", u, got, want)
		}
	}
}

// The int64-range guard of CocoaAuto: an integral float at or beyond 2^63 must be refused as
// nanoseconds without a float-to-int conversion (2^63 itself is the MaxInt64 sentinel float).
func TestCocoaAutoRefusesFloatsBeyondInt64(t *testing.T) {
	for _, v := range []float64{1 << 64, -(1 << 64), 1e30, -1e30, math.Nextafter(1<<63, 1<<64)} {
		tm, unit, ok := CocoaAuto(v)
		if ok || !tm.IsZero() || unit != UnitNanoseconds {
			t.Errorf("CocoaAuto(%v) = %v %v %v, want zero, nanoseconds, not ok", v, tm, unit, ok)
		}
	}
	if _, unit, ok := CocoaAuto(float64(1 << 62)); ok || unit != UnitNanoseconds {
		t.Errorf("2^62: unit %v ok %v", unit, ok)
	}
}
