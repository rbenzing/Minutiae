package sqlitefile

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
)

// ErrUnsupportedCollation is returned for a collation the reader cannot
// reproduce (anything but BINARY, NOCASE and RTRIM).
var ErrUnsupportedCollation = errors.New("sqlitefile: collation is not BINARY, NOCASE or RTRIM")

// CanonicalCollation returns "BINARY", "NOCASE" or "RTRIM" for name ("" is
// BINARY; the comparison is ASCII case-insensitive), or an error wrapping
// ErrUnsupportedCollation for a collation the reader cannot reproduce.
func CanonicalCollation(name string) (string, error) {
	if name == "" {
		return "BINARY", nil
	}
	for _, k := range []string{"BINARY", "NOCASE", "RTRIM"} {
		if asciiEqualFold(name, k) {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrUnsupportedCollation, name)
}

// ColumnCollation is the collation name that governs equality on column c:
// KeyCollation when set (the key clause governs the index), else Collation, as
// written ("" means none was declared).
func ColumnCollation(c Column) string {
	if c.KeyCollation != "" {
		return c.KeyCollation
	}
	return c.Collation
}

// KeyStatus says whether EqualityKey produced a key.
type KeyStatus uint8

const (
	KeyOK      KeyStatus = iota // a key was produced
	KeyNever                    // NULL or NaN: equal to nothing, itself included (the engine's rule)
	KeyUnknown                  // omitted, unread, clipped, or text the reader cannot fold
)

// EqualityKey is the canonical key of one value under a collation: two values
// have equal keys exactly when the engine compares them equal without affinity
// conversion. 1 and 1.0 are one key (an integral real in int64 range is the
// integer); text is compared under the collation (BINARY bytes, NOCASE with
// the ASCII letters folded, RTRIM without trailing spaces); a text and a blob
// never meet, and integers and text never meet. UTF-16 text is decoded before
// NOCASE or RTRIM folding and keyed by its UTF-8 bytes; BINARY keys the stored
// bytes. The collation is checked first: the error is non-nil only for an
// unsupported collation, never a silent fall back to BINARY.
func EqualityKey(v Value, collation string) (key [32]byte, st KeyStatus, err error) {
	coll, err := CanonicalCollation(collation)
	if err != nil {
		return key, KeyUnknown, err
	}
	h := sha256.New()
	if st = appendKey(h, v, coll, false); st != KeyOK {
		return key, st, nil
	}
	copy(key[:], h.Sum(nil))
	return key, KeyOK, nil
}

// keyWriter is the part of a hash the key encoder writes to.
type keyWriter interface{ Write(p []byte) (int, error) }

// appendKey writes the canonical encoding of v under the canonical collation
// coll and says whether it could. It writes nothing for a value without a key.
// legacy keeps the two behaviours of the history comparison that differ from
// the engine (open question Q13): a NaN is hashed by its bits (two NaNs are
// one key), and UTF-16 text under NOCASE or RTRIM has no key (and invalid
// UTF-8 is folded bytewise); NULL has no key in either mode.
func appendKey(w keyWriter, v Value, coll string, legacy bool) KeyStatus {
	var b [8]byte
	put := func(n uint64) {
		binary.BigEndian.PutUint64(b[:], n)
		_, _ = w.Write(b[:])
	}
	if v.Omitted || v.Clipped || (v.Unread && !legacy) {
		return KeyUnknown
	}
	switch v.Kind {
	case KindInt:
		_, _ = w.Write([]byte{'i'})
		put(uint64(v.Int))
	case KindFloat:
		f := v.Float
		switch {
		case math.IsNaN(f) && !legacy:
			return KeyNever
		case f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63:
			_, _ = w.Write([]byte{'i'}) // an integral real is the integer of the same value
			put(uint64(int64(f)))
		default:
			_, _ = w.Write([]byte{'f'})
			put(math.Float64bits(f))
		}
	case KindText, KindBlob:
		bs := v.Bytes
		if v.Kind == KindText && coll != "BINARY" {
			if legacy {
				if v.Enc != EncUTF8 {
					return KeyUnknown
				}
			} else {
				s, ok := v.Text() // decodes UTF-16; invalid text has no folded form
				if !ok {
					return KeyUnknown
				}
				bs = []byte(s)
			}
			switch coll {
			case "NOCASE":
				bs = slices.Clone(bs)
				for j, c := range bs {
					if c >= 'A' && c <= 'Z' {
						bs[j] = c + 32
					}
				}
			case "RTRIM":
				for len(bs) > 0 && bs[len(bs)-1] == ' ' {
					bs = bs[:len(bs)-1]
				}
			}
		}
		_, _ = w.Write([]byte{byte(v.Kind)})
		put(uint64(len(bs)))
		_, _ = w.Write(bs)
	case KindNull:
		if legacy {
			return KeyUnknown
		}
		return KeyNever // only NULL equals nothing
	default:
		return KeyUnknown // a Kind this library does not define is undecidable
	}
	return KeyOK
}
