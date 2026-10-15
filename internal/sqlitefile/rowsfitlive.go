package sqlitefile

import (
	"fmt"
	"strings"
)

// equalsLiveFit reports whether a row named by the column fit alone (BasisFit
// or BasisGuess) is, rowid and values, a live row of the table the fit names
// (C45). It is a note for the reader and nothing else: the relation stays
// unknown, the confidence is unchanged and the row is delivered, because the
// identity is a guess and an equal row of a guessed table is not a duplicate.
// It reuses the comparison of the schema-named rows, so the same storage-kind
// and exact-bytes rule applies and an omitted, clipped or unread value is
// inconclusive, never equal. Everything but a clean "same" is false; only a
// cancellation or an I/O error is returned.
func (rp *rowPass) equalsLiveFit(id ident, row *RecoveredRow) (bool, error) {
	res, err := rp.fitEquality(id, row)
	return res.kind == cmpSame, err
}

// fitEquality is equalsLiveFit with its answer: a result of cmpUnknown with the
// note NoteFitEqualityBudget means the equality was cut (its budget, MaxDiffRows
// or the memory budget), counted in RowStats.FitEqualityCapped (J110).
func (rp *rowPass) fitEquality(id ident, row *RecoveredRow) (cmpResult, error) {
	if id.kind == kindNone || id.table == "" {
		return cmpResult{}, nil
	}
	res, err := rp.compareIn(id, row, true)
	if err != nil {
		return cmpResult{}, err
	}
	if res.kind == cmpUnknown && res.note == NoteFitEqualityBudget {
		rp.st.FitEqualityCapped++
	}
	return res, nil
}

// warnFitEqualityCapped adds the one warning that counts the rows the
// fit-only equality left unannotated and names why.
func (rp *rowPass) warnFitEqualityCapped() {
	if rp.st.FitEqualityCapped == 0 {
		return
	}
	rp.h.warns.add(Warning{Code: WarnLimitReached, Msg: fmt.Sprintf(
		"the fit-only equality was cut (%s): %d fit-named rows are annotated unknown (%s) and were not compared with the live rows",
		strings.Join(rp.fitCauses, "; "), rp.st.FitEqualityCapped, NoteFitEqualityBudget)})
}
