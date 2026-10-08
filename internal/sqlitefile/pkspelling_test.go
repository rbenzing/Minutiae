package sqlitefile_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestParserAcceptsKeySpellingsTheEngineAccepts: PRIMARY KEY('a') (a string
// literal naming a column) and a key list that names a column twice are
// accepted by the engine; the parser accepts them too and reads the rows as the
// engine does (final review A, F7). The engine decides each case.
func TestParserAcceptsKeySpellingsTheEngineAccepts(t *testing.T) {
	for _, c := range []struct {
		name string
		ddl  string
		rows []string
	}{
		{"string literal key", `create table k(a integer, b, primary key('a'))`, []string{"insert into k values (5, 'x')", "insert into k values (3, 'y')"}},
		{"string literal key without rowid", `create table k(a text, b, primary key('a')) without rowid`, []string{"insert into k values ('q', 1)", "insert into k values ('p', 2)"}},
		{"duplicate key column", `create table k(a text, b, primary key(a, a))`, []string{"insert into k values ('x', 1)", "insert into k values ('y', 2)"}},
		{"duplicate key column without rowid", `create table k(a text, b, primary key(a, a)) without rowid`, []string{"insert into k values ('x', 1)", "insert into k values ('y', 2)"}},
		{"duplicate key column in other case", `create table k(a text, b, primary key(a, A)) without rowid`, []string{"insert into k values ('x', 1)", "insert into k values ('y', 2)"}},
		{"duplicate key column, one collation", `create table k(a text, b, primary key(a collate nocase, a collate nocase)) without rowid`, []string{"insert into k values ('x', 1)", "insert into k values ('y', 2)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "k.db")
			e := openEngine(t, path)
			if _, err := e.Exec(c.ddl); err != nil {
				t.Skipf("the engine refuses %q: %v", c.ddl, err) // nothing to match
			}
			for _, r := range c.rows {
				mustExec(t, e, r)
			}
			want := engineDumpRows(t, e)
			xi := engineXinfo(t, e, "k")
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, v := openLive(t, data, sqlitefile.Options{})
			tb, err := v.Table(context.Background(), "k")
			if err != nil {
				t.Fatal(err)
			}
			def := tb.Def()
			if !def.ParseOK {
				t.Fatalf("the definition is not parsed (%s), the engine accepts it", def.ParseNote)
			}
			for i, x := range xi {
				if def.Columns[i].PKOrdinal != x.pk {
					t.Errorf("column %s: pk ordinal %d, engine %d", x.name, def.Columns[i].PKOrdinal, x.pk)
				}
			}
			if diff, _ := diffDumps(liveDumpRows(t, v), want); diff != "" {
				t.Errorf("library differs from the engine: %s", diff)
			}
		})
	}
}

// TestDuplicateKeyColumnWithTwoCollationsStaysUnparsed: the engine keys the
// index by (a, a) under each collation, so 'x' and 'x ' are two rows under
// (rtrim, nocase). The parser does not derive an identity from it: the table is
// listed unparsed, with a warning, never read with the wrong key.
func TestDuplicateKeyColumnWithTwoCollationsStaysUnparsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	e := openEngine(t, path)
	mustExec(t, e, `create table k(a text, b, primary key(a collate rtrim, a collate nocase)) without rowid`)
	mustExec(t, e, `insert into k values ('x', 1)`)
	mustExec(t, e, `insert into k values ('x ', 2)`) // two rows for the engine
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, v := openLive(t, data, sqlitefile.Options{})
	tb, err := v.Table(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if def := tb.Def(); def.ParseOK {
		t.Errorf("parsed with a single key column %+v", def.Columns)
	}
	warned := false
	for _, w := range v.Warnings() {
		warned = warned || w.Code == sqlitefile.WarnSchemaSQLUnparsed
	}
	if !warned {
		t.Errorf("no schema-sql-unparsed warning: %v", v.Warnings())
	}
}
