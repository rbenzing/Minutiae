package sqlitedb

// WrapErr exposes the library-error mapping to the external tests.
func WrapErr(err error) error { return wrapErr(err) }

// RowValueCost exposes the per-value charge of Scan to the external tests.
const RowValueCost = rowValueCost

// MarkRecovered returns r as a recovered row (a stand-in until FromRecovered).
func MarkRecovered(r Row) Row {
	r.rec = &RecoveredInfo{Method: "test"}
	return r
}
