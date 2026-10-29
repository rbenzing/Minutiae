package dbrecover

import (
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

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

// SetSiblingScanForTest replaces the sibling scan run inside FindSiblings'
// panic guard and returns the restore function.
func SetSiblingScanForTest(f func([]evidence.ManifestRecord, evidence.ManifestRecord) SiblingReport) func() {
	old := siblingScan
	siblingScan = f
	return func() { siblingScan = old }
}

// SetAfterFlushHookForTest sets the function Run calls after its last Flush and
// warnings, before the fresh hash of the inputs, and returns the restore function.
func SetAfterFlushHookForTest(f func()) func() {
	old := afterFlushHook
	afterFlushHook = f
	return func() { afterFlushHook = old }
}

// SetRowHookForTest sets a function that may change each recovered row before
// it is mapped, and returns the restore function.
func SetRowHookForTest(f func(sqlitefile.RecoveredRow) sqlitefile.RecoveredRow) func() {
	old := rowHook
	rowHook = f
	return func() { rowHook = old }
}

// SetProgressEveryForTest sets how many rows lie between two Progress calls.
func SetProgressEveryForTest(n int64) func() {
	old := progressEvery
	progressEvery = n
	return func() { progressEvery = old }
}

// SetResolveHookForTest sets the function resolve calls once the database input
// is known, and returns the restore function.
func SetResolveHookForTest(f func()) func() {
	old := resolveHook
	resolveHook = f
	return func() { resolveHook = old }
}

// SetSchemaReadHookForTest sets a function the engine-refusal gate calls before
// it reads the schema; an error it returns is taken as the schema read error.
func SetSchemaReadHookForTest(f func() error) func() {
	old := schemaReadHook
	schemaReadHook = f
	return func() { schemaReadHook = old }
}

// SetOpenHookForTest sets a function called each time an input artifact is opened for a run, and returns the restore function.
func SetOpenHookForTest(f func(role, id string)) func() {
	old := openHook
	openHook = f
	return func() { openHook = old }
}
