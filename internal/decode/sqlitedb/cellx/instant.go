package cellx

import (
	"math"
	"strconv"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/ts"
)

// autoBoundary is the one threshold of the unit decision (ruling 4E-R15), the same
// as ts.CocoaAuto's: an absolute value at or above it is nanoseconds.
const autoBoundary = 100_000_000_000

// Instant is a timestamp read through decode/ts: the time (zero unless Status is
// StatusValid), the status, the unit CocoaAuto chose, and the source value.
type Instant struct {
	T      time.Time
	Status ts.Status
	Unit   ts.Unit // UnitNone except for CocoaAuto
	Auto   bool    // made by CocoaAuto: Record writes <col>_unit
	Raw    Number
}

// Valid reports whether the value is a time.
func (i Instant) Valid() bool { return i.Status == ts.StatusValid }

// invalid is the result for a value that is not a time and not a sentinel.
func invalid(n Number, unit ts.Unit, auto bool) Instant {
	return Instant{Status: ts.StatusInvalid, Unit: unit, Auto: auto, Raw: n}
}

// CocoaAuto reads a Cocoa value of unknown unit (ruling 4E-R15): an INTEGER whose
// absolute value is at least 1e11 is nanoseconds and never passes through a float;
// a smaller one is seconds; a REAL goes through ts.CocoaAuto.
func CocoaAuto(n Number) Instant {
	if !n.IsInt {
		st, unit := ts.ClassifyAuto(n.F)
		out := Instant{Status: st, Unit: unit, Auto: true, Raw: n}
		if st != ts.StatusValid {
			return out
		}
		t, _, ok := ts.CocoaAuto(n.F)
		if !ok {
			// classified valid but not convertible: never a valid status with a zero time.
			return invalid(n, unit, true)
		}
		out.T = t
		return out
	}
	v := n.I
	if v >= autoBoundary || v <= -autoBoundary { // no negation: MinInt64 must not overflow
		return intInstant(n, ts.KindCocoaNanos, ts.UnitNanoseconds, true, ts.CocoaNanos)
	}
	return intInstant(n, ts.KindCocoaSeconds, ts.UnitSeconds, true, func(v int64) (time.Time, bool) {
		return ts.CocoaSeconds(float64(v))
	})
}

// intInstant classifies an INTEGER as kind and converts it with conv. A sentinel
// has no unit.
func intInstant(n Number, kind ts.Kind, unit ts.Unit, auto bool, conv func(int64) (time.Time, bool)) Instant {
	if !auto {
		unit = ts.UnitNone
	}
	st := ts.Classify(kind, n.I)
	switch st {
	case ts.StatusSentinel:
		return Instant{Status: st, Auto: auto, Raw: n}
	case ts.StatusValid:
		t, ok := conv(n.I)
		if !ok {
			// classified valid but not convertible: never a valid status with a zero time.
			return invalid(n, unit, auto)
		}
		return Instant{T: t, Status: st, Unit: unit, Auto: auto, Raw: n}
	}
	return invalid(n, unit, auto)
}

// CocoaSeconds reads seconds since 2001-01-01 (a REAL or an INTEGER).
func CocoaSeconds(n Number) Instant {
	if n.IsInt {
		return intInstant(n, ts.KindCocoaSeconds, ts.UnitNone, false, func(v int64) (time.Time, bool) {
			return ts.CocoaSeconds(float64(v))
		})
	}
	st := ts.ClassifyFloat(ts.KindCocoaSeconds, n.F)
	switch st {
	case ts.StatusSentinel:
		return Instant{Status: st, Raw: n}
	case ts.StatusValid:
		if t, ok := ts.CocoaSeconds(n.F); ok {
			return Instant{T: t, Status: st, Raw: n}
		}
	}
	return invalid(n, ts.UnitNone, false)
}

// UnixSeconds reads Unix seconds: an INTEGER, or a REAL that is an exact integer;
// a fractional REAL (or one beyond int64) is StatusInvalid.
func UnixSeconds(n Number) Instant {
	v := n.I
	if !n.IsInt {
		// ClassifyFloat refuses a fraction, NaN, an infinity and a value outside int64.
		if ts.ClassifyFloat(ts.KindUnixSeconds, n.F) == ts.StatusInvalid {
			return invalid(n, ts.UnitNone, false)
		}
		v = int64(n.F)
	}
	out := intInstant(Number{I: v, IsInt: true}, ts.KindUnixSeconds, ts.UnitNone, false, ts.UnixSeconds)
	out.Raw = n
	return out
}

// Record writes raw[col] = the stored value (a non-finite REAL is raw[col+"_ieee"], its IEEE bits in hex, instead), raw[col+"_status"] = "sentinel" or
// "invalid" when the value is not a time, and raw[col+"_unit"] when CocoaAuto
// chose a unit.
func (i Instant) Record(raw map[string]any, col string) {
	if !i.Raw.IsInt && (math.IsNaN(i.Raw.F) || math.IsInf(i.Raw.F, 0)) {
		// a non-finite REAL is stated as its 16 lowercase hex digits of IEEE-754 bits
		// (ruling P-4-1): lossless for NaN payloads, no raw[col] key
		bits := strconv.FormatUint(math.Float64bits(i.Raw.F), 16)
		raw[col+"_ieee"] = bits // a non-finite float always has 16 digits (exponent bits all ones)
	} else {
		raw[col] = i.Raw.Raw()
	}
	if i.Status != ts.StatusValid {
		raw[col+"_status"] = i.Status.String()
	}
	if i.Auto && i.Unit != ts.UnitNone {
		raw[col+"_unit"] = i.Unit.String()
	}
}
