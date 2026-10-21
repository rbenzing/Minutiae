package filesys

import (
	"encoding/base64"
	"strings"
)

// DecodeRawNameAlias returns the bytes s stands for when s is exactly a
// RawNameAlias output, and false for anything else: a missing prefix, an empty
// payload, padding, the standard alphabet, embedded CR or LF, or a
// non-canonical spelling (trailing bits set). The text must be what encoding
// the decoded bytes gives, so each name has one spelling. (The decoder skips
// embedded CR and LF even in strict mode; the round trip rejects them.)
func DecodeRawNameAlias(s string) (raw []byte, ok bool) {
	rest, found := strings.CutPrefix(s, RawNamePrefix)
	if !found || rest == "" {
		return nil, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(rest)
	if err != nil || len(b) == 0 || base64.RawURLEncoding.EncodeToString(b) != rest {
		return nil, false
	}
	return b, true
}
