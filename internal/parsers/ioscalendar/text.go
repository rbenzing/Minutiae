package ioscalendar

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of spec 4.1 and 9: free texts are cut (and say so), identifiers never; maxStored bounds a value kept in
// raw only as stored (a rule's specifier, a code of another class), which is never cut either.
const (
	maxTitle  = 4 << 10   // title, location text and address, names
	maxBody   = 256 << 10 // the description
	maxID     = 1 << 10   // url, unique_identifier, external_id, participant addresses
	maxStored = 16 << 10  // a TEXT or BLOB kept in raw as stored
	maxList   = 1000      // attendees, alarms, recurrence rules, exception dates per event (the telephony list cap)
)

const stateTooLong = "too-long"

// ident is an identifier as stored, never cut (spec 9), restated from android.calendar (parsers share no code): within
// limit it is returned (valid UTF-8, each byte that starts no valid sequence replaced by U+FFFD); over it, nothing is
// returned and raw states <col>_state "too-long", <col>_len (of the stored bytes) and the SHA-256 of the stored bytes in
// <col>_sha256. The limit holds for the text the payload would carry.
func ident(raw map[string]any, col string, b []byte, limit int) (string, bool) {
	s, ok := cleanWithin(b, limit)
	if !ok {
		raw[col+"_state"] = stateTooLong
		raw[col+"_len"] = int64(len(b))
		raw[col+"_sha256"] = sha256Hex(b)
		return "", false
	}
	return s, true
}

