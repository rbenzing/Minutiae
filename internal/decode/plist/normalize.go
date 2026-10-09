package plist

import (
	"bytes"
	"fmt"
	"math"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	howett "howett.net/plist"
)

// UID is a keyed-archive object reference (binary tag 0x8x, or the XML form
// <dict><key>CF$UID</key><integer>n</integer></dict>).
type UID uint64

// What howett.net/plist v1.0.1 yields when unmarshalling into an `any` (observed with a
// throwaway exploration test before this file was written):
//
//   - maps are map[string]interface{} and arrays []interface{};
//   - integers: a negative value is int64, every non-negative one is uint64 (also from
//     1, 2 and 4-byte binary integers, which are unsigned by format); an 8-byte binary integer
//     with the top bit set is a negative int64; a 16-byte binary integer is read as hi:lo and
//     only lo is kept, which is why the validator refuses one that does not fit 64 bits;
//   - reals: a 4-byte binary real is float32, an 8-byte binary real and every XML real are
//     float64 (an XML <real> never yields float32);
//   - booleans bool; strings string (it copies the bytes, so the result never aliases the
//     input; it replaces a lone UTF-16 surrogate with U+FFFD and keeps invalid 8-bit bytes
//     as they are, which is why Decode reads those from the document itself); data []byte
//     (empty data is a non-nil empty slice; in binary a view of the input);
//   - dates time.Time in UTC for both formats (an XML date with an offset is converted); a
//     binary date double that is NaN, infinite or huge becomes a meaningless instant, which is
//     why Decode reads the stored double itself;
//   - plist.UID for the binary UID tag and, in XML, for a dict that has exactly one key,
//     CF$UID, whose value is an integer (a dict with other keys, or a string value, stays a
//     map);
//   - a duplicate key in a dict: the last value wins (a known property, not detected).
//
// normalizer.value turns that into the package's plain value set: map[string]any, []any, string
// (always valid UTF-8), int64 (uint64 only above math.MaxInt64), float64, bool, []byte,
// time.Time (UTC, years 1 to 9999), UID, and the two raw forms RawString and RawDate for what
// a Go string or time.Time would alter. Byte slices are copied so the result never aliases
// the input. Anything else the library might return is ErrUnsupported.

// normalizer is the value conversion. A non-zero base is the start of a block of 2048
// private code points that stand in for the surrogates U+D800 to U+DFFF in an XML document
// whose character references to surrogates were rewritten before the library saw it.
type normalizer struct {
	base rune
}

// stand reports whether r is one of the stand-in code points.
func (n normalizer) stand(r rune) bool {
	return n.base != 0 && r >= n.base && r < n.base+2048
}

func (n normalizer) hasStand(s string) bool {
	if n.base == 0 {
		return false
	}
	for _, r := range s {
		if n.stand(r) {
			return true
		}
	}
	return false
}

// raw returns the UTF-16 code units of s with the stand-ins mapped back to surrogates.
func (n normalizer) raw(s string) RawString {
	var units []uint16
	for _, r := range s {
		if n.stand(r) {
			units = append(units, uint16(0xD800+(r-n.base)))
		} else {
			units = utf16.AppendRune(units, r)
		}
	}
	out := make([]byte, 0, 2*len(units))
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return RawString{Bytes: out, UTF16: true}
}

func (n normalizer) value(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !utf8.ValidString(k) || n.hasStand(k) {
				return nil, fmt.Errorf("%w: dictionary key is not valid text", ErrUnsupported)
			}
			c, err := n.value(e)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			c, err := n.value(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case string:
		switch {
		case n.hasStand(x):
			return n.raw(x), nil
		case !utf8.ValidString(x):
			return RawString{Bytes: []byte(x)}, nil
		}
		return x, nil
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x), nil
		}
		return x, nil
	case int64:
		return x, nil
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	case bool:
		return x, nil
	case []byte:
		if len(x) == 0 {
			return []byte{}, nil
		}
		return bytes.Clone(x), nil
	case time.Time:
		t := x.UTC()
		if y := t.Year(); y < 1 || y > 9999 {
			return RawDate{Seconds: float64(t.Unix()-cocoaToUnix) + float64(t.Nanosecond())/1e9}, nil
		}
		return t, nil
	case howett.UID:
		return UID(x), nil
	}
	return nil, fmt.Errorf("%w: library value of type %T", ErrUnsupported, v)
}
