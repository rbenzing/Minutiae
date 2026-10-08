package sqlitedb

import (
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// FromRecovered maps a recovered row of table t through the same Row type as a
// live row, so a parser reads both with the same accessors. It reads no file:
// the table supplies only the declared columns, which are applied
// positionally to rr.Values.
//
// Nothing lost is presented as a value: a rowid that was not recovered leaves
// a rowid-alias column StateLost (and Rowid false), a Truncated record leaves
// the columns it no longer holds StateUnread, an unread, omitted or clipped
// value keeps that state, and a record flagged record-length-mismatch does not
// default the columns it lacks. Only a complete, short record takes the
// literal defaults of its columns. The label (method, origin, table basis,
// relation, provenance, notes) is carried unchanged in Recovered().
//
// rr.Index non-empty, rr.Table empty or a table that is not t (ASCII
// case-insensitive) is ErrRowMismatch. The row is owned: its memory is charged
// to the budget of t's DB before it is allocated and given back by Release or
// DB.Release; a refusal is the budget's error and the zero Row. Locator is
// ("", false).
func FromRecovered(t *Table, rr sqlitefile.RecoveredRow) (out Row, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = Row{}, fmt.Errorf("%w: %v", ErrInternal, r)
		}
	}()
	if t == nil {
		return Row{}, fmt.Errorf("%w: no table", ErrRowMismatch)
	}
	db := t.db
	if db.released {
		return Row{}, ErrReleased
	}
	switch {
	case rr.Index != "":
		return Row{}, fmt.Errorf("%w: an index entry of %q is not a row of table %q", ErrRowMismatch, rr.Index, t.name)
	case rr.Table == "":
		return Row{}, fmt.Errorf("%w: the recovered row names no table", ErrRowMismatch)
	case asciiFold(rr.Table) != asciiFold(t.name):
		return Row{}, fmt.Errorf("%w: the recovered row belongs to table %q, not %q", ErrRowMismatch, rr.Table, t.name)
	}
	var raw sqlitefile.Row
	if rr.Rowid != nil { // nil means lost: never dereferenced unchecked
		raw.Rowid, raw.HasRowid = *rr.Rowid, true
	}
	raw.Values = rr.Values
	in := rowInput{
		rowid: raw.Rowid, hasRowid: raw.HasRowid, vals: t.lt.Resolve(raw), storedLen: len(rr.Values),
		truncated: rr.Truncated, loc: rr.Loc,
		rec: &RecoveredInfo{
			Method: rr.Method, Origin: rr.Origin, Basis: rr.TableBasis, Relation: rr.Relation, Truncated: rr.Truncated,
			WAL: rr.WAL, Journal: rr.Journal, Notes: rr.Notes,
		},
	}
	if slices.Contains(rr.Notes, sqlitefile.NoteRecordLengthMismatch) {
		in.flags |= FlagLengthMismatch
	}
	var held int64
	charge := func(n int64) error {
		if err := db.budget.Alloc(n); err != nil {
			return err
		}
		held += n
		return nil
	}
	defer func() { db.budget.Free(held) }()
	built, err := t.buildRow(in, charge)
	if err != nil {
		return Row{}, err
	}
	return built.Clone()
}
