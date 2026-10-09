package ts

import (
	"math"
	"testing"
	"time"
)

var allKinds = []Kind{
	KindUnixSeconds, KindUnixMillis, KindUnixMicros, KindUnixNanos,
	KindCocoaSeconds, KindCocoaNanos, KindWebKitMicros, KindFileTime,
}

// convInt dispatches an integer raw value to the spec converter of kind k.
func convInt(k Kind, v int64) (time.Time, bool) {
	switch k {
	case KindUnixSeconds:
		return UnixSeconds(v)
	case KindUnixMillis:
		return UnixMillis(v)
	case KindUnixMicros:
		return UnixMicros(v)
	case KindUnixNanos:
		return UnixNanos(v)
	case KindCocoaSeconds:
		return CocoaSeconds(float64(v))
	case KindCocoaNanos:
		return CocoaNanos(v)
	case KindWebKitMicros:
		return WebKitMicros(v)
	case KindFileTime:
		return FileTime(v)
	}
	return time.Time{}, false
}

func TestSentinelTimesAreNoTime(t *testing.T) {
	for _, k := range allKinds {
		for _, raw := range []int64{0, -1, math.MaxInt64, math.MinInt64} {
			tm, ok := convInt(k, raw)
			if ok || !tm.IsZero() {
				t.Errorf("kind %d raw %d: got %v ok=%v, want no time", k, raw, tm, ok)
			}
			if got := Classify(k, raw); got != StatusSentinel {
				t.Errorf("Classify(%d, %d) = %v, want sentinel", k, raw, got)
			}
		}
	}
	for _, f := range []float64{0, math.Copysign(0, -1), -1} {
		if tm, ok := CocoaSeconds(f); ok || !tm.IsZero() {
			t.Errorf("CocoaSeconds(%v) = %v, %v", f, tm, ok)
		}
		if got := ClassifyFloat(KindCocoaSeconds, f); got != StatusSentinel {
			t.Errorf("ClassifyFloat(%v) = %v, want sentinel", f, got)
		}
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if tm, ok := CocoaSeconds(f); ok || !tm.IsZero() {
			t.Errorf("CocoaSeconds(%v) = %v, %v", f, tm, ok)
		}
		if got := ClassifyFloat(KindCocoaSeconds, f); got != StatusInvalid {
			t.Errorf("ClassifyFloat(%v) = %v, want invalid", f, got)
		}
	}
}

