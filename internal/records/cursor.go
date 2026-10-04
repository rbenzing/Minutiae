package records

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	cursorPrefix = "v1."
	// maxCursorLen bounds the work a forged cursor can cause: a real one is
	// under 200 bytes.
	maxCursorLen = 256
	// fingerprintLen is the length of a listing fingerprint in hex digits (128
	// bits of SHA-256: a guard against a mistake, not against a forger, who can
	// read the fingerprint off any cursor and gains nothing by reusing it).
	fingerprintLen = 32
)

// cursor is the keyset position after the last row of a page: its direction,
// whether its ts was NULL, its ts (0 when NULL) and its id, bound to the listing
// that produced it by F. The wire form is "v1." + base64url(JSON{d,n,ts,id,f}),
// opaque to callers.
//
// F is the fingerprint of the listing (see Filter.fingerprint): the case, the
// canonical filter, the order and the direction. A cursor used with another
// filter, order, direction or case is ErrBadCursor, never the wrong page.
type cursor struct {
	D  int    `json:"d"` // 1 = descending
	N  int    `json:"n"` // 1 = the last row's ts is NULL
	TS int64  `json:"ts"`
	ID int64  `json:"id"`
	F  string `json:"f"` // fingerprintLen lower-case hex digits
}

// encode returns the wire form.
func (c cursor) encode() string {
	b, err := json.Marshal(c)
	if err != nil { // cannot happen: four integers and a string
		panic(fmt.Sprintf("records: encode cursor: %v", err))
	}
	return cursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// invalid reports why c cannot be a cursor this reader produced, or "".
func (c cursor) invalid() string {
	switch {
	case c.D != 0 && c.D != 1:
		return "direction is not 0 or 1"
	case c.N != 0 && c.N != 1:
		return "null flag is not 0 or 1"
	case c.N == 1 && c.TS != 0:
		return "an untimed position carries a timestamp"
	case c.ID < 1:
		return "id is not positive"
	case !validFingerprint(c.F):
		return "listing fingerprint is not 32 lower-case hex digits"
	}
	return ""
}

func validFingerprint(s string) bool {
	if len(s) != fingerprintLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// decodeCursor parses and validates a cursor. Anything that is not exactly what
// encode produces (another version, padding, whitespace, reordered, duplicated,
// missing or extra fields, out-of-range values) is ErrBadCursor: a successful
// decode always re-encodes to the same string, and nothing a cursor carries is
// ever more than two int64 bind values and a fingerprint it is compared with.
func decodeCursor(s string) (cursor, error) {
	bad := func(why string) (cursor, error) { return cursor{}, fmt.Errorf("%w: %s", ErrBadCursor, why) }
	if len(s) > maxCursorLen {
		return bad("too long")
	}
	body, ok := strings.CutPrefix(s, cursorPrefix)
	if !ok {
		return bad("unknown version")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return bad("not base64url")
	}
	var w struct {
		D  *int    `json:"d"`
		N  *int    `json:"n"`
		TS *int64  `json:"ts"`
		ID *int64  `json:"id"`
		F  *string `json:"f"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return bad("not the expected JSON")
	}
	if w.D == nil || w.N == nil || w.TS == nil || w.ID == nil || w.F == nil {
		return bad("a field is missing")
	}
	c := cursor{D: *w.D, N: *w.N, TS: *w.TS, ID: *w.ID, F: *w.F}
	if why := c.invalid(); why != "" {
		return bad(why)
	}
	if c.encode() != s {
		return bad("not in canonical form")
	}
	return c, nil
}
