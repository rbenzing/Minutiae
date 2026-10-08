package sqlitedb

import "github.com/rbenzing/minutiae/internal/sqlitefile"

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
