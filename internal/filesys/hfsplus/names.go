package hfsplus

import (
	"unicode"
	"unicode/utf16"
)

// maxNameUnits is the longest HFSUniStr255 name: 255 UTF-16 code units.
const maxNameUnits = 255

// ignorable reports whether a UTF-16 code unit is skipped by the HFS+ case
// folding comparison: the zero-width and bidirectional controls U+200C-200F,
// U+202A-202E, U+206A-206F and U+FEFF (TN1150 "Unicode Subtleties", from memory;
// NOT validated against a real image, the fixtures hold no such names).
//
// U+0000 is NOT ignorable: Apple's case-folding table maps it to 0xFFFF, so it
// sorts after every other unit (this is why the hard-link private folder, whose
// name starts with four NULs, is the last child of the root). This is the
// coordinator's ruling from two independent recollections plus the
// FastUnicodeCompare table; it is UNVERIFIED (no real image holds such a name).
// An earlier version skipped U+0000 like the others. The reader does not rely
// on the order for the private folder: when the descent misses it scans the
// root (see privateFolder). The effect elsewhere is limited to name lookups on
// case-folding volumes: Lookup tries an exact (binary) spelling before the
// folded one, so a name that exists as typed is always found.
func ignorable(u uint16) bool {
	switch {
	case u == 0xFEFF:
		return true
	case u >= 0x200C && u <= 0x200F:
		return true
	case u >= 0x202A && u <= 0x202E:
		return true
	case u >= 0x206A && u <= 0x206F:
		return true
	}
	return false
}

// foldUnit lower-cases one BMP code unit (U+0000 becomes 0xFFFF, as Apple's table has it). Surrogates and everything whose
// lower case is outside the BMP are left unchanged. Apple's real fold is a
// 64 K-entry table that is not embedded here, so this is an APPROXIMATION
// (unicode.ToLower per unit): it agrees for ASCII, Latin-1, Greek and Cyrillic
// and may differ for exotic letters (Go's case data follows a newer Unicode version than
// the table frozen in HFS+, which Lookup's exact-before-folded order mitigates).
func foldUnit(u uint16) uint16 {
	if u == 0 {
		return 0xFFFF // unverified, see ignorable
	}
	if u < 0x80 {
		if u >= 'A' && u <= 'Z' {
			return u + 'a' - 'A'
		}
		return u
	}
	if u >= 0xD800 && u <= 0xDFFF {
		return u
	}
	if l := unicode.ToLower(rune(u)); l >= 0 && l <= 0xFFFF {
		return uint16(l)
	}
	return u
}

// fold returns the case-folded form of a name: ignorable units removed, the
// rest lower-cased per code unit. The input is not modified.
func fold(units []uint16) []uint16 {
	out := make([]uint16, 0, len(units))
	for _, u := range units {
		if ignorable(u) {
			continue
		}
		out = append(out, foldUnit(u))
	}
	return out
}

// compareUnits orders two unit sequences lexicographically by unit value
// (a prefix sorts first): the HFSX binary order.
func compareUnits(a, b []uint16) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// compareNames orders two catalog names: by raw unit value when binary (HFSX
// case-sensitive volumes), else after case folding.
func compareNames(a, b []uint16, binary bool) int {
	if binary {
		return compareUnits(a, b)
	}
	return compareUnits(fold(a), fold(b))
}

// catalogKey is a decoded catalog B-tree key: the parent folder id and the
// name in UTF-16 code units. A thread record's key has an empty name.
type catalogKey struct {
	parent uint32
	name   []uint16
}

// compareKeys orders catalog keys: parent id first, then the name.
func compareKeys(a, b catalogKey, binary bool) int {
	switch {
	case a.parent < b.parent:
		return -1
	case a.parent > b.parent:
		return 1
	}
	return compareNames(a.name, b.name, binary)
}

// decodeUnits converts UTF-16 code units to a string. ok is false when the
// units are not valid UTF-16 (an unpaired surrogate): the string then holds
// U+FFFD for each bad unit and callers choose the raw-name form.
func decodeUnits(units []uint16) (s string, ok bool) {
	ok = true
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u < 0xDC00:
			if i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] < 0xE000 {
				i++
			} else {
				ok = false
			}
		case u >= 0xDC00 && u < 0xE000:
			ok = false
		}
	}
	return string(utf16.Decode(units)), ok
}
