package sqlitefile

import (
	"math"
	"unicode/utf8"
)

// KeyColumn is one column of a b-tree key: the collation it is compared under ("" is BINARY) and its sort order.
type KeyColumn struct {
	Collation string
	Desc      bool
}

// OrderStatus says whether the comparator could decide.
type OrderStatus uint8

// The statuses.
const (
	// OrderOK: the order is the engine's.
	OrderOK OrderStatus = iota
	// OrderUndecided: a collation this reader does not reproduce, an unread,
	// omitted or clipped value, text invalid in its encoding (for the collations
	// that fold it), text of two encodings under BINARY, or a Kind this library
	// does not define. The result is 0 and carries no order.
	OrderUndecided
)

// orderClass is the engine's cross-type order: NULL, numbers, text, blobs.
func orderClass(k Kind) (int, bool) {
	switch k {
	case KindNull:
		return 0, true
	case KindInt, KindFloat:
		return 1, true
	case KindText:
		return 2, true
	case KindBlob:
		return 3, true
	}
	return 0, false
}

// isNullLike is true for NULL and for a float NaN: the engine's record reader
// turns a stored NaN into NULL, so a NaN orders exactly as NULL.
func isNullLike(v Value) bool {
	return v.Kind == KindNull || (v.Kind == KindFloat && math.IsNaN(v.Float))
}

// OrderValues orders two values as the engine's record comparison does for one column in
// ascending order: NULL, then numbers (integers and reals compared exactly), then text under
// the collation, then blobs; it returns -1, 0 or 1. A float NaN is ordered exactly as NULL (the
// engine's record reader turns a stored NaN into NULL; EqualityKey already gives KeyNever), so
// OrderValues(NaN, NaN) == 0, NaN equals NULL and sorts before every number.
//
// A result the reader cannot reproduce is (0, OrderUndecided), never a guess. The comparison
// allocates nothing for UTF-8 text and reads no more than the two values.
func OrderValues(a, b Value, collation string) (int, OrderStatus) {
	for _, v := range [2]Value{a, b} {
		if v.Omitted || v.Unread || v.Clipped {
			return 0, OrderUndecided
		}
		if _, ok := orderClass(v.Kind); !ok {
			return 0, OrderUndecided
		}
	}
	coll, err := CanonicalCollation(collation)
	if err != nil {
		return 0, OrderUndecided
	}
	an, bn := isNullLike(a), isNullLike(b)
	if an || bn {
		switch {
		case an && bn:
			return 0, OrderOK
		case an:
			return -1, OrderOK
		}
		return 1, OrderOK
	}
	ca, _ := orderClass(a.Kind)
	cb, _ := orderClass(b.Kind)
	if ca != cb {
		return cmpInt(ca, cb), OrderOK
	}
	switch ca {
	case 1:
		return orderNumbers(a, b), OrderOK
	case 2:
		return orderText(a, b, coll)
	}
	return cmpBytes(a.Bytes, b.Bytes), OrderOK
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpBytes is the engine's memcmp over the shorter length, then the lengths.
func cmpBytes(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(a), len(b))
}

// orderNumbers compares two numeric values exactly. An integer meets a real as
// the engine's integer/real comparison does: out-of-range reals by magnitude, then
// the integer part, then the fraction; no int64 is rounded through a float64.
func orderNumbers(a, b Value) int {
	switch {
	case a.Kind == KindInt && b.Kind == KindInt:
		switch {
		case a.Int < b.Int:
			return -1
		case a.Int > b.Int:
			return 1
		}
		return 0
	case a.Kind == KindFloat && b.Kind == KindFloat:
		switch {
		case a.Float < b.Float:
			return -1
		case a.Float > b.Float:
			return 1
		}
		return 0 // -0.0 equals 0.0
	case a.Kind == KindInt:
		return orderIntFloat(a.Int, b.Float)
	}
	return -orderIntFloat(b.Int, a.Float)
}

func orderIntFloat(i int64, f float64) int {
	switch {
	case f < -9223372036854775808.0:
		return 1
	case f >= 9223372036854775808.0:
		return -1
	}
	y := int64(f) // in range: truncation toward zero
	switch {
	case i < y:
		return -1
	case i > y:
		return 1
	}
	s := float64(i)
	switch {
	case s < f:
		return -1
	case s > f:
		return 1
	}
	return 0
}

// orderText compares two text values under the canonical collation coll.
func orderText(a, b Value, coll string) (int, OrderStatus) {
	if coll == "BINARY" {
		// The stored bytes, as the engine's memcmp does; two encodings are not
		// compared (the engine translates one side, which this reader does not).
		if a.Enc != b.Enc {
			return 0, OrderUndecided
		}
		return cmpBytes(a.Bytes, b.Bytes), OrderOK
	}
	ab, ok1 := foldBytes(a)
	bb, ok2 := foldBytes(b)
	if !ok1 || !ok2 {
		return 0, OrderUndecided
	}
	if coll == "RTRIM" {
		ab, bb = trimSpaces(ab), trimSpaces(bb)
		return cmpBytes(ab, bb), OrderOK
	}
	// NOCASE as the engine's collation: ASCII letters folded, byte by byte over the
	// common length, stopping at the first NUL (both bytes NUL: the rest is not
	// looked at), then the lengths decide (U55).
	n := min(len(ab), len(bb))
	for i := range n {
		x, y := lowerASCII(ab[i]), lowerASCII(bb[i])
		if x != y {
			if x < y {
				return -1, OrderOK
			}
			return 1, OrderOK
		}
		if x == 0 {
			break
		}
	}
	return cmpInt(len(ab), len(bb)), OrderOK
}

// foldBytes returns the UTF-8 bytes NOCASE and RTRIM compare: the stored bytes of
// UTF-8 text (which must be valid, and are not copied), the decoded text of
// UTF-16. ok is false for text invalid in its encoding.
func foldBytes(v Value) ([]byte, bool) {
	switch v.Enc {
	case EncUTF16LE, EncUTF16BE:
		s, ok := v.Text()
		return []byte(s), ok
	}
	return v.Bytes, utf8.Valid(v.Bytes)
}

func trimSpaces(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return b
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// OrderKeys compares the first len(cols) values of a and b column by column, applying each
// column's collation and sort order (DESC inverts that column's result, NULLs included).
// n is the index of the first column that differs (len(cols) when none does). A key shorter
// than cols is OrderUndecided (n is then the first missing column), and so is a column that
// cannot be decided before one that differs (n is that column).
func OrderKeys(a, b []Value, cols []KeyColumn) (c int, n int, st OrderStatus) {
	if short := min(len(a), len(b)); short < len(cols) {
		return 0, short, OrderUndecided
	}
	for i, kc := range cols {
		r, st := OrderValues(a[i], b[i], kc.Collation)
		if st != OrderOK {
			return 0, i, OrderUndecided
		}
		if r != 0 {
			if kc.Desc {
				r = -r
			}
			return r, i, OrderOK
		}
	}
	return 0, len(cols), OrderOK
}
