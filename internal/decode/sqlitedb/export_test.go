package sqlitedb

// WrapErr exposes the library-error mapping to the external tests.
func WrapErr(err error) error { return wrapErr(err) }

// RowValueCost exposes the per-value charge of Scan to the external tests.
const RowValueCost = rowValueCost
