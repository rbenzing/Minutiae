package sqlitedb

import (
	"context"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// WrapErr exposes the library-error mapping to the external tests.
func WrapErr(err error) error { return wrapErr(err) }

// RowValueCost exposes the per-value charge of Scan to the external tests.
const RowValueCost = rowValueCost

// MarkRecovered returns r as a recovered row (a stand-in until FromRecovered).
func MarkRecovered(r Row) Row {
	r.rec = &RecoveredInfo{Method: "test"}
	return r
}

// MarkRecoveredWithNotes is MarkRecovered with notes on the recovered info.
func MarkRecoveredWithNotes(r Row, notes ...string) Row {
	r.rec = &RecoveredInfo{Method: "test", Notes: notes}
	return r
}

// Scribble overwrites every slice r holds that Clone must deep-copy (states,
// UTF-16 text, overflow list, notes) and reports what it reached.
func Scribble(r Row) (states, text, overflow, notes int) {
	for i := range r.states {
		r.states[i] = StateAbsent
		states++
	}
	for _, t := range r.text {
		for i := range t {
			t[i] = '#'
			text++
		}
	}
	for i := range r.loc.Overflow {
		r.loc.Overflow[i] = sqlitefile.PagePart{}
		overflow++
	}
	if r.rec != nil {
		for i := range r.rec.Notes {
			r.rec.Notes[i] = "scribbled"
			notes++
		}
	}
	return
}

// OpenAfter is Open that calls after once the library calls have returned.
func OpenAfter(ctx context.Context, f Files, b Budget, after func()) (*DB, error) {
	return open(ctx, f, b, after)
}

// GetAfter is Get that calls after once the library has returned.
func (t *Table) GetAfter(ctx context.Context, rowid int64, after func()) (Row, bool, error) {
	return t.get(ctx, rowid, after)
}

// CloneCost exposes the size Clone charges for r.
func (r Row) CloneCost() int64 { return r.cloneCost() }

// WithRecovered returns r as a recovered row carrying the given provenance.
func WithRecovered(r Row, w *sqlitefile.WALProv, j *sqlitefile.JournalProv) Row {
	r.rec = &RecoveredInfo{Method: "test", WAL: w, Journal: j}
	return r
}

// Prov returns the WAL and journal provenance pointers of a recovered row.
func Prov(r Row) (*sqlitefile.WALProv, *sqlitefile.JournalProv) {
	if r.rec == nil {
		return nil, nil
	}
	return r.rec.WAL, r.rec.Journal
}

// SetBuildIndex replaces the index build of a Context (a test seam).
func (c *Context) SetBuildIndex(f func(context.Context, *Table, string) (*Index, error)) { c.build = f }

// SetIndexLimit makes the Context build its indexes with the given entry cap.
func (c *Context) SetIndexLimit(n int) {
	c.build = func(ctx context.Context, t *Table, col string) (*Index, error) { return t.Index(ctx, col, n) }
}
