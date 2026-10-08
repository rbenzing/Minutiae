package dbrecover

// SetCheckForTest replaces the checker run inside validateDBRow's panic guard
// and returns the function that restores it. It exists only to let a test
// trigger the recover path; nothing in a non-test file reaches it.
func SetCheckForTest(f func(map[string]any) error) func() {
	old := check
	check = f
	return func() { check = old }
}

// SetVersionForTest replaces the parser version and returns the restore function.
func SetVersionForTest(v string) func() {
	old := version
	version = v
	return func() { version = old }
}
