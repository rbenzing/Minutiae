package ts

import (
	"math"
	"testing"
	"time"
)

// ts has no public entry that recovers from a panic: its functions are total arithmetic
// (range-reduced before every multiplication), so there is no NoRecover twin. This fuzz
// target is the proof: no kind, raw integer or float may panic or return a time out of bounds.
func FuzzTS(f *testing.F) {
	for _, i := range []int64{0, -1, 1, math.MaxInt64, math.MinInt64, 978307200, 4102444800, 1e11, 7216928000000000000 / 10} {
		f.Add(i, float64(i), uint8(0))
		f.Add(i, float64(i), uint8(4))
		f.Add(i, float64(i), uint8(7))
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), 99999999999.0, 1e11, 1e19, 721692800.5} {
		f.Add(int64(1), v, uint8(4))
		f.Add(int64(1), v, uint8(5))
	}
	f.Add(int64(3), float64(0), uint8(200))
	// Epoch edges of every integer kind: the raw values one below, at and one above the first
	// valid instant (1970-01-01) and the first invalid one (2100-01-01), per the oracle's units.
	for kind, u := range map[uint8]struct{ per, off int64 }{
		0: {1, 0}, 1: {1_000, 0}, 2: {1_000_000, 0}, 3: {1_000_000_000, 0},
		5: {1_000_000_000, 978307200}, 6: {1_000_000, -11644473600}, 7: {10_000_000, -11644473600},
	} {
		lo, hi := -u.off*u.per, (4102444800-u.off)*u.per
		for _, v := range []int64{lo - 1, lo, lo + 1, hi - 1, hi, hi + 1} {
			f.Add(v, float64(v), kind)
		}
	}
	for _, v := range []float64{
		-978307201, -978307200, -978307199, 3124137599, 3124137600, 3124137601,
		math.Nextafter(-978307200, 0), math.Nextafter(3124137600, 0), -0.5, 0.5,
	} {
		f.Add(int64(1), v, uint8(4))
	}
	f.Fuzz(func(t *testing.T, i int64, v float64, kind uint8) {
		k := Kind(kind)
		inBounds := func(name string, tm time.Time, ok bool) {
			if !ok {
				return
			}
			if tm.Location() != time.UTC || tm.Before(Min()) || !tm.Before(Max()) {
				t.Fatalf("%s(%d, %v): time %v out of bounds", name, i, v, tm)
			}
		}
		for _, c := range []struct {
			name string
			f    func() (time.Time, bool)
		}{
			{"UnixSeconds", func() (time.Time, bool) { return UnixSeconds(i) }},
			{"UnixMillis", func() (time.Time, bool) { return UnixMillis(i) }},
			{"UnixMicros", func() (time.Time, bool) { return UnixMicros(i) }},
			{"UnixNanos", func() (time.Time, bool) { return UnixNanos(i) }},
			{"CocoaSeconds", func() (time.Time, bool) { return CocoaSeconds(v) }},
			{"CocoaNanos", func() (time.Time, bool) { return CocoaNanos(i) }},
			{"WebKitMicros", func() (time.Time, bool) { return WebKitMicros(i) }},
			{"FileTime", func() (time.Time, bool) { return FileTime(i) }},
		} {
			tm, ok := c.f()
			inBounds(c.name, tm, ok)
		}
		// Classify against the documented rule, computed directly (not through convertInt).
		st := Classify(k, i)
		if want, known := expectedStatus(k, i); known && st != want {
			t.Fatalf("Classify(%d, %d) = %v, want %v", k, i, st, want)
		}
		if k == KindCocoaSeconds {
			fs := ClassifyFloat(k, v)
			shifted := v + float64(unixToCocoaSeconds)
			finite := v >= -1e11 && v <= 1e11 // false for NaN
			inRange := shifted >= float64(Min().Unix()) && shifted < float64(Max().Unix())
			if fs == StatusValid && !inRange {
				t.Fatalf("ClassifyFloat(%v) valid outside the range", v)
			}
			if finite && inRange && v != 0 && v != -1 && fs != StatusValid {
				t.Fatalf("ClassifyFloat(%v) = %v inside the range, want valid", v, fs)
			}
			if (v == 0 || v == -1) && fs != StatusSentinel {
				t.Fatalf("ClassifyFloat(%v) = %v, want sentinel", v, fs)
			}
			if (math.IsNaN(v) || math.IsInf(v, 0)) && fs != StatusInvalid {
				t.Fatalf("ClassifyFloat(%v) = %v, want invalid", v, fs)
			}
		}
		_ = ClassifyFloat(k, v)
		at, unit, aok := CocoaAuto(v)
		inBounds("CocoaAuto", at, aok)
		if aok && unit == UnitNone {
			t.Fatalf("CocoaAuto(%v) ok with no unit", v)
		}
		ast, aunit := ClassifyAuto(v)
		if aunit != unit || aok != (ast == StatusValid) {
			t.Fatalf("CocoaAuto(%v) = %v %v but ClassifyAuto = %v %v", v, unit, aok, ast, aunit)
		}
		sentinel := v == 0 || v == -1 || v == math.MinInt64
		switch {
		case math.IsNaN(v) || math.IsInf(v, 0) || sentinel:
			if unit != UnitNone || aok {
				t.Fatalf("CocoaAuto(%v) = %v %v, want no unit and not ok", v, unit, aok)
			}
		case math.Abs(v) >= 1e11:
			if unit != UnitNanoseconds {
				t.Fatalf("CocoaAuto(%v) unit %v, want nanoseconds", v, unit)
			}
		default:
			if unit != UnitSeconds {
				t.Fatalf("CocoaAuto(%v) unit %v, want seconds", v, unit)
			}
		}
		if !aok && !at.IsZero() {
			t.Fatalf("CocoaAuto(%v) not ok but returned %v", v, at)
		}
	})
}

// expectedStatus is the documented rule for an integer raw value, written without the
// package's converters: sentinels first, then the instant (floor-divided to whole Unix
// seconds) must lie in [Min, Max). known is false for kinds this oracle does not cover.
func expectedStatus(k Kind, v int64) (Status, bool) {
	var per, off int64
	switch k {
	case KindUnixSeconds:
		per, off = 1, 0
	case KindUnixMillis:
		per, off = 1_000, 0
	case KindUnixMicros:
		per, off = 1_000_000, 0
	case KindUnixNanos:
		per, off = 1_000_000_000, 0
	case KindCocoaNanos:
		per, off = 1_000_000_000, 978307200
	case KindWebKitMicros:
		per, off = 1_000_000, -11644473600
	case KindFileTime:
		per, off = 10_000_000, -11644473600
	default:
		return StatusInvalid, k > KindFileTime
	}
	if v == 0 || v == -1 || v == math.MaxInt64 || v == math.MinInt64 {
		return StatusSentinel, true
	}
	q := v / per
	if v%per < 0 {
		q--
	}
	if sec := q + off; sec >= 0 && sec < 4102444800 { // 1970-01-01 up to 2100-01-01 exclusive
		return StatusValid, true
	}
	return StatusInvalid, true
}
