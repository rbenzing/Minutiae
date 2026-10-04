package records

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Field caps (bytes unless noted).
const (
	maxSummary    = 512
	maxBody       = 4 << 20
	maxLocator    = 1024
	maxSourcePath = 4096
	maxTimes      = 16 // secondary times per record
)

var (
	locatorSchemeRE = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}:`)
	recoveryRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	kindRE          = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	parserNameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	parserHashRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// Timestamps outside [1970-01-01, 2100-01-01) are accepted only when the payload
// explains them in a non-empty string "ts_note".
const (
	minPlainMicros = 0
	maxPlainMicros = 4102444800 * 1_000_000 // 2100-01-01T00:00:00Z
)

// artifactInfo is what the writer knows about the artifact a record points at
// (from the manifest).
type artifactInfo struct {
	ID, SHA256 string
	Size       int64
	Incomplete bool
}

// prepared is a validated record as a row, ready to be numbered and written.
type prepared struct {
	row         evidence.RecordRow
	approxBytes int // rough in-memory/on-disk size, for batching by bytes
}

// prepare validates r against the artifact it points at and converts it to a
// row. The row's ID and the parser fields are filled in by the writer. Nothing
// is repaired or truncated: a record that breaks a rule is rejected with a typed
// error (every one wraps ErrInvalidRecord).
//
// Times are stored as Unix microseconds, computed from T.Unix() and
// T.Nanosecond() with overflow-checked math; a sub-microsecond remainder is
// truncated (toward negative infinity: parsers keep raw values in the payload).
func prepare(r Record, art artifactInfo) (prepared, error) {
	ty, ok := LookupType(r.Type)
	if !ok {
		return prepared{}, fmt.Errorf("%w: %q", ErrUnknownType, clip(r.Type))
	}
	if r.ArtifactID == "" {
		return prepared{}, fmt.Errorf("%w: the record names no artifact", ErrUnknownArtifact)
	}
	if r.ArtifactID != art.ID {
		return prepared{}, fmt.Errorf("%w: %q is not the artifact %q", ErrUnknownArtifact, clip(r.ArtifactID), clip(art.ID))
	}

	for _, f := range []struct {
		name string
		s    string
		max  int
	}{
		{"summary", r.Summary, maxSummary},
		{"body", r.Body, maxBody},
		{"source path", r.SourcePath, maxSourcePath},
		{"locator", r.Locator, maxLocator},
	} {
		if len(f.s) > f.max {
			return prepared{}, fmt.Errorf("%w: %s is %d bytes, the cap is %d", ErrRecordTooLarge, f.name, len(f.s), f.max)
		}
		if err := checkText(f.name, f.s); err != nil {
			return prepared{}, err
		}
	}
	if r.Locator != "" && !locatorSchemeRE.MatchString(r.Locator) {
		return prepared{}, fmt.Errorf("%w: %q does not start with <scheme>: (scheme [a-z][a-z0-9]{1,15})", ErrInvalidLocator, clip(r.Locator))
	}

	row := evidence.RecordRow{
		Type: r.Type, PayloadV: ty.PayloadVersion, ArtifactID: r.ArtifactID,
		Deleted: r.Deleted, Summary: r.Summary,
	}
	if r.SourcePath != "" {
		row.SourcePath = &r.SourcePath
	}
	if r.Locator != "" {
		row.Locator = &r.Locator
	}
	if r.Body != "" {
		row.Body = &r.Body
	}

	if rg := r.Range; rg != nil {
		if rg.Offset < 0 || rg.Length < 0 {
			return prepared{}, fmt.Errorf("%w: offset %d and length %d must not be negative", ErrInvalidRange, rg.Offset, rg.Length)
		}
		if rg.Length > math.MaxInt64-rg.Offset {
			return prepared{}, fmt.Errorf("%w: offset %d + length %d overflows", ErrInvalidRange, rg.Offset, rg.Length)
		}
		if rg.Offset+rg.Length > art.Size {
			return prepared{}, fmt.Errorf("%w: offset %d + length %d is beyond the artifact's %d bytes", ErrInvalidRange, rg.Offset, rg.Length, art.Size)
		}
		off, length := rg.Offset, rg.Length
		row.SrcOffset, row.SrcLength = &off, &length
	}

	payload, err := canonicalPayload(r.Payload)
	if err != nil {
		return prepared{}, err
	}
	row.Payload = payload
	explained := hasTimeNote(r.Payload)

	if err := prepareTimes(&row, r, explained); err != nil {
		return prepared{}, err
	}

	if r.Recovery != "" {
		if !r.Deleted {
			return prepared{}, fmt.Errorf("%w: recovery %q requires the record to be deleted", ErrInvalidField, clip(r.Recovery))
		}
		if !recoveryRE.MatchString(r.Recovery) {
			return prepared{}, fmt.Errorf("%w: recovery method %q must match [a-z][a-z0-9-]{1,31}", ErrInvalidField, clip(r.Recovery))
		}
		method := r.Recovery
		row.Recovered, row.RecoveryMethod = true, &method
	}
	if r.Confidence != nil {
		if *r.Confidence < 0 || *r.Confidence > 100 {
			return prepared{}, fmt.Errorf("%w: confidence %d is outside 0..100", ErrInvalidField, *r.Confidence)
		}
		c := int64(*r.Confidence)
		row.Confidence = &c
	}

	if ty.Validate != nil {
		if err := runValidator(ty, r.Payload); err != nil {
			return prepared{}, err
		}
	}

	approx := 192 + len(row.Type) + len(row.ArtifactID) + len(row.Summary) + len(row.Payload) + 64*len(row.Times)
	for _, s := range []*string{row.SourcePath, row.Locator, row.Body, row.RecoveryMethod} {
		if s != nil {
			approx += len(*s)
		}
	}
	return prepared{row: row, approxBytes: approx}, nil
}

// prepareTimes fills the row's ts, ts_end, basis, offset and secondary times.
func prepareTimes(row *evidence.RecordRow, r Record, explained bool) error {
	if r.TimeEnd != nil && r.Time == nil {
		return fmt.Errorf("%w: an end time needs a start time", ErrInvalidTime)
	}
	if r.Time != nil {
		ts, basis, off, err := convertTime(*r.Time, explained)
		if err != nil {
			return err
		}
		row.TS, row.TSBasis, row.TZOffsetMin = &ts, &basis, off
		if r.TimeEnd != nil {
			end, endBasis, endOff, err := convertTime(*r.TimeEnd, explained)
			if err != nil {
				return fmt.Errorf("end time: %w", err)
			}
			// the row has one basis and offset, shared by ts and ts_end
			if endBasis != basis || (endOff == nil) != (off == nil) || (off != nil && *endOff != *off) {
				return fmt.Errorf("%w: the end time must have the start time's basis and offset", ErrInvalidTime)
			}
			row.TSEnd = &end
		}
	}
	if len(r.Times) > maxTimes {
		return fmt.Errorf("%w: %d secondary times, the cap is %d", ErrRecordTooLarge, len(r.Times), maxTimes)
	}
	seen := make(map[string]bool, len(r.Times))
	for _, nt := range r.Times {
		if !kindRE.MatchString(nt.Kind) {
			return fmt.Errorf("%w: time kind %q must match [a-z][a-z0-9_]{1,31}", ErrInvalidField, clip(nt.Kind))
		}
		if seen[nt.Kind] {
			return fmt.Errorf("%w: time kind %q appears twice", ErrInvalidField, nt.Kind)
		}
		seen[nt.Kind] = true
		ts, basis, off, err := convertTime(nt.Time, explained)
		if err != nil {
			return fmt.Errorf("time %q: %w", nt.Kind, err)
		}
		row.Times = append(row.Times, evidence.RecordTime{Kind: nt.Kind, TS: ts, Basis: basis, TZOffsetMin: off})
	}
	slices.SortFunc(row.Times, func(a, b evidence.RecordTime) int { return strings.Compare(a.Kind, b.Kind) })
	return nil
}

// convertTime validates t and returns its microseconds, basis and offset (nil
// unless the basis is local-offset).
func convertTime(t Time, explained bool) (micros int64, basis string, offset *int64, err error) {
	switch t.Basis {
	case "", BasisUTC:
		basis = string(BasisUTC)
	case BasisLocalOffset, BasisLocalUnknown:
		basis = string(t.Basis)
	default:
		return 0, "", nil, fmt.Errorf("%w: unknown basis %q", ErrInvalidTime, clip(string(t.Basis)))
	}
	if t.Basis == BasisLocalOffset {
		if t.OffsetMin <= -1440 || t.OffsetMin >= 1440 {
			return 0, "", nil, fmt.Errorf("%w: offset %d minutes is outside (-1440, 1440)", ErrInvalidTime, t.OffsetMin)
		}
		o := int64(t.OffsetMin)
		offset = &o
	} else if t.OffsetMin != 0 {
		return 0, "", nil, fmt.Errorf("%w: an offset is only allowed with basis %q", ErrInvalidTime, BasisLocalOffset)
	}
	micros, ok := unixMicros(t.T)
	if !ok {
		return 0, "", nil, fmt.Errorf("%w: %v does not fit in Unix microseconds", ErrInvalidTime, t.T.UTC().Format(time.RFC3339Nano))
	}
	if (micros < minPlainMicros || micros >= maxPlainMicros) && !explained {
		return 0, "", nil, fmt.Errorf("%w: %v is outside 1970-01-01..2100-01-01 and the payload has no ts_note", ErrInvalidTime, t.T.UTC().Format(time.RFC3339Nano))
	}
	return micros, basis, offset, nil
}

// unixMicros is t as Unix microseconds, floored, with every step checked.
func unixMicros(t time.Time) (int64, bool) {
	sec, us := t.Unix(), int64(t.Nanosecond())/1000 // us in [0, 999999]
	if sec >= 0 {
		if sec > math.MaxInt64/1_000_000 {
			return 0, false
		}
		base := sec * 1_000_000
		if us > math.MaxInt64-base {
			return 0, false
		}
		return base + us, true
	}
	// sec*1e6 may fall below MinInt64 while the sum with us does not, so
	// compute (sec+1)*1e6 + (us-1e6) instead.
	s1 := sec + 1
	if s1 < math.MinInt64/1_000_000 {
		return 0, false
	}
	base, d := s1*1_000_000, us-1_000_000
	if base < math.MinInt64-d {
		return 0, false
	}
	return base + d, true
}

func hasTimeNote(p map[string]any) bool {
	s, ok := p["ts_note"].(string)
	return ok && s != ""
}

func checkText(name, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidText, name)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("%w: %s contains NUL", ErrInvalidText, name)
	}
	return nil
}

// maxClip is how many bytes of caller-supplied text an error message may carry.
const maxClip = 128

// clip shortens hostile text for an error message: at most maxClip bytes, cut on
// a rune boundary, invalid UTF-8 replaced. Callers print it with %q, which also
// escapes control characters.
func clip(s string) string {
	if len(s) <= maxClip {
		return strings.ToValidUTF8(s, "?")
	}
	cut := maxClip
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], "?") + "..."
}

// runValidator calls a type's validator on a deep copy of the payload (so it can
// neither change the caller's map nor what is stored) and turns a panic into a
// typed validation error: a registered validator is code the writer does not
// control, and a parser's record must never take the process down.
func runValidator(ty Type, payload map[string]any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: type %q: validator panicked: panic: %s", ErrInvalidPayload, ty.Name, clip(panicText(r)))
		}
	}()
	if err := ty.Validate(deepCopyPayload(payload)); err != nil {
		if errors.Is(err, ErrInvalidPayload) {
			return err
		}
		return fmt.Errorf("%w: type %q: %w", ErrInvalidPayload, ty.Name, err)
	}
	return nil
}

// deepCopyPayload copies maps and slices recursively. It runs only on a payload
// that canonicalPayload already accepted (so depth and size are bounded and it
// holds only the encodable value types, whose scalars are immutable).
func deepCopyPayload(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyPayload(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyValue(e)
		}
		return out
	}
	return v
}

// validateParser checks a parser identity. The hash is optional: "" means none
// and is stored as NULL, never as an empty string; a given hash is a token of
// at most 128 characters.
func validateParser(p Parser) error {
	if !parserNameRE.MatchString(p.Name) {
		return fmt.Errorf("%w: parser name %q must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}", ErrInvalidField, clip(p.Name))
	}
	if !parserNameRE.MatchString(p.Version) {
		return fmt.Errorf("%w: parser version %q must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}", ErrInvalidField, clip(p.Version))
	}
	if p.Hash != "" && !parserHashRE.MatchString(p.Hash) {
		return fmt.Errorf("%w: parser hash %q must be empty (none) or match [A-Za-z0-9][A-Za-z0-9._:-]{0,127}", ErrInvalidField, clip(p.Hash))
	}
	return nil
}

// panicText describes a recovered panic value without running any method of an
// arbitrary type: a string as it is, an error through Error() (guarded: a
// panicking Error method must not panic inside the recover), anything else by
// its type name.
func panicText(r any) (text string) {
	defer func() {
		if p := recover(); p != nil {
			text = fmt.Sprintf("%T (its Error method panicked)", r)
		}
	}()
	switch v := r.(type) {
	case string:
		return v
	case error:
		return v.Error()
	}
	return fmt.Sprintf("%T", r)
}
