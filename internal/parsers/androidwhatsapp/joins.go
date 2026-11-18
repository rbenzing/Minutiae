package androidwhatsapp

import (
	"errors"
	"math"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

const (
	maxText = 256 << 10 // bytes of text kept; the rest is flagged
	maxList = 1000      // entries of a list in one record
	maxAddr = 1024      // the message contract's limit for a participant address and a thread id
)

// joinState is the answer of a join: never "no rows" for what is really unknown.
type joinState int

const (
	jFound     joinState = iota // the join was made (a Match may return no rows)
	jNoRow                      // a clean miss: no row has the key
	jNoTable                    // the table (or its key column) is absent from this variant
	jUndecided                  // the join cannot be decided (an undecidable key, a lossy index, an unreadable table)
)

// joinFailure turns the errors that say "this join is unknown" into a state; every other error is fatal.
func joinFailure(err error) (joinState, error) {
	var us *sqlitedb.UnsupportedSchemaError
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return jNoTable, nil
	case errors.As(err, &us), errors.Is(err, sqlitedb.ErrKeyUndecidable):
		return jUndecided, nil
	}
	return jFound, err
}

// getRow returns the row of table with the rowid id (the join column is the rowid alias: Probe checks it).
func getRow(m parse.MapContext, table string, id int64) (parse.Row, joinState, error) {
	r, found, err := m.Get(table, id)
	if err != nil {
		st, ferr := joinFailure(err)
		return nil, st, ferr
	}
	if !found {
		return nil, jNoRow, nil
	}
	return r, jFound, nil
}

// matchInt returns the rows of table whose column col equals id.
func matchInt(m parse.MapContext, table, col string, id int64) ([]parse.Row, joinState, error) {
	rows, undecided, err := rowrun.Match(m, table, col, id)
	if err != nil {
		st, ferr := joinFailure(err)
		return nil, st, ferr
	}
	if undecided {
		return nil, jUndecided, nil
	}
	return rows, jFound, nil
}

// undecidedHits is what *sqlitedb.Context offers beside parse.MapContext.
type undecidedHits interface {
	UndecidedHits(table, col string) int64
}

// matchText returns the rows of table whose TEXT column col equals key, the engine's equality under the column's
// collation. A hit on an index that lost rows may be a short answer: that is undecided, never "all the rows".
func matchText(m parse.MapContext, table, col string, key []byte) ([]parse.Row, joinState, error) {
	j, _ := m.(undecidedHits)
	hits := func() int64 {
		if j == nil {
			return 0
		}
		return j.UndecidedHits(table, col)
	}
	before := hits()
	rows, err := m.Match(table, col, sqlitedb.LiteralTextKey(key))
	if err != nil {
		st, ferr := joinFailure(err)
		return nil, st, ferr
	}
	if hits() > before {
		return nil, jUndecided, nil
	}
	return rows, jFound, nil
}

// cellReader reads the cells of one row into raw and unknown (payload.raw and payload.raw.unknown_columns).
type cellReader struct {
	row     parse.Row
	cols    rowrun.Cols
	raw     map[string]any
	unknown map[string]any
	prefix  string // "" for the mapped table, "<table>." for a joined row: the name an unknown column is listed under
}

// int reads an INTEGER cell; a cell of another class or an unreadable one is stated in unknown, never read as 0.
func (r cellReader) int(name string) (int64, bool) {
	v, k := rowrun.IntCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// text reads a TEXT cell.
func (r cellReader) text(name string) ([]byte, bool) {
	v, k := rowrun.TextCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, r.prefix+name, r.cols.At(name), k)
	return v, k == rowrun.CellValue
}

// rawInts copies the INTEGER cells of the named columns into raw as stored (the columns the parser keeps without
// interpreting them).
func (r cellReader) rawInts(names ...string) {
	for _, n := range names {
		if v, ok := r.int(n); ok {
			r.raw[n] = v
		}
	}
}

// rawText copies a non-empty TEXT cell into raw as a cleaned string; a cleaned text is listed in raw.text_flags.
func (r cellReader) rawText(name string) {
	if b, ok := r.text(name); ok && len(b) > 0 {
		r.raw[name] = cleanField(r.raw, name, b)
	}
}

// stored returns the stored value of a cell of any storage class for raw: an integer, a cleaned text, a finite float
// or a blob marker; nil for NULL, an absent column and an unreadable cell.
func (r cellReader) stored(name string) any {
	col := r.cols.At(name)
	if col < 0 || r.row.IsNull(col) {
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

// blobValue is a BLOB as raw shows it: the bytes in base64 when short, else only their length.
func blobValue(b []byte) map[string]any {
	if len(b) <= 256 {
		return map[string]any{"blob_b64": base64(b)}
	}
	return map[string]any{"blob_len": int64(len(b))}
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

// undecidedJoin lists a join in raw.join_undecided.
func undecidedJoin(raw map[string]any, join string) {
	cur, _ := raw["join_undecided"].([]any)
	for _, j := range cur {
		if j == join {
			return
		}
	}
	raw["join_undecided"] = append(cur, join)
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
