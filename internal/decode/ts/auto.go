package ts

import (
	"math"
	"time"
)

// Unit is the unit CocoaAuto read a value in.
type Unit uint8

// The units.
const (
	UnitNone        Unit = iota // no unit: a sentinel, NaN or an infinity
	UnitSeconds                 // seconds since 2001-01-01
	UnitNanoseconds             // nanoseconds since 2001-01-01
)

// String returns "seconds" or "nanoseconds", the strings a parser stores in raw.date_unit,
// and "" for UnitNone (and any unknown value).
func (u Unit) String() string {
	switch u {
	case UnitSeconds:
		return "seconds"
	case UnitNanoseconds:
		return "nanoseconds"
	}
	return ""
}

// autoBoundary is the one threshold of CocoaAuto.
const autoBoundary = 1e11

// CocoaAuto converts a Cocoa-epoch value (2001-01-01 UTC) whose unit is not known, as in
// iOS message.date across releases.
//
// The ONE rule: abs(v) >= 1e11 means nanoseconds, otherwise seconds. Seconds since 2001 stay
// below 3.2e9 until 2100 and nanosecond values are above 1e17 for any date after 2004, so
// nothing valid falls in the gap between them (a test walks the gap). The chosen unit is
// returned even when ok is false because the value is out of range, so the caller can still
// record it (raw.date_unit). For a sentinel (0, -0, -1, or the float of MaxInt64 or MinInt64)
// and for NaN or an infinity the unit is UnitNone and ok is false; no time is ever invented.
//
// A float64 has 53 bits of mantissa, so a nanosecond count of 7e17 is only exact to about
// 100 ns. A value with abs(v) >= 1e11 is converted through int64 only when it is an exact
// integer below 2^63; otherwise ok is false with UnitNanoseconds. A caller that reads an
// INTEGER column should call CocoaNanos directly and not pass the value through a float.
func CocoaAuto(v float64) (t time.Time, unit Unit, ok bool) {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return time.Time{}, UnitNone, false
	case v == 0 || v == -1 || v == math.MaxInt64 || v == math.MinInt64:
		return time.Time{}, UnitNone, false
	}
	if math.Abs(v) < autoBoundary {
		t, ok = CocoaSeconds(v)
		return t, UnitSeconds, ok
	}
	if v != math.Trunc(v) || v < -(1<<63) || v >= 1<<63 {
		return time.Time{}, UnitNanoseconds, false
	}
	t, ok = CocoaNanos(int64(v))
	return t, UnitNanoseconds, ok
}
