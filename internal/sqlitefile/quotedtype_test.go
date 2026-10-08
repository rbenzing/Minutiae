package sqlitefile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestQuotedDeclaredTypeIsDequoted: the engine dequotes the tokens of a
// declared type, so `a "INTEGER" PRIMARY KEY` (and the bracket, backtick and
// single-quote spellings) is a rowid alias. The library must read the rowid,
// as the engine does, with no warning (final review A, F3). A quoted STRICT
// type is accepted by the engine and by the library.
func TestQuotedDeclaredTypeIsDequoted(t *testing.T) {
	for _, c := range []struct{ name, sql string }{
		{"double quotes", `create table q(a "INTEGER" primary key, b)`},
		{"brackets", `create table q(a [INTEGER] primary key, b)`},
		{"backticks", "create table q(a `INTEGER` primary key, b)"},
		{"single quotes", `create table q(a 'INTEGER' primary key, b)`},
		{"lower case quoted", `create table q(a "integer" primary key, b)`},
		{"strict quoted", `create table q(a "INTEGER" primary key, b "TEXT") strict`},
		{"quoted affinity", `create table q(a integer primary key, b "TEXT")`},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "q.db")
			db := openEngine(t, path)
			mustExec(t, db, c.sql)
			for _, v := range []string{"x", "y", "z"} {
				mustExec(t, db, "insert into q(b) values (?)", v)
			}
			want := engineDumpRows(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, v := openLive(t, data, sqlitefile.Options{})
			got := liveDumpRows(t, v)
			if diff, _ := diffDumps(got, want); diff != "" {
				t.Errorf("library differs from the engine: %s", diff)
			}
			for _, w := range v.Warnings() {
				t.Errorf("warning: %s %s", w.Code, w.Msg)
			}
		})
	}
}
