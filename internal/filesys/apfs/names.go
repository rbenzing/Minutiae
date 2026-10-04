package apfs

import (
	"hash/crc32"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/unicode/rangetable"
)

// appleUnicodeVersion is the Unicode release whose character set APFS is
// understood to have frozen its normalization and case-folding tables at (9.0,
// from memory of the format reference; it is not read from any image). Where
// this reader's tables (Unicode 15.0 in golang.org/x/text) and Apple's differ:
//
//   - A character assigned after that release is hashed by Apple's tables as
//     itself (it is unknown there), while the newer tables here may give it a
//     case folding or a decomposition (for example Georgian Mtavruli letters,
//     Unicode 11.0, fold to Mkhedruli). A name holding such a character cannot
//     be hashed with confidence, so nameHash declines it (ok false): the stored
//     hash is neither verified nor trusted, and no warning is raised.
//   - For characters that existed in 9.0, normalization is stable by policy
//     (decompositions never change once assigned) and case folding only gains
//     entries for new characters, so both tables agree. Every one of the 678
//     names of the real populated fixture, written by a kernel driver and
//     covering Latin, Greek, Cyrillic, Hangul, Japanese, emoji, ss, dotted I
//     and titlecase folds, hashes identically (TestPopulatedFixtureNameHashesAllVerified).
const appleUnicodeVersion = "9.0.0"

// appleAssigned holds the code points assigned in appleUnicodeVersion. It is nil
// only if the tables of that version are missing from golang.org/x/text, in
// which case no non-ASCII name is verified.
var appleAssigned = rangetable.Assigned(appleUnicodeVersion)

// castagnoli is the CRC-32C table.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// isASCII reports whether s holds only bytes below 0x80.
func isASCII(s []byte) bool {
	for _, c := range s {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// matchKey is the form of a valid UTF-8 name that the volume compares: NFD
// (normalization-insensitive volumes), and with foldCase also Unicode full case
// folding (C+F), then NFD again as folding can leave the form (the same steps
// the hash is taken over). On ASCII, NFD is the identity and the fold is
// lower-casing.
func matchKey(name []byte, foldCase bool) string {
	if isASCII(name) {
		if !foldCase {
			return string(name)
		}
		b := make([]byte, len(name))
		for i, c := range name {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b[i] = c
		}
		return string(b)
	}
	s := norm.NFD.String(string(name))
	if foldCase {
		s = norm.NFD.String(cases.Fold().String(s))
	}
	return s
}

// nameHash is the 22-bit hash a hashed directory-record key stores for name
// (Format reference: the name in NFD, as UTF-32 little-endian code points
// without the NUL, CRC-32C with initial value 0xFFFFFFFF and no final
// complement, low 22 bits; a case-insensitive volume hashes the case-folded
// name). The recipe is validated on every name of a real kernel-written image,
// non-ASCII ones included (see appleUnicodeVersion). ok is false for a name it
// does not cover: not valid UTF-8, or holding a character newer than the
// Unicode version APFS froze its tables at.
func nameHash(name []byte, foldCase bool) (hash uint32, ok bool) {
	if !isASCII(name) {
		if !utf8.Valid(name) || appleAssigned == nil {
			return 0, false
		}
		for _, r := range string(name) {
			if !unicode.Is(appleAssigned, r) {
				return 0, false
			}
		}
	}
	key := matchKey(name, foldCase)
	u := make([]byte, 0, 4*len(key))
	for _, r := range key {
		u = append(u, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
	}
	// hash/crc32 complements the result; the stored value does not.
	return ^crc32.Checksum(u, castagnoli) & 0x3fffff, true
}
