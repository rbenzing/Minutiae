package rowrun

import (
	"time"

	"github.com/rbenzing/minutiae/internal/decode/ts"
)

// Instant is a timestamp column read through decode/ts: the time (zero unless
// Status is StatusValid), the status and the source value.
type Instant struct {
	T      time.Time
	Status ts.Status
	Raw    int64
}

// Valid reports whether the value is a time.
func (i Instant) Valid() bool { return i.Status == ts.StatusValid }

// Millis reads Unix milliseconds.
func Millis(raw int64) Instant { return instant(ts.KindUnixMillis, raw, false) }

// MillisFrom1973 reads Unix milliseconds and also refuses a value below 1e11
// (before 1973-03-03): a seconds value in a milliseconds column is invalid, not
// guessed (spec 9.1).
func MillisFrom1973(raw int64) Instant { return instant(ts.KindUnixMillis, raw, true) }

// Seconds reads Unix seconds.
func Seconds(raw int64) Instant { return instant(ts.KindUnixSeconds, raw, false) }

func instant(k ts.Kind, raw int64, guard bool) Instant {
	st := ts.Classify(k, raw)
	i := Instant{Status: st, Raw: raw}
	if st != ts.StatusValid {
		return i
	}
	if guard && raw < 100_000_000_000 {
		return Instant{Status: ts.StatusInvalid, Raw: raw}
	}
	var ok bool
	switch k {
	case ts.KindUnixMillis:
		i.T, ok = ts.UnixMillis(raw)
	default:
		i.T, ok = ts.UnixSeconds(raw)
	}
	if !ok {
		// classified valid but not convertible: never a valid status with a zero time.
		// This defends against a future divergence between Classify and the converters in
		// the ts package; it is unreachable today and no test claims it.
		return Instant{Status: ts.StatusInvalid, Raw: raw}
	}
	return i
}

// Record stores the source value in raw under col and, when it is not a time,
// its status under col+"_status" ("sentinel" or "invalid"): the marker of spec
// 7.5 (D5). A valid time has no marker.
func (i Instant) Record(raw map[string]any, col string) {
	raw[col] = i.Raw
	if i.Status != ts.StatusValid {
		raw[col+"_status"] = i.Status.String()
	}
}
