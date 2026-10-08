// Package ts converts the integer and floating point timestamps found in device artifacts
// to UTC instants, and says when a raw value must not become a time at all.
//
// Rule: a value converts only if it is not a sentinel for its kind and the instant lies in
// [Min(), Max()), that is 1970-01-01T00:00:00Z inclusive to 2100-01-01T00:00:00Z exclusive.
// The sentinels are 0, -1, math.MaxInt64 and math.MinInt64 for every integer kind, and for
// CocoaSeconds also 0, -0 and -1. Every value falls in exactly one of three classes (Status):
// valid (a time is returned), sentinel (no time; the caller leaves the record time unset and
// keeps the raw value) and invalid (outside the range, not finite, not an exact integer for an
// integer kind, or an unknown kind; no time, the caller marks ts_invalid).
//
// Every returned time has location UTC; the sub-second part is exact for the integer kinds and
// rounded to the nearest nanosecond for CocoaSeconds. Nothing is rescaled or guessed: a seconds
// value handed to UnixMillis is read as milliseconds. Plausibility floors belong to the parser.
// The raw value is never returned; callers keep it. Arithmetic is range-reduced before any
// multiplication, so no value wraps into a plausible date.
package ts

import (
	"math"
	"time"
)

// Kind names an epoch and unit.
type Kind uint8

// The supported kinds.
const (
	KindUnixSeconds Kind = iota
	KindUnixMillis
	KindUnixMicros
	KindUnixNanos
	KindCocoaSeconds
	KindCocoaNanos
	KindWebKitMicros
	KindFileTime
)

// Status is the class of a raw value.
type Status uint8

// The three classes.
const (
	StatusValid    Status = iota // converts to a time
	StatusSentinel               // a documented "no value" marker: no time
	StatusInvalid                // out of range, not finite, not an integer for an integer kind, unknown kind: no time
)

// String returns "valid", "sentinel" or "invalid".
func (s Status) String() string {
	switch s {
	case StatusValid:
		return "valid"
	case StatusSentinel:
		return "sentinel"
	}
	return "invalid"
}

// Epoch differences in seconds.
const (
	unixToCocoaSeconds   int64 = 978307200   // 1970-01-01 to 2001-01-01
	unixToWebKitSeconds  int64 = 11644473600 // 1601-01-01 to 1970-01-01
	maxUnixSeconds       int64 = 4102444800  // 2100-01-01T00:00:00Z, exclusive
	nanosPerSecond       int64 = 1_000_000_000
	microsPerSecond      int64 = 1_000_000
	millisPerSecond      int64 = 1_000
	hundredNanosPerSec   int64 = 10_000_000
	maxCocoaSecondsFloat       = 1e11 // anything beyond is far outside the range; avoids int conversion overflow
)

// Min is 1970-01-01T00:00:00Z, the first instant that converts.
func Min() time.Time { return time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC) }

// Max is 2100-01-01T00:00:00Z, the first instant that does not convert.
func Max() time.Time { return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) }

// floorDiv returns q and r with v = q*d + r and 0 <= r < d (d > 0).
func floorDiv(v, d int64) (q, r int64) {
	q, r = v/d, v%d
	if r < 0 {
		q--
		r += d
	}
	return q, r
}

func isIntSentinel(v int64) bool {
	return v == 0 || v == -1 || v == math.MaxInt64 || v == math.MinInt64
}

// build returns the instant for Unix seconds sec and nanoseconds nsec (0 <= nsec < 1e9).
func build(sec, nsec int64) (time.Time, bool) {
	if sec < 0 || sec >= maxUnixSeconds {
		return time.Time{}, false
	}
	return time.Unix(sec, nsec).UTC(), true
}

