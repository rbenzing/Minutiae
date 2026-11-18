package ioswhatsapp

import (
	"crypto/sha256"
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const (
	maxText = 256 << 10 // bytes of text kept; the rest is flagged
	maxAddr = 1024      // the message contract's limit for a participant address and a thread id
)

// The tables the mapping reads, and the prefix under which one of their unreadable columns is listed in
// raw.unknown_columns (4I-R22: "<table>.<column>").
const (
	prefixMessage = "ZWAMESSAGE."
	prefixSession = "ZWACHATSESSION."
	prefixMember  = "ZWAGROUPMEMBER."
	prefixMedia   = "ZWAMEDIAITEM."
	prefixInfo    = "ZWAMESSAGEINFO."
)

// cellReader reads the cells of one row into raw and unknown (payload.raw and payload.raw.unknown_columns); a cell
// that holds no usable value is stated in unknown, never read as 0 or as empty.
type cellReader struct {
	row     parse.Row
	cols    rowrun.Cols
	raw     map[string]any
	unknown map[string]any
	prefix  string
}

// int reads an INTEGER cell.
func (r cellReader) int(name string) (int64, bool) {
	v, k := rowrun.IntCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// text reads a TEXT cell; the bytes may be empty (a stored empty string is a value).
func (r cellReader) text(name string) ([]byte, bool) {
	v, k := rowrun.TextCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// number reads an INTEGER or REAL cell.
func (r cellReader) number(name string) (cellx.Number, bool) {
	v, k := cellx.NumberCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// blob reads a BLOB cell.
func (r cellReader) blob(name string) ([]byte, bool) {
	v, k := blobCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// blobCell reads a BLOB column as rowrun.TextCell reads a TEXT one: CellOther for another storage class.
func blobCell(row parse.Row, col int) ([]byte, rowrun.CellKind) {
	switch {
	case col < 0:
		return nil, rowrun.CellMissing
	case !rowrun.StateOf(row, col).Known():
		return nil, rowrun.CellUnknown
	case row.IsNull(col):
		return nil, rowrun.CellNull
	}
	if v, ok := row.Blob(col); ok {
		return v, rowrun.CellValue
	}
	return nil, rowrun.CellOther
}

// stored returns the stored value of a cell of any storage class for raw: an integer, a cleaned text, a finite float, the
// string "non-finite" or a blob marker; nil for NULL, an absent column and an unreadable cell.
func (r cellReader) stored(name string) any {
	col := r.cols.At(name)
	if col < 0 || r.row.IsNull(col) || !rowrun.StateOf(r.row, col).Known() {
		return nil
	}
	if v, ok := r.row.Int(col); ok {
		return v
	}
	if b, ok := r.row.Text(col); ok {
		return cleanField(r.raw, name, b)
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

// code is a stored enum cell: its value when it is a usable integer, and the stored value as it must appear in raw (an
// unusable cell keeps its stored value there and never gets a label).
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

// readCode reads an enum column into raw as stored. A cell that is not an INTEGER is stated in unknown_columns.
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

// setBody puts a cleaned body into the message and states what cleaning did (spec C4, C6, amendment 4I2-A2): the same
// fields as Android: the flags, on a cut `body_truncated` and `body_total_bytes`, and the original bytes (base64, or their
// length and SHA-256) of a text that held NUL or invalid UTF-8. An altered body is also listed in raw.text_flags.
func setBody(msg *message.Message, raw map[string]any, c common.Cleaned) {
	if len(c.Flags) == 0 {
		return
	}
	markText(raw, "body")
	msg.BodyFlags = c.Flags
	if c.Truncated {
		t, total := true, int64(c.TotalBytes)
		msg.BodyTruncated, msg.BodyTotalBytes = &t, &total
	}
	msg.BodyRawB64, msg.BodyRawSHA256 = c.RawB64, c.RawSHA256
	if c.RawB64 != "" || c.RawSHA256 != "" {
		n := int64(c.RawLen)
		msg.BodyRawLen = &n
	}
}

// nanCell says a cell is a stored REAL NaN: the reader (as the engine) reads it as NULL but keeps the serial type 7, so
// it is detectable here and is never read as absent (unknown is never absent).
func nanCell(row parse.Row, col int) bool {
	r, ok := row.(sqlitedb.Row)
	return ok && col >= 0 && r.State(col) == sqlitedb.StateNull && r.Value(col).Serial == 7
}

// nanStatus states a date column that holds a NaN as raw.<name>_status = "invalid" (no time, no stored value to show).
func (r cellReader) nanStatus(name string) {
	if nanCell(r.row, r.cols.At(name)) {
		r.raw[name+"_status"] = "invalid"
	}
}

// digest is a blob as raw states it: its length and SHA-256, never its content (spec 8: not decoded).
func digest(b []byte) map[string]any {
	sum := sha256.Sum256(b)
	return map[string]any{"bytes": int64(len(b)), "sha256": fmt.Sprintf("%x", sum)}
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
