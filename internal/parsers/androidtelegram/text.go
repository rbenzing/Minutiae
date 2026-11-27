package androidtelegram

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of spec 8.5 and 12 (C4): free texts are cut and say so, identifiers never are. (The plan's maxList, the cap
// of the per-row warnings, comes with its first user, the users mapping of Task 12.)
const (
	maxBody  = 256 << 10 // the message text (Body)
	maxIdent = 1 << 10   // an identifier (a username, a phone, an id as text): never cut, over it absent
)

// stateTooLong is the state of an identifier over its limit.
const stateTooLong = "too-long"

// ident is an identifier as stored, never cut (spec 12, C4; restated from androidcalendar/text.go, F29): within limit
// it is returned cleaned (each invalid byte and NUL as U+FFFD); over it, nothing is returned and raw states
// <key>_state "too-long", <key>_len (of the stored bytes) and <key>_sha256 (of the stored bytes). The limit holds
// for the cleaned text, so a stored text within it whose invalid bytes grow past it is too long as well. A stored
// text over the limit is refused before any copy.
func ident(raw map[string]any, key string, b []byte, limit int) (string, bool) {
	if s, _, ok := identClean(b, limit); ok {
		return s, true
	}
	raw[key+"_state"] = stateTooLong
	raw[key+"_len"] = int64(len(b))
	raw[key+"_sha256"] = sha256Hex(b)
	return "", false
}

// identClean is the shared core of ident and the poll's country codes: the cleaned text of b and whether the cleaning
// flagged anything, ok false when b is over limit (tested first, so nothing is copied) or its cleaning grows past it.
func identClean(b []byte, limit int) (text string, flagged, ok bool) {
	if len(b) > limit {
		return "", false, false
	}
	c := common.CleanText(b, limit)
	if c.Truncated {
		return "", false, false
	}
	return c.Text, len(c.Flags) > 0, true
}

// sha256Hex is the lower-case hex SHA-256 of b: the digest raw keeps beside a blob or an identifier it does not hold
// whole (spec 8.4, 12).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