// convertInt converts an integer raw value of kind k.
func convertInt(k Kind, v int64) (time.Time, Status) {
	if k > KindFileTime {
		return time.Time{}, StatusInvalid
	}
	if isIntSentinel(v) {
		return time.Time{}, StatusSentinel
	}
	var sec, nsec int64
	switch k {
	case KindUnixSeconds:
		sec = v
	case KindUnixMillis:
		q, r := floorDiv(v, millisPerSecond)
		sec, nsec = q, r*1_000_000
	case KindUnixMicros:
		q, r := floorDiv(v, microsPerSecond)
		sec, nsec = q, r*1_000
	case KindUnixNanos:
		sec, nsec = floorDiv(v, nanosPerSecond)
	case KindCocoaSeconds:
		return convertFloat(float64(v))
	case KindCocoaNanos:
		q, r := floorDiv(v, nanosPerSecond)
		sec, nsec = q+unixToCocoaSeconds, r // |q| < 9.3e9: no overflow
	case KindWebKitMicros:
		q, r := floorDiv(v, microsPerSecond)
		sec, nsec = q-unixToWebKitSeconds, r*1_000
	case KindFileTime:
		q, r := floorDiv(v, hundredNanosPerSec)
		sec, nsec = q-unixToWebKitSeconds, r*100
	default:
		return time.Time{}, StatusInvalid
	}
	t, ok := build(sec, nsec)
	if !ok {
		return time.Time{}, StatusInvalid
	}
	return t, StatusValid
}

// convertFloat converts Cocoa seconds.
func convertFloat(v float64) (time.Time, Status) {
	switch {
	case !(v >= -maxCocoaSecondsFloat && v <= maxCocoaSecondsFloat):
		// Checked on the float before any conversion to int: NaN fails both comparisons, so it
		// is refused on every platform, as are the infinities and huge values.
		return time.Time{}, StatusInvalid
	case v == 0 || v == -1: // 0 also matches -0
		return time.Time{}, StatusSentinel
	}
	whole := math.Floor(v)
	ns := int64(math.Round((v - whole) * float64(nanosPerSecond)))
	sec := int64(whole) + unixToCocoaSeconds
	if ns >= nanosPerSecond {
		sec++
		ns = 0
	}
	t, ok := build(sec, ns)
	if !ok {
		return time.Time{}, StatusInvalid
	}
	return t, StatusValid
}

// Classify reports what raw, read as kind k, is. KindCocoaSeconds is read as the float
// of the same value.
func Classify(k Kind, raw int64) Status {
	_, st := convertInt(k, raw)
	return st
}

// ClassifyFloat is Classify for a float raw value. KindCocoaSeconds is converted directly;
// for an integer kind the float must be an exact integer that fits int64, else it is invalid.
func ClassifyFloat(k Kind, raw float64) Status {
	if k == KindCocoaSeconds {
		_, st := convertFloat(raw)
		return st
	}
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw != math.Trunc(raw) ||
		raw < -(1<<63) || raw >= 1<<63 {
		return StatusInvalid
	}
	return Classify(k, int64(raw))
}

func result(t time.Time, st Status) (time.Time, bool) {
	if st != StatusValid {
		return time.Time{}, false
	}
	return t, true
}

// UnixSeconds converts seconds since 1970-01-01 UTC.
func UnixSeconds(v int64) (time.Time, bool) { return result(convertInt(KindUnixSeconds, v)) }

// UnixMillis converts milliseconds since 1970-01-01 UTC.
func UnixMillis(v int64) (time.Time, bool) { return result(convertInt(KindUnixMillis, v)) }

// UnixMicros converts microseconds since 1970-01-01 UTC.
func UnixMicros(v int64) (time.Time, bool) { return result(convertInt(KindUnixMicros, v)) }

// UnixNanos converts nanoseconds since 1970-01-01 UTC.
func UnixNanos(v int64) (time.Time, bool) { return result(convertInt(KindUnixNanos, v)) }

// CocoaSeconds converts seconds since 2001-01-01 UTC, rounded to the nearest nanosecond.
func CocoaSeconds(v float64) (time.Time, bool) { return result(convertFloat(v)) }

// CocoaNanos converts nanoseconds since 2001-01-01 UTC.
func CocoaNanos(v int64) (time.Time, bool) { return result(convertInt(KindCocoaNanos, v)) }

// WebKitMicros converts microseconds since 1601-01-01 UTC.
func WebKitMicros(v int64) (time.Time, bool) { return result(convertInt(KindWebKitMicros, v)) }

// FileTime converts 100 ns ticks since 1601-01-01 UTC.
func FileTime(v int64) (time.Time, bool) { return result(convertInt(KindFileTime, v)) }
