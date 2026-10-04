package records

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Payload caps. The size cap is on the canonical encoding.
const (
	maxPayload      = 16 << 20
	maxPayloadDepth = 64
)

// canonicalPayload returns the canonical JSON encoding of a payload, the exact
// bytes that are stored and hashed:
//   - keys are sorted (byte order), there is no insignificant whitespace and no
//     HTML escaping (strings are escaped only where JSON requires it: `"`, `\`
//     and control characters below 0x20; U+2028 and U+2029 are written as they
//     are);
//   - numbers are written exactly as given: a json.Number is emitted verbatim
//     (after a grammar check), so 12345678901234567890 survives, and 1.50 stays
//     1.50;
//   - the only allowed value types are nil, bool, string, json.Number, float64
//     (finite), int, int64, uint64, []any and map[string]any; anything else
//     (including every other named or sized type) is an error naming the type;
//   - strings and keys must be valid UTF-8 without NUL. They are checked on the
//     input tree as they are encoded, so nothing is ever silently replaced by
//     U+FFFD the way encoding/json would do;
//   - containers nest at most 64 levels (the object itself is level 1) and the
//     encoding is at most 16 MiB; the encoder stops at the cap, so a hostile
//     tree (a cycle, or one with exponentially many paths) costs bounded work.
//
// A nil map encodes as {}. Every error wraps ErrInvalidPayload, except the size
// cap (ErrRecordTooLarge).
func canonicalPayload(p map[string]any) (string, error) { return canonicalValue(p) }

// canonicalValue is canonicalPayload for any value; the top level must be a
// map[string]any (a JSON object).
func canonicalValue(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%w: the payload must be a JSON object, got %T", ErrInvalidPayload, v)
	}
	var e payloadEncoder
	if err := e.value(m, 1); err != nil {
		return "", err
	}
	return string(e.buf), nil
}

type payloadEncoder struct{ buf []byte }

func (e *payloadEncoder) tooLarge() error {
	return fmt.Errorf("%w: payload is larger than %d bytes", ErrRecordTooLarge, maxPayload)
}

// room reports whether n more bytes still fit under the cap.
func (e *payloadEncoder) room(n int) bool { return n <= maxPayload-len(e.buf) }

func (e *payloadEncoder) value(v any, depth int) error {
	if !e.room(1) {
		return e.tooLarge()
	}
	switch x := v.(type) {
	case nil:
		e.buf = append(e.buf, "null"...)
	case bool:
		e.buf = strconv.AppendBool(e.buf, x)
	case string:
		return e.str(x)
	case json.Number:
		if !validNumber(string(x)) {
			return fmt.Errorf("%w: %q is not a JSON number", ErrInvalidPayload, clip(string(x)))
		}
		if !e.room(len(x)) {
			return e.tooLarge()
		}
		e.buf = append(e.buf, x...)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("%w: non-finite float %v", ErrInvalidPayload, x)
		}
		e.buf = strconv.AppendFloat(e.buf, x, 'g', -1, 64)
	case int:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case int64:
		e.buf = strconv.AppendInt(e.buf, x, 10)
	case uint64:
		e.buf = strconv.AppendUint(e.buf, x, 10)
	case []any:
		if depth > maxPayloadDepth {
			return depthError()
		}
		e.buf = append(e.buf, '[')
		for i, el := range x {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			if err := e.value(el, depth+1); err != nil {
				return err
			}
		}
		e.buf = append(e.buf, ']')
	case map[string]any:
		if depth > maxPayloadDepth {
			return depthError()
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		e.buf = append(e.buf, '{')
		for i, k := range keys {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			if err := e.str(k); err != nil {
				return err
			}
			e.buf = append(e.buf, ':')
			if err := e.value(x[k], depth+1); err != nil {
				return err
			}
		}
		e.buf = append(e.buf, '}')
	default:
		return fmt.Errorf("%w: unsupported value type %T", ErrInvalidPayload, v)
	}
	if len(e.buf) > maxPayload {
		return e.tooLarge()
	}
	return nil
}

func depthError() error {
	return fmt.Errorf("%w: nesting depth exceeds %d levels", ErrInvalidPayload, maxPayloadDepth)
}

const hexDigits = "0123456789abcdef"

// str appends s as a JSON string after checking it is valid UTF-8 without NUL.
func (e *payloadEncoder) str(s string) error {
	if len(s) > maxPayload {
		return e.tooLarge()
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: string is not valid UTF-8", ErrInvalidPayload)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("%w: string contains NUL", ErrInvalidPayload)
	}
	if !e.room(len(s) + 2) {
		return e.tooLarge()
	}
	e.buf = append(e.buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			e.buf = append(e.buf, '\\', '"')
		case c == '\\':
			e.buf = append(e.buf, '\\', '\\')
		case c == '\n':
			e.buf = append(e.buf, '\\', 'n')
		case c == '\r':
			e.buf = append(e.buf, '\\', 'r')
		case c == '\t':
			e.buf = append(e.buf, '\\', 't')
		case c < 0x20:
			e.buf = append(e.buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			e.buf = append(e.buf, c)
		}
		if len(e.buf) > maxPayload {
			return e.tooLarge()
		}
	}
	e.buf = append(e.buf, '"')
	return nil
}

// validNumber reports whether s is a number in the JSON grammar:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?
func validNumber(s string) bool {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i >= len(s):
		return false
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	digits := func() bool {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i > start
	}
	if i < len(s) && s[i] == '.' {
		i++
		if !digits() {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if !digits() {
			return false
		}
	}
	return i == len(s)
}