func TestStatusString(t *testing.T) {
	for s, want := range map[Status]string{StatusValid: "valid", StatusSentinel: "sentinel", StatusInvalid: "invalid", Status(200): "invalid"} {
		if got := s.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestTimestampConversions(t *testing.T) {
	// 2023-11-14T22:13:20Z is Unix second 1700000000 (19675 days x 86400 + 80000 s).
	nov := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	// WebKit 13414166000000000 us = 13414166000 s; minus 11644473600 s = Unix 1769692400 =
	// 1700000000 + 69692400 s = 806 days 15:00:00 after the instant above = 2026-01-29T13:13:20Z.
	// FileTime 133e15 x 100 ns = 13300000000 s; minus 11644473600 = Unix 1655526400 = 2022-06-18T04:26:40Z
	// (2022-06-18T00:00:00Z is 1655510400, plus 16000 s).
	for _, tc := range []struct {
		name string
		got  func() (time.Time, bool)
		want time.Time
	}{
		{"UnixSeconds", func() (time.Time, bool) { return UnixSeconds(1700000000) }, nov},
		{"UnixMillis", func() (time.Time, bool) { return UnixMillis(1700000000000) }, nov},
		{"UnixMicros", func() (time.Time, bool) { return UnixMicros(1700000000123456) }, nov.Add(123456 * time.Microsecond)},
		{"UnixNanos", func() (time.Time, bool) { return UnixNanos(1700000000123456789) }, nov.Add(123456789)},
		{"CocoaSeconds", func() (time.Time, bool) { return CocoaSeconds(721692800) }, nov},
		{"CocoaSeconds.5", func() (time.Time, bool) { return CocoaSeconds(721692800.5) }, nov.Add(500 * time.Millisecond)},
		{"CocoaNanos", func() (time.Time, bool) { return CocoaNanos(721692800000000000) }, nov},
		{"CocoaNanosFrac", func() (time.Time, bool) { return CocoaNanos(721692800000000007) }, nov.Add(7)},
		{"WebKitMicros", func() (time.Time, bool) { return WebKitMicros(13414166000000000) }, time.Date(2026, 1, 29, 13, 13, 20, 0, time.UTC)},
		{"WebKitMicrosFrac", func() (time.Time, bool) { return WebKitMicros(13414166000000123) }, time.Date(2026, 1, 29, 13, 13, 20, 123000, time.UTC)},
		{"FileTime", func() (time.Time, bool) { return FileTime(133_000_000_000_000_000) }, time.Date(2022, 6, 18, 4, 26, 40, 0, time.UTC)},
		{"FileTime100ns", func() (time.Time, bool) { return FileTime(133_000_000_000_000_001) }, time.Date(2022, 6, 18, 4, 26, 40, 100, time.UTC)},
	} {
		got, ok := tc.got()
		if !ok || !got.Equal(tc.want) {
			t.Errorf("%s = %v, %v; want %v", tc.name, got, ok, tc.want)
		}
	}
}

func TestMinMax(t *testing.T) {
	if !Min().Equal(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)) || Min().Location() != time.UTC {
		t.Errorf("Min = %v", Min())
	}
	if !Max().Equal(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) || Max().Location() != time.UTC {
		t.Errorf("Max = %v", Max())
	}
}

func TestRangeEdges(t *testing.T) {
	// For each kind: the smallest valid raw value (or one that is not a sentinel), the raw value
	// one unit below it, the last valid raw value (2099-12-31T23:59:59.999999999Z rounded down to
	// the kind's unit) and the raw value of 2100-01-01T00:00:00Z. Unix 4102444800 s is 2100-01-01.
	for _, tc := range []struct {
		kind                     Kind
		minOK, below, maxOK, max int64
	}{
		{KindUnixSeconds, 1, -2, 4102444799, 4102444800},
		{KindUnixMillis, 1, -2, 4102444799999, 4102444800000},
		{KindUnixMicros, 1, -2, 4102444799999999, 4102444800000000},
		{KindUnixNanos, 1, -2, 4102444799999999999, 4102444800000000000},
		// Cocoa: Unix 0 is -978307200 s; Unix 4102444800 is 3124137600 s.
		{KindCocoaNanos, -978307200000000000, -978307200000000001, 3124137599999999999, 3124137600000000000},
		// WebKit: Unix 0 is 11644473600 s; Unix 4102444800 is 15746918400 s.
		{KindWebKitMicros, 11644473600000000, 11644473599999999, 15746918399999999, 15746918400000000},
		{KindFileTime, 116444736000000000, 116444735999999999, 157469183999999999, 157469184000000000},
	} {
		if got := Classify(tc.kind, tc.minOK); got != StatusValid {
			t.Errorf("kind %d min %d: %v, want valid", tc.kind, tc.minOK, got)
		}
		if got := Classify(tc.kind, tc.below); got != StatusInvalid {
			t.Errorf("kind %d below %d: %v, want invalid", tc.kind, tc.below, got)
		}
		if got := Classify(tc.kind, tc.maxOK); got != StatusValid {
			t.Errorf("kind %d maxOK %d: %v, want valid", tc.kind, tc.maxOK, got)
		}
		if got := Classify(tc.kind, tc.max); got != StatusInvalid {
			t.Errorf("kind %d max %d: %v, want invalid", tc.kind, tc.max, got)
		}
		if tm, ok := convInt(tc.kind, tc.maxOK); !ok || !tm.Before(Max()) {
			t.Errorf("kind %d maxOK converts to %v, %v", tc.kind, tm, ok)
		}
		if tm, ok := convInt(tc.kind, tc.max); ok || !tm.IsZero() {
			t.Errorf("kind %d at Max converts to %v", tc.kind, tm)
		}
		if tm, ok := convInt(tc.kind, tc.below); ok || !tm.IsZero() {
			t.Errorf("kind %d below Min converts to %v", tc.kind, tm)
		}
		for _, raw := range []int64{math.MaxInt64 - 1, math.MinInt64 + 1} {
			if got := Classify(tc.kind, raw); got != StatusInvalid {
				t.Errorf("kind %d raw %d: %v, want invalid", tc.kind, raw, got)
			}
			if tm, ok := convInt(tc.kind, raw); ok || !tm.IsZero() {
				t.Errorf("kind %d raw %d wrapped into %v", tc.kind, raw, tm)
			}
		}
	}
	// the exact instants at the edges
	last := time.Date(2099, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if tm, _ := UnixNanos(4102444799999999999); !tm.Equal(last) {
		t.Errorf("last nanosecond = %v", tm)
	}
	if tm, _ := CocoaNanos(3124137599999999999); !tm.Equal(last) {
		t.Errorf("CocoaNanos last = %v", tm)
	}
	if tm, _ := FileTime(157469183999999999); !tm.Equal(last.Add(-99)) {
		t.Errorf("FileTime last = %v", tm)
	}
	if tm, _ := UnixMillis(1); !tm.Equal(time.Date(1970, 1, 1, 0, 0, 0, 1000000, time.UTC)) {
		t.Errorf("first millisecond = %v", tm)
	}
	if tm, ok := CocoaNanos(-978307200000000000); !ok || !tm.Equal(Min()) {
		t.Errorf("CocoaNanos at Min = %v, %v", tm, ok)
	}
	if tm, ok := WebKitMicros(11644473600000000); !ok || !tm.Equal(Min()) {
		t.Errorf("WebKitMicros at Min = %v, %v", tm, ok)
	}
	if tm, ok := FileTime(116444736000000000); !ok || !tm.Equal(Min()) {
		t.Errorf("FileTime at Min = %v, %v", tm, ok)
	}
	// negative values (before 1970)
	if _, ok := UnixSeconds(-86400); ok {
		t.Error("UnixSeconds before 1970 converted")
	}
	// float edges
	if tm, ok := CocoaSeconds(-978307200); !ok || !tm.Equal(Min()) {
		t.Errorf("CocoaSeconds at Min = %v, %v", tm, ok)
	}
	if _, ok := CocoaSeconds(math.Nextafter(-978307200, math.Inf(-1))); ok {
		t.Error("CocoaSeconds one ulp below Min converted")
	}
	if _, ok := CocoaSeconds(math.Nextafter(3124137600, 0)); !ok {
		t.Error("CocoaSeconds one ulp below Max did not convert")
	}
	if _, ok := CocoaSeconds(3124137600); ok {
		t.Error("CocoaSeconds at Max converted")
	}
	for _, f := range []float64{1e300, -1e300, math.MaxFloat64, 9.3e18, -9.3e18} {
		if _, ok := CocoaSeconds(f); ok {
			t.Errorf("CocoaSeconds(%v) converted", f)
		}
		if got := ClassifyFloat(KindCocoaSeconds, f); got != StatusInvalid {
			t.Errorf("ClassifyFloat(%v) = %v", f, got)
		}
	}
}

func TestNoRescaling(t *testing.T) {
	// 1700000000 is a seconds value, but as milliseconds it is 1970-01-20T16:13:20Z, which is
	// inside [1970, 2100): this package does not rescale or guess. Plausibility floors (for
	// example the SMS date below 1e11 ms rule, spec section 9) are the PARSER's rule, applied
	// before calling ts; ts only guards the record range.
	got, ok := UnixMillis(1700000000)
	if !ok || !got.Equal(time.Date(1970, 1, 20, 16, 13, 20, 0, time.UTC)) {
		t.Errorf("UnixMillis(1700000000) = %v, %v", got, ok)
	}
	// and a milliseconds value read as seconds is far past 2100: invalid, never divided down
	if _, ok := UnixSeconds(1700000000000); ok {
		t.Error("a millisecond value converted as seconds")
	}
	if got := Classify(KindUnixSeconds, 1700000000000); got != StatusInvalid {
		t.Errorf("Classify = %v, want invalid", got)
	}
}

func TestResultsAreUTC(t *testing.T) {
	raws := map[Kind]int64{
		KindUnixSeconds: 1700000000, KindUnixMillis: 1700000000000, KindUnixMicros: 1700000000000000,
		KindUnixNanos: 1700000000000000000, KindCocoaSeconds: 721692800, KindCocoaNanos: 721692800000000000,
		KindWebKitMicros: 13414166000000000, KindFileTime: 133_000_000_000_000_000,
	}
	for _, k := range allKinds {
		tm, ok := convInt(k, raws[k])
		if !ok || tm.Location() != time.UTC || !tm.Equal(tm.UTC()) {
			t.Errorf("kind %d: %v ok=%v loc=%v", k, tm, ok, tm.Location())
		}
	}
	if tm, _ := CocoaSeconds(721692800.25); tm.Location() != time.UTC {
		t.Errorf("CocoaSeconds location %v", tm.Location())
	}
}

func TestFloatPrecision(t *testing.T) {
	base := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	// 721692800.123456789 is not representable (spacing near 7e8 is 1.19e-7 s); the result is the
	// nearest representable value, within 120 ns, and always in the same second.
	tm, ok := CocoaSeconds(721692800.123456789)
	if !ok || tm.Unix() != base.Unix() {
		t.Fatalf("got %v, %v", tm, ok)
	}
	if d := tm.Nanosecond() - 123456789; d < -120 || d > 120 {
		t.Errorf("nanoseconds = %d, want 123456789 +- 120", tm.Nanosecond())
	}
	// the largest float below the next second stays in its own second (never rounds up to it)
	tm, ok = CocoaSeconds(math.Nextafter(721692801, 0))
	if !ok || tm.Unix() != base.Unix() || tm.Nanosecond() < 999999000 {
		t.Errorf("below the next second: %v, %v", tm, ok)
	}
	// pinned rounding: a fraction that rounds to a whole 1e9 ns rolls into the next second
	// (5 - 1e-10 s is 4.9999999999, nearest nanosecond is 5.000000000).
	tm, ok = CocoaSeconds(5 - 1e-10)
	if !ok || !tm.Equal(time.Date(2001, 1, 1, 0, 0, 5, 0, time.UTC)) {
		t.Errorf("rollover: %v, %v", tm, ok)
	}
	// nearest, not truncated: 5.0000000006 is 5 s + 0.6 ns, which rounds to 1 ns
	tm, _ = CocoaSeconds(5.0000000006)
	if tm.Nanosecond() != 1 {
		t.Errorf("round to nearest: %d ns, want 1", tm.Nanosecond())
	}
	// a negative fraction counts from the floor
	tm, ok = CocoaSeconds(-1e8 - 0.5)
	want := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(-100000000*time.Second - 500*time.Millisecond)
	if !ok || !tm.Equal(want) {
		t.Errorf("negative fraction: %v, %v", tm, ok)
	}
}

func TestClassifyFloatIntegerKinds(t *testing.T) {
	for _, tc := range []struct {
		k    Kind
		f    float64
		want Status
	}{
		{KindUnixMillis, 1700000000000, StatusValid},
		{KindUnixMillis, 1700000000000.5, StatusInvalid}, // not an exact integer
		{KindUnixMillis, 0, StatusSentinel},
		{KindUnixMillis, -1, StatusSentinel},
		{KindUnixMillis, 9.3e18, StatusInvalid}, // does not fit int64
		{KindUnixMillis, -9.3e18, StatusInvalid},
		{KindUnixMillis, math.NaN(), StatusInvalid},
		{KindUnixMillis, math.Inf(1), StatusInvalid},
		{KindUnixSeconds, 1700000000, StatusValid},
		{KindWebKitMicros, 13414166000000000, StatusValid},
		{KindCocoaSeconds, 721692800.5, StatusValid},
		{Kind(99), 1, StatusInvalid},
		{Kind(8), 1, StatusInvalid}, // first value past KindFileTime
		{KindUnixMillis, 1 << 63, StatusInvalid},
		{KindUnixMillis, -(1 << 63), StatusSentinel}, // exactly MinInt64
	} {
		if got := ClassifyFloat(tc.k, tc.f); got != tc.want {
			t.Errorf("ClassifyFloat(%d, %v) = %v, want %v", tc.k, tc.f, got, tc.want)
		}
	}
	if got := Classify(Kind(8), 1); got != StatusInvalid {
		t.Errorf("Kind(8): %v", got)
	}
	if got := Classify(Kind(99), 1); got != StatusInvalid {
		t.Errorf("unknown kind: %v", got)
	}
	if got := Classify(KindCocoaSeconds, 721692800); got != StatusValid {
		t.Errorf("Classify cocoa seconds: %v", got)
	}
}

func TestConvertersAgreeWithClassify(t *testing.T) {
	vals := []int64{0, 1, -1, -2, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1}
	state := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state
	}
	for i := 0; i < 10000; i++ {
		v := next()
		shift := next() >> 58 // 0..63
		vals = append(vals, int64(v>>shift), -int64(v>>shift))
	}
	for _, k := range allKinds {
		for _, v := range vals {
			tm, ok := convInt(k, v)
			st := Classify(k, v)
			if ok != (st == StatusValid) {
				t.Fatalf("kind %d raw %d: ok=%v status=%v", k, v, ok, st)
			}
			if ok && (tm.Before(Min()) || !tm.Before(Max())) {
				t.Fatalf("kind %d raw %d: %v outside range", k, v, tm)
			}
			if !ok && !tm.IsZero() {
				t.Fatalf("kind %d raw %d: no-time result carries %v", k, v, tm)
			}
		}
	}
}

func TestCocoaNanosDoesNotOverflow(t *testing.T) {
	for _, v := range []int64{math.MaxInt64 - 1, math.MinInt64 + 1} {
		if tm, ok := CocoaNanos(v); ok || !tm.IsZero() {
			t.Errorf("CocoaNanos(%d) = %v, %v", v, tm, ok)
		}
	}
}

func TestWebKitMicrosDoesNotOverflow(t *testing.T) {
	for _, v := range []int64{math.MaxInt64 - 1, math.MinInt64 + 1} {
		if tm, ok := WebKitMicros(v); ok || !tm.IsZero() {
			t.Errorf("WebKitMicros(%d) = %v, %v", v, tm, ok)
		}
	}
}

// The float range check runs before any float-to-int conversion, so NaN is refused by a
// failed comparison and not by the platform's conversion result (MinInt64 on amd64, 0 on arm64).
func TestConvertFloatRefusesNaNBeforeConversion(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Float64frombits(0x7ff8000000000123), math.Inf(1), math.Inf(-1)} {
		if tm, st := convertFloat(v); st != StatusInvalid || !tm.IsZero() {
			t.Errorf("convertFloat(%v) = %v %v, want invalid", v, tm, st)
		}
	}
}