// cleanWithin is validText(b) when that text is at most limit bytes, else false. validText never shrinks its input, so
// a stored text over the limit is refused before any copy; one within it is cleaned and checked again, because each
// invalid byte grows to the three bytes of U+FFFD.
func cleanWithin(b []byte, limit int) (string, bool) {
	if len(b) > limit {
		return "", false
	}
	s := validText(b)
	if len(s) > limit {
		return "", false
	}
	return s, true
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// cells reads the cells of one row into raw and unknown (payload.raw and raw.unknown_columns): a cell that holds no
// usable value is listed in unknown as "<prefix><col>", never read as absent.
type cells struct {
	row          parse.Row
	c            rowrun.Cols
	raw, unknown map[string]any
	prefix       string
}

// text reads a TEXT cell: its bytes for a value (maybe empty), nil for NULL or a missing column; a cell of another
// class or an unreadable one is listed in unknown.
func (r cells) text(name string) ([]byte, bool) {
	col := r.c.At(name)
	b, k := textCellOf(r.row, col)
	rowrun.Flag(r.unknown, r.row, r.prefix+name, col, k)
	return b, k == rowrun.CellValue
}

// ident reads an identifier column through ident into raw; "" when absent, empty, too long or unreadable.
func (r cells) ident(name string, limit int) string {
	b, ok := r.text(name)
	if !ok {
		return ""
	}
	s, _ := ident(r.raw, name, b, limit)
	return s
}

// key reads a join key: its INTEGER and true; false for NULL or a missing column (no key: the join states nothing);
// undecided for a cell of another class or an unreadable one, which is listed in unknown.
func (r cells) key(name string) (v int64, ok, undecided bool) {
	col := r.c.At(name)
	v, k := intCellOf(r.row, col)
	switch k {
	case rowrun.CellValue:
		return v, true, false
	case rowrun.CellOther, rowrun.CellUnknown:
		rowrun.Flag(r.unknown, r.row, r.prefix+name, col, k)
		return 0, false, true
	}
	return 0, false, false
}

// stored is the stored value of a cell of any class as raw keeps it (C1): an INTEGER as itself, a TEXT or a BLOB of at
// most maxStored bytes as storedOther does, a finite REAL as its number; an unreadable cell (a stored NaN included) is
// listed in unknown and gives nothing, a NULL or a missing column nothing. A TEXT or BLOB over maxStored is not kept:
// it is reported through over (its length and SHA-256), never cut.
func (r cells) stored(name string) (v any, over []byte, ok bool) {
	col := r.c.At(name)
	if col < 0 {
		return nil, nil, false
	}
	if !rowrun.StateOf(r.row, col).Known() || nanCell(r.row, col) {
		k := rowrun.CellUnknown
		if nanCell(r.row, col) {
			k = rowrun.CellOther
		}
		rowrun.Flag(r.unknown, r.row, r.prefix+name, col, k)
		return nil, nil, false
	}
	if r.row.IsNull(col) {
		return nil, nil, false
	}
	if v, ok := r.row.Int(col); ok {
		return v, nil, true
	}
	if b, ok := r.row.Text(col); ok && len(b) > maxStored {
		return nil, b, false
	}
	if b, ok := r.row.Blob(col); ok && len(b) > maxStored {
		return nil, b, false
	}
	v, ok = storedOther(r.row, col)
	return v, nil, ok
}

// keep states the stored value of column name in dst under key (putEntry): a value over maxStored states
// <key>_state "too-long", <key>_len and <key>_sha256 instead.
func (r cells) keep(dst map[string]any, key, name string) {
	v, over, ok := r.stored(name)
	switch {
	case over != nil:
		dst[key+"_state"] = stateTooLong
		dst[key+"_len"] = int64(len(over))
		dst[key+"_sha256"] = sha256Hex(over)
	case ok:
		putEntry(dst, key, v)
	}
}

// keepTop states the stored value of column name in raw under its own name, as raw keeps a value at its top level
// (a BLOB as {"blob_b64": ...}, C1); a value over maxStored states <name>_state "too-long", <name>_len and
// <name>_sha256 instead.
func (r cells) keepTop(name string) {
	if v, _, ok := r.stored(name); ok {
		r.raw[name] = v
		return
	}
	r.keep(r.raw, name, name)
}

// putEntry keeps a stored value v in dst under key. A child entry is a map inside a raw list (level 4), so it holds
// scalars only (B8): a BLOB, which raw elsewhere states as {"blob_b64": ...}, is flattened to the sibling scalar
// <key>_blob_b64. Restated from android.calendar.
func putEntry(dst map[string]any, key string, v any) {
	if b, ok := v.(map[string]any); ok {
		if s, ok := b["blob_b64"].(string); ok {
			dst[key+"_blob_b64"] = s
		}
		return
	}
	dst[key] = v
}

// textFlags is the set of free-text fields CleanText altered or cut, written as raw.text_flags (one comma-joined
// string, B8).
type textFlags map[string]bool

func (t textFlags) write(raw map[string]any) {
	if len(t) == 0 {
		return
	}
	names := make([]string, 0, len(t))
	for n := range t {
		names = append(names, n)
	}
	slices.Sort(names)
	raw["text_flags"] = strings.Join(names, ",")
}

// freeText cleans a free text (C4) at limit: an altered or cut text is named in flags, a cut one in truncated; the
// original bytes of an altered text are kept in raw.<field>_raw_b64 (or raw.<field>_raw_sha256 when large).
func freeText(raw map[string]any, flags textFlags, truncated *[]string, field string, b []byte, limit int) string {
	c := common.CleanText(b, limit)
	if len(c.Flags) > 0 {
		flags[field] = true
	}
	if c.Truncated {
		*truncated = append(*truncated, field)
	}
	switch {
	case c.RawB64 != "":
		raw[field+"_raw_b64"] = c.RawB64
	case c.RawSHA256 != "":
		raw[field+"_raw_sha256"] = c.RawSHA256
	}
	return c.Text
}

// undecidedJoin lists a join table in raw.join_undecided (sorted, once).
func undecidedJoin(raw map[string]any, table string) {
	cur, _ := raw["join_undecided"].([]any)
	for _, j := range cur {
		if j == table {
			return
		}
	}
	cur = append(cur, table)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["join_undecided"] = cur
}

// capCount is the _total and _truncated pair of a list of n items cut at maxList (C6): nil and false within the cap.
func capCount(n int) (*int64, bool) {
	if n <= maxList {
		return nil, false
	}
	t := int64(n)
	return &t, true
}

// putList states a raw list cut at maxList with its _total and _truncated when n exceeds the cap (C6).
func putList(raw map[string]any, key string, list []any, n int) {
	if len(list) == 0 {
		return
	}
	raw[key] = list
	if total, cut := capCount(n); cut {
		raw[key+"_total"] = *total
		raw[key+"_truncated"] = true
	}
}
