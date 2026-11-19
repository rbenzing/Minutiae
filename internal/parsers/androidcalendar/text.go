package androidcalendar

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of spec 4.1 and 9: free texts are cut (and say so), identifiers never.
const (
	maxTitle    = 4 << 10   // title and location text
	maxBody     = 256 << 10 // the description
	maxZone     = 256       // eventTimezone, eventEndTimezone
	maxID       = 1 << 10   // uid2445, _sync_id, original_sync_id, organizer
	maxRule     = 16 << 10  // rrule, rdate, exrule, exdate
	maxSyncData = 1 << 10   // sync_data1..10, raw only
)

const stateTooLong = "too-long"

// ident is an identifier as stored, never cut (spec 9): within limit it is returned (valid UTF-8, each byte that
// starts no valid sequence replaced by U+FFFD); over it, nothing is returned and raw states <col>_state "too-long",
// <col>_len and the SHA-256 of the stored bytes in <col>_sha256.
func ident(raw map[string]any, col string, b []byte, limit int) (string, bool) {
	if len(b) > limit {
		raw[col+"_state"] = stateTooLong
		raw[col+"_len"] = int64(len(b))
		raw[col+"_sha256"] = sha256Hex(b)
		return "", false
	}
	return validText(b), true
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// cells reads the cells of one Events row into raw and unknown (payload.raw and raw.unknown_columns): a cell that
// holds no usable value is listed in unknown, never read as absent.
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

// ident reads an identifier column through ident; "" when absent, empty, too long or unreadable.
func (r cells) ident(name string, limit int) string {
	b, ok := r.text(name)
	if !ok {
		return ""
	}
	s, _ := ident(r.raw, name, b, limit)
	return s
}

// stored is the stored value of a cell of any class as raw keeps it (C1): an INTEGER as itself, else storedOther; an
// unreadable cell is listed in unknown and gives nothing, a NULL or a missing column nothing.
func (r cells) stored(name string) (any, bool) {
	col := r.c.At(name)
	if col < 0 {
		return nil, false
	}
	if !rowrun.StateOf(r.row, col).Known() || nanCell(r.row, col) {
		k := rowrun.CellUnknown
		if nanCell(r.row, col) {
			k = rowrun.CellOther
		}
		rowrun.Flag(r.unknown, r.row, r.prefix+name, col, k)
		return nil, false
	}
	if r.row.IsNull(col) {
		return nil, false
	}
	if v, ok := r.row.Int(col); ok {
		return v, true
	}
	return storedOther(r.row, col)
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

// undecidedJoin lists a join in raw.join_undecided (sorted, once).
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
