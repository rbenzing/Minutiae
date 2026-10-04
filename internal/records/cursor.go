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
	// under 100 bytes.
	maxCursorLen = 256
)

// cursor is the keyset position after the last row of a page: its direction,
// whether its ts was NULL, its ts (0 when NULL) and its id. The wire form is
// "v1." + base64url(JSON{d,n,ts,id}), opaque to callers.
//
// The cursor holds position only, not the filter: a cursor used with another
// filter simply continues that filter's order from the same (ts, id).
type cursor struct {
	D  int   `json:"d"` // 1 = descending
	N  int   `json:"n"` // 1 = the last row's ts is NULL
	TS int64 `json:"ts"`
	ID int64 `json:"id"`
}

// encode returns the wire form.
func (c cursor) encode() string {
	b, err := json.Marshal(c)
	if err != nil { // cannot happen: four integers
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
	}
	return ""
}

// decodeCursor parses and validates a cursor. Anything that is not exactly what
// encode produces (another version, padding, whitespace, reordered, duplicated,
// missing or extra fields, out-of-range values) is ErrBadCursor: a successful
// decode always re-encodes to the same string, and nothing a cursor carries is
// ever more than two int64 bind values.
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
		D  *int   `json:"d"`
		N  *int   `json:"n"`
		TS *int64 `json:"ts"`
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return bad("not the expected JSON")
	}
	if w.D == nil || w.N == nil || w.TS == nil || w.ID == nil {
		return bad("a field is missing")
	}
	c := cursor{D: *w.D, N: *w.N, TS: *w.TS, ID: *w.ID}
	if why := c.invalid(); why != "" {
		return bad(why)
	}
	if c.encode() != s {
		return bad("not in canonical form")
	}
	return c, nil
}
