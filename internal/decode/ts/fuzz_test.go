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
		// Classify agrees with the converter of the same kind.
		st := Classify(k, i)
		var tm time.Time
		var ok bool
		switch k {
		case KindUnixSeconds:
			tm, ok = UnixSeconds(i)
		case KindUnixMillis:
			tm, ok = UnixMillis(i)
		case KindUnixMicros:
			tm, ok = UnixMicros(i)
		case KindUnixNanos:
			tm, ok = UnixNanos(i)
		case KindCocoaSeconds:
			tm, ok = CocoaSeconds(float64(i))
		case KindCocoaNanos:
			tm, ok = CocoaNanos(i)
		case KindWebKitMicros:
			tm, ok = WebKitMicros(i)
		case KindFileTime:
			tm, ok = FileTime(i)
		}
		_ = tm
		if (st == StatusValid) != ok {
			t.Fatalf("Classify(%d, %d) = %v but converter ok = %v", k, i, st, ok)
		}
		if k == KindCocoaSeconds {
			_, okf := CocoaSeconds(v)
			if (ClassifyFloat(k, v) == StatusValid) != okf {
				t.Fatalf("ClassifyFloat(%v) disagrees with CocoaSeconds", v)
			}
		}
		_ = ClassifyFloat(k, v)
		at, unit, aok := CocoaAuto(v)
		inBounds("CocoaAuto", at, aok)
		if aok && unit == UnitNone {
			t.Fatalf("CocoaAuto(%v) ok with no unit", v)
		}
		if !aok && !at.IsZero() {
			t.Fatalf("CocoaAuto(%v) not ok but returned %v", v, at)
		}
	})
}
