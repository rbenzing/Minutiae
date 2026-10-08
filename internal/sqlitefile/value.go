package sqlitefile

import (
	"unicode/utf8"
)

// Kind is the storage class of a Value.
type Kind uint8

// The storage classes. KindNull is the zero value.
const (
	KindNull Kind = iota
	KindInt
	KindFloat
	KindText
	KindBlob
)

// Value is one column of a record. The invariants (the Format reference,
// "Value invariants"): Len is the byte length of a text or blob value in the
// record and 0 for null, integer and float values; Omitted means the value
// was not materialized (over a cap, bytes in an unreadable part, a column
// nobody asked for), and an omitted value has Bytes == nil while Len keeps
// the true length of a text or blob. For text and blob exactly one of these
// holds: Omitted; len(Bytes) == Len; Clipped with len(Bytes) < Len. Clipped is
// set only for recovered (history) values.
type Value struct {
	Kind    Kind
	Int     int64
	Float   float64
	Bytes   []byte // raw text (database encoding) or blob; nil when Omitted
	Len     int64
	Omitted bool
	// Unread: the value is stored but its bytes could not be read (the record or
	// its overflow chain is damaged), as opposed to a value that is not stored at
	// all (a column past the end of a short record, a virtual generated column).
	// An unread value is inconclusive: it is never equal or different.
	Unread  bool
	Clipped bool
	Enc     Encoding // for KindText
	Serial  uint64   // the serial type the record stored it with
}

// Text returns a text value as a UTF-8 string. ok is false for a value that
// is not text, is omitted, or whose bytes are not valid in its encoding
// (invalid UTF-8, an odd byte count or an unpaired surrogate in UTF-16): such
// text is never repaired, Bytes keeps the raw bytes. A BOM is kept as U+FEFF.
// A clipped value is decoded as far as it goes, so check Clipped first.
func (v Value) Text() (string, bool) {
	if v.Kind != KindText || v.Omitted {
		return "", false
	}
	switch v.Enc {
	case EncUTF16LE:
		return decodeUTF16(v.Bytes, false)
	case EncUTF16BE:
		return decodeUTF16(v.Bytes, true)
	}
	if !utf8.Valid(v.Bytes) {
		return "", false
	}
	return string(v.Bytes), true
}

// decodeUTF16 decodes strict UTF-16: an odd byte count, a high surrogate not
// followed by a low one and a lone low surrogate are all rejected.
func decodeUTF16(b []byte, bigEndian bool) (string, bool) {
	if len(b)%2 != 0 {
		return "", false
	}
	unit := func(i int) rune {
		if bigEndian {
			return rune(b[i])<<8 | rune(b[i+1])
		}
		return rune(b[i+1])<<8 | rune(b[i])
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i += 2 {
		u := unit(i)
		switch {
		case u >= 0xd800 && u < 0xdc00: // high surrogate: a low one must follow
			if i+2 >= len(b) {
				return "", false
			}
			lo := unit(i + 2)
			if lo < 0xdc00 || lo >= 0xe000 {
				return "", false
			}
			u = 0x10000 + (u-0xd800)<<10 + (lo - 0xdc00)
			i += 2
		case u >= 0xdc00 && u < 0xe000: // lone low surrogate
			return "", false
		}
		out = utf8.AppendRune(out, u)
	}
	return string(out), true
}
