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

// ClassifyAuto reports what v is and the unit CocoaAuto reads it in. It is built from
// ClassifyFloat: NaN and the infinities are StatusInvalid with UnitNone; abs(v) < 1e11 is
// ClassifyFloat(KindCocoaSeconds, v) with UnitSeconds; any other value is
// ClassifyFloat(KindCocoaNanos, v) with UnitNanoseconds, so a float that does not fit int64
// after the unit decision (2^63 and above, a fraction) is StatusInvalid there as in
// ClassifyFloat. A sentinel (0, -0, -1 as seconds; -2^63, the float of MinInt64, as
// nanoseconds) is StatusSentinel with UnitNone: no time, no unit, and not invalid.
func ClassifyAuto(v float64) (Status, Unit) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return StatusInvalid, UnitNone
	}
	kind, unit := KindCocoaSeconds, UnitSeconds
	if math.Abs(v) >= autoBoundary {
		kind, unit = KindCocoaNanos, UnitNanoseconds
	}
	st := ClassifyFloat(kind, v)
	if st == StatusSentinel {
		unit = UnitNone
	}
	return st, unit
}

// CocoaAuto converts a Cocoa-epoch value (2001-01-01 UTC) whose unit is not known, as in
// iOS message.date across releases.
//
// The ONE rule: abs(v) >= 1e11 means nanoseconds, otherwise seconds. Seconds since 2001 stay
// below 3.2e9 until 2100 and nanosecond values are above 1e17 for any date after 2004, so
// nothing valid falls in the gap between them (a test walks the gap). The one exception is
// a genuine nanosecond count below 3.1e9 (a device clock within three seconds of the 2001
// default): it is read as seconds, which gives a plausible but wrong time (2e9 ns reads as
// the year 2064). The chosen unit is returned even when ok is false because the value is out
// of range, so the caller can still record it (raw.date_unit). For a sentinel and for NaN or
// an infinity the unit is UnitNone and ok is false; ClassifyAuto tells the two apart (and is
// the Status companion of this function: ok is true exactly when it reports StatusValid); no
// time is ever invented.
//
// A float64 has 53 bits of mantissa, so a nanosecond count of 7e17 is only exact to about
// 100 ns. A value with abs(v) >= 1e11 is converted through int64 only when it is an exact
// integer below 2^63; otherwise ok is false with UnitNanoseconds. A caller that reads an
// INTEGER column should call CocoaNanos directly and not pass the value through a float.
func CocoaAuto(v float64) (t time.Time, unit Unit, ok bool) {
	st, unit := ClassifyAuto(v)
	if st != StatusValid {
		return time.Time{}, unit, false
	}
	if unit == UnitSeconds {
		t, ok = CocoaSeconds(v)
	} else {
		t, ok = CocoaNanos(int64(v)) // valid: an exact integer inside int64
	}
	return t, unit, ok
}
