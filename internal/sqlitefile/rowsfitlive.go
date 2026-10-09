package sqlitefile

// equalsLiveFit reports whether a row named by the column fit alone (BasisFit
// or BasisGuess) is, rowid and values, a live row of the table the fit names
// (C45). It is a note for the reader and nothing else: the relation stays
// unknown, the confidence is unchanged and the row is delivered, because the
// identity is a guess and an equal row of a guessed table is not a duplicate.
// It reuses the comparison of the schema-named rows, so the same storage-kind
// and exact-bytes rule applies and an omitted, clipped or unread value is
// inconclusive, never equal. Everything but a clean "same" is false; only a
// cancellation, the budget or an I/O error is returned.
func (rp *rowPass) equalsLiveFit(id ident, row *RecoveredRow) (bool, error) {
	if id.kind == kindNone || id.table == "" {
		return false, nil
	}
	res, err := rp.compare(id, row)
	if err != nil {
		return false, err
	}
	return res.kind == cmpSame, nil
}
