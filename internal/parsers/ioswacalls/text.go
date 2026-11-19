package ioswacalls

import (
	"math"
	"strconv"
	"time"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

const (
	maxText     = 256 << 10     // bytes of text kept; the rest is flagged
	maxDuration = 1_000_000_000 // seconds: a longer call has a duration but no end time (restated from ios.calls)
)

// The tables the mapping reads, and the prefix under which one of their unreadable columns is listed in raw.unknown_columns
// (4I-R22: "<table>.<column>").
const (
	tableCall    = "ZWACDCALLEVENT"
	tableAgg     = "ZWAAGGREGATECALLEVENT"
	prefixCall   = tableCall + "."
	prefixAgg    = tableAgg + "."
	channelWA    = "whatsapp"
	stateUndec   = "undecided"
	stateRecover = "recovered-row"
)

// cellReader reads the cells of one row into raw and unknown (payload.raw and payload.raw.unknown_columns); a cell that holds no
// usable value is stated in unknown, never read as 0 or as empty.
type cellReader struct {
	row     parse.Row
	cols    rowrun.Cols
	raw     map[string]any
	unknown map[string]any
	prefix  string
}

// int reads an INTEGER cell.
func (r cellReader) int(name string) (int64, bool) {
	v, k := intCellOf(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// text reads a TEXT cell; the bytes may be empty (a stored empty string is a value).
func (r cellReader) text(name string) ([]byte, bool) {
	v, k := textCellOf(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// number reads an INTEGER or REAL cell.
func (r cellReader) number(name string) (cellx.Number, bool) {
	v, k := cellx.NumberCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// stored returns the stored value of a cell of any storage class for raw: an integer, a cleaned text, a finite float, the string
// "non-finite" or a blob marker; nil for NULL, an absent column and an unreadable cell. A text of the aggregate row that
// cleaning changed is listed in raw.text_flags as "aggregate.<column>" (raw.aggregate holds it).
func (r cellReader) stored(name string) any {
	col := r.cols.At(name)
	if col < 0 || r.row.IsNull(col) || !rowrun.StateOf(r.row, col).Known() {
		return nil
	}
	if v, ok := r.row.Int(col); ok {
		return v
	}
	if b, ok := r.row.Text(col); ok {
		field := name
		if r.prefix == prefixAgg {
			field = "aggregate." + name
		}
		return cleanField(r.raw, field, b)
	}
	if f, ok := r.row.Float(col); ok {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "non-finite"
		}
		return f
	}
	if b, ok := r.row.Blob(col); ok {
		return blobValue(b)
	}
	return nil
}

// code is a stored enum cell: its value when it is a usable integer.
type code struct {
	Column string
	Value  int64
	OK     bool
}

// label is the research label of a usable code, and whether the research supports the value.
func (c code) label() (string, bool) {
	if !c.OK {
		return "", false
	}
	return lookupEnum(c.Column, c.Value)
}

// readCode reads a code column into raw as stored. A cell that is not an INTEGER is stated in unknown_columns, and an unusable
// cell keeps its stored value in raw and never gets a label.
func readCode(r cellReader, name string) code {
	c := code{Column: name}
	c.Value, c.OK = r.int(name)
	if v := r.stored(name); v != nil {
		r.raw[name] = v
	}
	return c
}

// cleanField is common.CleanText for a free-text field with no flag slot of its own; an altered text is listed in
// raw.text_flags.
func cleanField(raw map[string]any, field string, b []byte) string {
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	return cl.Text
}

// markText lists field in raw.text_flags (sorted).
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	for _, f := range cur {
		if f == field {
			return
		}
	}
	cur = append(cur, field)
	for i := len(cur) - 1; i > 0 && cur[i].(string) < cur[i-1].(string); i-- {
		cur[i], cur[i-1] = cur[i-1], cur[i]
	}
	raw["text_flags"] = cur
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
	for i := len(cur) - 1; i > 0 && cur[i].(string) < cur[i-1].(string); i-- {
		cur[i], cur[i-1] = cur[i-1], cur[i]
	}
	raw["join_undecided"] = cur
}

// nanCell says a cell is a stored REAL NaN: the reader (as the engine) reads it as NULL but keeps the serial type 7, so it is
// detectable here and is never read as absent (unknown is never absent).
func nanCell(row parse.Row, col int) bool {
	r, ok := row.(sqlitedb.Row)
	return ok && col >= 0 && r.State(col) == sqlitedb.StateNull && r.Value(col).Serial == 7
}

// intCellOf is rowrun.IntCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func intCellOf(row parse.Row, col int) (int64, rowrun.CellKind) {
	v, k := rowrun.IntCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return v, k
}

// textCellOf is rowrun.TextCell with a stored NaN read as a value of another class (CellOther), never as NULL.
func textCellOf(row parse.Row, col int) ([]byte, rowrun.CellKind) {
	b, k := rowrun.TextCell(row, col)
	if k == rowrun.CellNull && nanCell(row, col) {
		k = rowrun.CellOther
	}
	return b, k
}

// nanStatus states a column that holds a NaN as raw.<name>_status = "invalid" (no time, no stored value to show).
func (r cellReader) nanStatus(name string) {
	if nanCell(r.row, r.cols.At(name)) {
		r.raw[name+"_status"] = ts.StatusInvalid.String()
	}
}

// duration reads ZDURATION through cellx. A finite value >= 0 is returned (it is not stated in raw: the payload's duration_s is
// the stored value); a negative one is stated in raw.ZDURATION with raw.ZDURATION_status "negative"; a non-finite REAL by its
// IEEE bits (raw.ZDURATION_ieee, status "invalid", no raw.ZDURATION key); a stored NaN, which the reader gives as NULL with its
// serial type, is status "invalid"; an INTEGER beyond 2^53 is not a float64, so it is unknown ("precision") and stays in raw
// (restated from ios.calls).
func duration(r cellReader) (float64, bool) {
	const col = "ZDURATION"
	n, ok := r.number(col)
	if !ok {
		r.nanStatus(col)
		return 0, false
	}
	switch {
	case !n.IsInt && (math.IsNaN(n.F) || math.IsInf(n.F, 0)):
		r.raw[col+"_ieee"] = strconv.FormatUint(math.Float64bits(n.F), 16)
		r.raw[col+"_status"] = ts.StatusInvalid.String()
		return 0, false
	case (n.IsInt && n.I < 0) || (!n.IsInt && n.F < 0):
		r.raw[col] = n.Raw()
		r.raw[col+"_status"] = "negative"
		return 0, false
	}
	f, ok := n.Float()
	if !ok {
		r.unknown[r.prefix+col] = "precision"
		r.raw[col] = n.Raw()
		return 0, false
	}
	if f == 0 {
		f = 0 // never -0
	}
	return f, true
}

// endTime is start + duration for 0 < duration <= 1e9 seconds, and only while the result is a representable time; otherwise
// there is no end.
func endTime(start time.Time, dur float64) *records.Time {
	if !(dur > 0 && dur <= maxDuration) {
		return nil
	}
	end := start.Add(time.Duration(math.Round(dur * 1e9)))
	if !end.Before(ts.Max()) {
		return nil
	}
	return &records.Time{T: end}
}

// blobValue is a BLOB as raw shows it: the bytes in base64 when short, else only their length.
func blobValue(b []byte) map[string]any {
	if len(b) <= 256 {
		return map[string]any{"blob_b64": base64(b)}
	}
	return map[string]any{"blob_len": int64(len(b))}
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64 is standard padded base64 (encoding/base64 is not importable by a parser; a blob marker is its only use).
func base64(b []byte) string {
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		k := min(3, len(b)-i)
		for j := 0; j < 3; j++ {
			n <<= 8
			if j < k {
				n |= uint32(b[i+j])
			}
		}
		out = append(out, b64Alphabet[n>>18&63], b64Alphabet[n>>12&63])
		if k > 1 {
			out = append(out, b64Alphabet[n>>6&63])
		} else {
			out = append(out, '=')
		}
		if k > 2 {
			out = append(out, b64Alphabet[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
