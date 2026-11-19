package archtest

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // the oracle engine (tests only)
)

// The shared helpers of the calendar DDL snapshot tests (plan 4I3, F22): the Android snapshots
// (calendar_android_ddl_test.go, Task 2) and the iOS one (Task 7) read their CREATE TABLE
// statements, their CROSSCHECK.md rows and their URL ban through these, and load in the engine.

var calendarCreateRE = regexp.MustCompile(`(?m)^CREATE TABLE (\w+) `)

// calendarSchemaDir is tools/fixtures/schemas/<family>.
func calendarSchemaDir(t *testing.T, family string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "tools", "fixtures", "schemas", family)
}

// calendarCreateTables lists the tables a snapshot creates, in file order.
func calendarCreateTables(src string) []string {
	var out []string
	for _, m := range calendarCreateRE.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

// calendarCrosscheckVerdict returns the third cell of the CROSSCHECK.md row "| <file> | <table> | ... |",
// anchored at the start of its line so that neither the Evidence column nor a longer row can satisfy it,
// and whether there is such a row.
func calendarCrosscheckVerdict(report, file, table string) (string, bool) {
	row := regexp.MustCompile(`(?m)^\| ` + regexp.QuoteMeta(file) + ` \| ` + regexp.QuoteMeta(table) + ` \| ([^|]+?) \|`)
	m := row.FindStringSubmatch(report)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// calendarURLLines returns the lines of src that hold "http": a calendar snapshot or report cites its
// sources by repository, tag, commit and SHA-256, never by a URL.
func calendarURLLines(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "http") {
			out = append(out, line)
		}
	}
	return out
}

// calendarLoadInEngine executes the whole snapshot in a fresh engine database and returns the number
// of tables it created (the engine's own sqlite_sequence, which AUTOINCREMENT creates, not counted).
func calendarLoadInEngine(t *testing.T, src string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(src); err != nil {
		t.Fatalf("the snapshot does not load: %v", err)
	}
	var n int
	if err := db.QueryRow("select count(*) from sqlite_master where type = 'table' and name not like 'sqlite\\_%' escape '\\'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// calendarReadFile reads a file of a schema directory.
func calendarReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
