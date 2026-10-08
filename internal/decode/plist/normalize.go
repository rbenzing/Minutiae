package plist

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"time"

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
//   - booleans bool; strings string (binary ASCII strings and 8-bit data are zero-copy
//     views of the input buffer, so they are cloned here); data []byte (empty data is a
//     non-nil empty slice; also a view of the input in binary);
//   - dates time.Time in UTC for both formats (an XML date with an offset is converted);
//   - plist.UID for the binary UID tag and, in XML, for a dict that has exactly one key,
//     CF$UID, whose value is an integer (a dict with other keys, or a string value, stays a
//     map);
//   - a duplicate key in a dict: the last value wins (a known property, not detected).
//
// normalize turns that into the package's plain value set: map[string]any, []any, string,
// int64 (uint64 only above math.MaxInt64), float64, bool, []byte, time.Time (UTC) and UID,
// with every string and byte slice copied so the result never aliases the input. Anything
// else the library might return is ErrUnsupported.
func normalize(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[strings.Clone(k)] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case string:
		return strings.Clone(x), nil
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
		return x.UTC(), nil
	case howett.UID:
		return UID(x), nil
	}
	return nil, fmt.Errorf("%w: library value of type %T", ErrUnsupported, v)
}
