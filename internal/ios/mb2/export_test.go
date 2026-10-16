package mb2

// SetFoldCase forces the case-folding behaviour of the file system for a test
// and returns the function that restores it.
func SetFoldCase(v bool) (restore func()) {
	old := foldCase
	foldCase = v
	return func() { foldCase = old }
}
