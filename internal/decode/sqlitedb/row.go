package sqlitedb

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// RowFlags are facts about a whole row.
type RowFlags uint16

// The row flags.
const (
	FlagLengthMismatch RowFlags = 1 << iota // the record is longer than its header and body account for
	FlagKeyRange                            // the rowid lies outside the key range its position promises
	FlagOverflowMixed                       // overflow pages lie in a different file or frame than the cell
	FlagExtraValues                         // the record holds values beyond the declared columns
	FlagShortRecord                         // the record ended before the last stored column
	FlagUnknownValues                       // some declared column is not Known
)

// Loc says where a row's cell lies on disk.
type Loc = sqlitefile.Loc

// RecoveredInfo is what a recovered row adds to a live one. It is declared
// here and filled by FromRecovered.
type RecoveredInfo struct {
	Method    string
	Origin    sqlitefile.Origin
	Basis     sqlitefile.TableBasis
	Relation  sqlitefile.Relation
	Truncated bool
	WAL       *sqlitefile.WALProv
	Journal   *sqlitefile.JournalProv
	Notes     []string
}

// Row is one row of a table. It implements parse.Row. The bytes its accessors
// return alias the reader's memory: they are valid only until the Scan callback
// that received the row returns, so copy what must outlive it. The zero Row has
// no table and no columns.
type Row struct {
	tbl       *Table
	rowid     int64
	hasRowid  bool
	vals      []sqlitefile.Value
	states    []ColState
	text      [][]byte // decoded UTF-8 of the UTF-16 text columns; nil elsewhere
	storedLen int
	flags     RowFlags
	loc       Loc
	rec       *RecoveredInfo
}

var _ parse.Row = Row{}

// rowInput is what a Row is built from.
type rowInput struct {
	rowid     int64
	hasRowid  bool
	vals      []sqlitefile.Value // resolved, in declared column order
	storedLen int
	truncated bool
	flags     RowFlags // the damage flags the reader raised
	loc       Loc
	rec       *RecoveredInfo
}

// rowValueCost is a constant upper bound of sizeof(sqlitefile.Value) plus the
// state byte, charged for every column of a row while it is delivered.
const rowValueCost = 96

// buildRow derives the states, the UTF-16 decoding and the flags of a row.
// charge is asked for the memory before it is allocated and its error ends
// the build.
func (t *Table) buildRow(in rowInput, charge func(n int64) error) (Row, error) {
	def := &t.def
	r := Row{
		tbl: t, rowid: in.rowid, hasRowid: in.hasRowid, vals: in.vals, storedLen: in.storedLen,
		flags: in.flags, loc: in.loc, rec: in.rec,
	}
	if err := charge(int64(len(in.vals)) * rowValueCost); err != nil {
		return Row{}, err
	}
	r.states = make([]ColState, len(in.vals))
	var chargeErr error
	// decode decodes the UTF-16 text of col once and reports whether it is
	// undecodable. The charge comes first.
	decode := func(col int) bool {
		if r.text == nil {
			r.text = make([][]byte, len(in.vals))
		}
		if r.text[col] != nil {
			return false
		}
		v := &in.vals[col]
		if chargeErr = charge(int64(len(v.Bytes))*3/2 + 4); chargeErr != nil {
			return false
		}
		s, ok := v.Text()
		if !ok {
			return true
		}
		r.text[col] = []byte(s)
		return false
	}
	si := stateInput{
		def: def, vals: in.vals, storedLen: in.storedLen, hasRowid: in.hasRowid, truncated: in.truncated,
		undecodable: decode,
	}
	unknown := false
	for col := range in.vals {
		st := deriveState(si, col)
		if st == StateDefaulted && in.vals[col].Kind == sqlitefile.KindText && isUTF16(in.vals[col].Enc) && decode(col) {
			st = StateUndecodable
		}
		if chargeErr != nil {
			return Row{}, chargeErr
		}
		r.states[col] = st
		unknown = unknown || !st.Known()
	}
	if in.loc.OverflowMixed {
		r.flags |= FlagOverflowMixed
	}
	switch {
	case in.storedLen < def.StoredColumns:
		r.flags |= FlagShortRecord
	case in.storedLen > def.StoredColumns:
		r.flags |= FlagExtraValues
	}
	if unknown {
		r.flags |= FlagUnknownValues
	}
	return r, nil
}

// Table is the name of the row's table, "" for the zero Row.
func (r Row) Table() string {
	if r.tbl == nil {
		return ""
	}
	return r.tbl.name
}

// Rowid returns the rowid; ok is false for a WITHOUT ROWID row and for a
// recovered row whose rowid was lost.
func (r Row) Rowid() (int64, bool) { return r.rowid, r.hasRowid }

