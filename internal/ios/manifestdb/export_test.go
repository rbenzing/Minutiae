package manifestdb

// SetMaxRows lowers the row cap for a test; the returned func restores it.
func SetMaxRows(n int) (restore func()) {
	orig := maxRows
	maxRows = n
	return func() { maxRows = orig }
}
