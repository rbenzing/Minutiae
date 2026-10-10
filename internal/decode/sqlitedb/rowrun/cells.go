package rowrun

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
)

// CellKind says what a column of a row holds, finely enough that a mapping never
// reads an unknown as an absence.
type CellKind uint8

// The kinds.
const (
	CellMissing CellKind = iota // this variant of the table lacks the column (col < 0)
	CellNull                    // stored or defaulted NULL
	CellValue                   // a value of the wanted storage class
	CellOther                   // a value of another storage class (text in an integer column)
	CellUnknown                 // not a value the database vouches for: omitted, unread, clipped, undecodable, lost, generated
)

// StateOf returns the decoder's state of column col; a row of another
// implementation answers from IsNull only. A negative column is StateAbsent.
func StateOf(row parse.Row, col int) sqlitedb.ColState {
	if col < 0 {
		return sqlitedb.StateAbsent
	}
	if st, ok := sqlitedb.StateOf(row, col); ok {
		return st
	}
	if row.IsNull(col) {
		return sqlitedb.StateNull
	}
	return sqlitedb.StatePresent
}

func kindOf(row parse.Row, col int) CellKind {
	if col < 0 {
		return CellMissing
	}
	switch st := StateOf(row, col); {
	case !st.Known():
		return CellUnknown
	case row.IsNull(col):
		return CellNull
	}
	return CellValue
}

// IntCell reads an integer column. Only CellValue carries a usable number.
func IntCell(row parse.Row, col int) (int64, CellKind) {
	k := kindOf(row, col)
	if k != CellValue {
		return 0, k
	}
	if v, ok := row.Int(col); ok {
		return v, CellValue
	}
	return 0, CellOther
}

// TextCell reads a text column. The bytes alias the reader's memory and are
// valid until the Scan callback or Context that owns the row is done.
func TextCell(row parse.Row, col int) ([]byte, CellKind) {
	k := kindOf(row, col)
	if k != CellValue {
		return nil, k
	}
	if v, ok := row.Text(col); ok {
		return v, CellValue
	}
	return nil, CellOther
}

// Flag records in unknown (payload.raw.unknown_columns) a column that exists but
// holds no usable value: "type-mismatch" for CellOther, the state name for
// CellUnknown. Other kinds record nothing.
func Flag(unknown map[string]any, row parse.Row, name string, col int, k CellKind) {
	switch k {
	case CellOther:
		unknown[name] = "type-mismatch"
	case CellUnknown:
		unknown[name] = StateOf(row, col).String()
	}
}

type flaggedRow interface{ Flags() sqlitedb.RowFlags }

// DamageFlags names the row-level damage the decoder saw ("length_mismatch",
// "key_range", "extra_values"), for payload.raw.row_flags. A short record
// (written before an ADD COLUMN) is normal and not damage. nil when none.
func DamageFlags(row parse.Row) []any {
	f, ok := row.(flaggedRow)
	if !ok {
		return nil
	}
	fl := f.Flags()
	var out []any
	if fl&sqlitedb.FlagLengthMismatch != 0 {
		out = append(out, "length_mismatch")
	}
	if fl&sqlitedb.FlagKeyRange != 0 {
		out = append(out, "key_range")
	}
	if fl&sqlitedb.FlagExtraValues != 0 {
		out = append(out, "extra_values")
	}
	return out
}

// Cols maps the column names a mapping reads to their index in this variant of
// its table, -1 when the variant lacks the column.
type Cols map[string]int

// Resolve looks every name up once with m.Column. A -1 means only that the
// variant lacks the column (rowrun.Run fails the job when the Context failed for
// any other reason).
func Resolve(m parse.MapContext, table string, names ...string) Cols {
	c := make(Cols, len(names))
	for _, n := range names {
		c[n] = m.Column(table, n)
	}
	return c
}

// At returns the index of a resolved column, -1 for a name that was not resolved.
func (c Cols) At(name string) int {
	if i, ok := c[name]; ok {
		return i
	}
	return -1
}