// State says what column col holds. A column outside the table is Absent.
func (r Row) State(col int) ColState {
	if col < 0 || col >= len(r.states) {
		return StateAbsent
	}
	return r.states[col]
}

// Unknown reports whether column col is not a value the database holds
// (anything but Present, Null and Defaulted).
func (r Row) Unknown(col int) bool { return !r.State(col).Known() }

// Value returns the resolved value of column col as the reader holds it, the
// zero Value for a column outside the table. Check State before trusting it.
func (r Row) Value(col int) sqlitefile.Value {
	if col < 0 || col >= len(r.vals) {
		return sqlitefile.Value{}
	}
	return r.vals[col]
}

// Kind is the storage class of column col, KindNull for a column outside the
// table.
func (r Row) Kind(col int) sqlitefile.Kind { return r.Value(col).Kind }

// known returns the value of column col when it is stored or defaulted.
func (r Row) known(col int) (sqlitefile.Value, bool) {
	switch r.State(col) {
	case StatePresent, StateDefaulted:
		return r.vals[col], true
	}
	return sqlitefile.Value{}, false
}

// IsNull is true only for a stored or defaulted NULL.
func (r Row) IsNull(col int) bool {
	switch r.State(col) {
	case StateNull:
		return true
	case StateDefaulted:
		return r.vals[col].Kind == sqlitefile.KindNull
	}
	return false
}

// Int returns an integer value. No other kind is converted.
func (r Row) Int(col int) (int64, bool) {
	if v, ok := r.known(col); ok && v.Kind == sqlitefile.KindInt {
		return v.Int, true
	}
	return 0, false
}

// Float returns a real value (an integer stored in a REAL column was promoted
// when the row was read). No other kind is converted.
func (r Row) Float(col int) (float64, bool) {
	if v, ok := r.known(col); ok && v.Kind == sqlitefile.KindFloat {
		return v.Float, true
	}
	return 0, false
}

// Text returns a text value as UTF-8. In a UTF-8 database these are the stored
// bytes, valid UTF-8 or not; UTF-16 text is decoded strictly, and text that
// does not decode is not answered (see RawText).
func (r Row) Text(col int) ([]byte, bool) {
	v, ok := r.known(col)
	if !ok || v.Kind != sqlitefile.KindText {
		return nil, false
	}
	if isUTF16(v.Enc) {
		if r.text == nil || r.text[col] == nil {
			return nil, false
		}
		return r.text[col], true
	}
	return v.Bytes, true
}

// Blob returns a blob value.
func (r Row) Blob(col int) ([]byte, bool) {
	if v, ok := r.known(col); ok && v.Kind == sqlitefile.KindBlob {
		return v.Bytes, true
	}
	return nil, false
}

// RawText returns the bytes of a text value as the database stores them, in
// the database encoding. It also answers for text that is clipped or
// undecodable, whose state says so, and not for an omitted or unread value.
func (r Row) RawText(col int) ([]byte, bool) {
	switch r.State(col) {
	case StatePresent, StateDefaulted, StateClipped, StateUndecodable:
		if v := r.vals[col]; v.Kind == sqlitefile.KindText && !v.Omitted && !v.Unread {
			return v.Bytes, true
		}
	}
	return nil, false
}

// NumCols is the number of declared columns of the table.
func (r Row) NumCols() int { return len(r.vals) }

// Flags are the facts about the row as a whole.
func (r Row) Flags() RowFlags { return r.flags }

// ExtraValues is how many values the record holds beyond the declared columns
// (they are flagged, not exposed).
func (r Row) ExtraValues() int {
	if r.tbl == nil {
		return 0
	}
	return max(0, r.storedLen-r.tbl.def.StoredColumns)
}

// Loc says where the row's cell lies.
func (r Row) Loc() Loc { return r.loc }

// Range is the cell's byte range and the file it lies in.
func (r Row) Range() (records.Range, parse.FileRole) {
	role := parse.RoleDB
	switch r.loc.File {
	case sqlitefile.FileWAL:
		role = parse.RoleWAL
	case sqlitefile.FileJournal:
		role = parse.RoleJournal
	}
	return records.Range{Offset: r.loc.Offset, Length: r.loc.Length}, role
}

// Recovered describes a recovered row; it is nil for a live row.
func (r Row) Recovered() *RecoveredInfo { return r.rec }

// StateOf returns the state of column col of r when r is a Row of this
// package; ok is false for any other parse.Row.
func StateOf(r parse.Row, col int) (ColState, bool) {
	if row, ok := r.(Row); ok {
		return row.State(col), true
	}
	return StateAbsent, false
}
