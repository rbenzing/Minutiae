package sqlitedb

// WrapErr exposes the library-error mapping to the external tests.
func WrapErr(err error) error { return wrapErr(err) }
