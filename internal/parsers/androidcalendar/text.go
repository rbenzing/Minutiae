package androidcalendar

import (
	"crypto/sha256"
	"fmt"
	"math"
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
// <col>_len (of the stored bytes) and the SHA-256 of the stored bytes in <col>_sha256. The limit holds for the text the
// payload would carry: a stored text within it whose invalid bytes grow past it as U+FFFD is too long as well
// (FuzzMapAndroidCalendarRows).
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
// a stored text over the limit is refused before any copy (a 1 MiB hostile cell costs nothing); one within it is
// cleaned and checked again, because each invalid byte grows to the three bytes of U+FFFD.
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
	flagCell(r.unknown, r.row, r.prefix+name, col, k)
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

// stored is the stored value of a cell of any class as raw keeps it (C1): an INTEGER as itself, else storedOther; a
// non-finite REAL (a stored NaN, an infinity) in its display form "NaN", "+Inf" or "-Inf", also listed in unknown as
// type-mismatch, never dropped (task 9 fix round 1b, A1); an unreadable cell is listed in unknown and gives nothing, a
// NULL or a missing column nothing.
func (r cells) stored(name string) (any, bool) {
	col := r.c.At(name)
	if col < 0 {
		return nil, false
	}
	if nanCell(r.row, col) {
		flagCell(r.unknown, r.row, r.prefix+name, col, rowrun.CellOther)
		return "NaN", true
	}
	if !rowrun.StateOf(r.row, col).Known() {
		flagCell(r.unknown, r.row, r.prefix+name, col, rowrun.CellUnknown)
		return nil, false
	}
	if r.row.IsNull(col) {
		return nil, false
	}
	if f, ok := r.row.Float(col); ok && math.IsInf(f, 0) {
		listUnknown(r.unknown, r.prefix+name, "type-mismatch")
		if f > 0 {
			return "+Inf", true
		}
		return "-Inf", true
	}
	if v, ok := r.row.Int(col); ok {
		return v, true
	}
	return storedOther(r.row, col)
}

// flagCell lists in unknown (payload.raw.unknown_columns) a column that exists but holds no usable value, as
// rowrun.Flag does ("type-mismatch" for a value of another class, the decoder's state name for an unknown cell), but
// never overwrites: the kinds a column shows across the rows of a record (a child table's rows) are comma-joined and
// sorted, as the plan's vocabulary states (task 9 fix round 1b, A1).
func flagCell(unknown map[string]any, row parse.Row, name string, col int, k rowrun.CellKind) {
	switch k {
	case rowrun.CellOther:
		listUnknown(unknown, name, "type-mismatch")
	case rowrun.CellUnknown:
		listUnknown(unknown, name, rowrun.StateOf(row, col).String())
	}
}

// listUnknown adds kind to the comma-joined, sorted kinds of unknown[name].
func listUnknown(unknown map[string]any, name, kind string) {
	cur, _ := unknown[name].(string)
	kinds := []string{kind}
	if cur != "" {
		kinds = append(strings.Split(cur, ","), kind)
	}
	slices.Sort(kinds)
	unknown[name] = strings.Join(slices.Compact(kinds), ",")
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
