package sqlitefile_test

// The committed fixtures of plan 3I, Task 14 (names are the contract). Class
// A is byte-reproducible (builder-written files, and engine-written
// database-only files), class B is written by the engine together with a WAL
// or journal (salts and nonces come from its PRNG: image and oracle are
// regenerated together), class C is written by the container family
// (tools/fixtures/sqlite.sh).

// fixtureSpec names one fixture and the companion files it has.
type fixtureSpec struct {
	Name      string
	Class     string // "A", "B" or "C"
	WAL       bool
	Journal   bool
	NotSQLite bool // the database file is not SQLite at all
}

var fixtureSpecs = []fixtureSpec{
	{Name: "basic-utf8-4k", Class: "A"},
	{Name: "utf16le-1k", Class: "A"},
	{Name: "utf16be-512", Class: "A"},
	{Name: "overflow-wide", Class: "A"},
	{Name: "without-rowid", Class: "A"},
	{Name: "addcolumn-short", Class: "A"},
	{Name: "autovacuum-incr", Class: "A"},
	{Name: "freelist-dropped", Class: "A"},
	{Name: "secure-delete", Class: "A"},
	{Name: "builder-wal-generations", Class: "A", WAL: true},
	{Name: "builder-hot-journal", Class: "A", Journal: true},
	{Name: "builder-journal-multiseg", Class: "A", Journal: true},
	{Name: "wal-uncheckpointed", Class: "B", WAL: true},
	{Name: "wal-stale-generation", Class: "B", WAL: true},
	{Name: "hot-journal", Class: "B", Journal: true},
	{Name: "persist-journal", Class: "B", Journal: true},
	{Name: "encrypted-like", Class: "A", NotSQLite: true},
	{Name: "sqlite3-wal-killed", Class: "C", WAL: true},
	{Name: "sqlite3-hot-journal-killed", Class: "C", Journal: true},
	{Name: "sqlite3-freelist", Class: "C"},
	{Name: "sqlite3-utf16", Class: "C"},
}

// classBNames is the table of the plan: the fixtures whose files the engine
// writes together with a WAL or journal. The determinism test excludes
// exactly these by name (and the container family, which Go does not write).
var classBNames = []string{"wal-uncheckpointed", "wal-stale-generation", "hot-journal", "persist-journal"}
