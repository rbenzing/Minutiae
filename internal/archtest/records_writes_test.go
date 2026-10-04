package archtest

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// recordTables are the tables only internal/evidence and internal/records may
// write: the unified record tables, records_meta, and artifacts (which only
// internal/evidence writes today).
const recordTables = `(?:records_meta|records|record_batches|parsers|record_times|record_runs|record_run_artifacts|record_superseded|artifacts)`

// recordWriteRE matches SQL that inserts into, updates, deletes from, drops or
// alters one of recordTables (an optional main. prefix and quoting allowed).
var recordWriteRE = regexp.MustCompile(`(?is)\b(?:insert\s+(?:or\s+\w+\s+)?into|replace\s+into|update(?:\s+or\s+\w+)?|delete\s+from|drop\s+table(?:\s+if\s+exists)?|alter\s+table)\s+(?:main\s*\.\s*)?["` + "`" + `\[]?` + recordTables + `\b`)

// recordTableWrites returns the string literals of Go source src that hold SQL
// writing to a record table. Only string literals are inspected, so a comment
// that mentions such SQL is not a violation.
func recordTableWrites(src string) []string {
	var found []string
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, []byte(src), func(token.Position, string) {}, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return found
		}
		if tok != token.STRING {
			continue
		}
		text, err := strconv.Unquote(lit)
		if err != nil {
			text = lit
		}
		if recordWriteRE.MatchString(text) {
			found = append(found, strings.TrimSpace(text))
		}
	}
}

// TestOnlyRecordsPackageWritesRecordTables is the single-writer rule: no Go
// source (tests included) outside internal/evidence and internal/records writes
// to the record tables with SQL. Tamper helpers live in recordstest as named
// functions.
func TestOnlyRecordsPackageWritesRecordTables(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	exempt := []string{
		filepath.Join(root, "internal", "evidence") + string(filepath.Separator),
		filepath.Join(root, "internal", "records") + string(filepath.Separator),
	}
	scanned := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); strings.HasPrefix(name, ".") || name == "docs" || name == "bin" || name == "cases" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		for _, e := range exempt {
			if strings.HasPrefix(p, e) {
				return nil
			}
		}
		src, err := os.ReadFile(p) //nolint:gosec // reading the repository's own sources; the walk never follows links
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, p)
		for _, v := range recordTableWrites(string(src)) {
			t.Errorf("%s writes a record table with SQL (only internal/evidence and internal/records may): %q", filepath.ToSlash(rel), v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Go files: the walk is not reaching the tree", scanned)
	}
}

// TestRecordTableWriteScannerSelfTest keeps the rule from going vacuous: the
// scanner must flag a table of violating snippets and pass a table of harmless
// ones. The snippets are built by concatenation so this file does not contain
// the SQL it looks for.
func TestRecordTableWriteScannerSelfTest(t *testing.T) {
	violating := []string{
		"INSERT INTO " + "records (id) VALUES (1)",
		"insert or replace into " + "parsers (name) VALUES ('x')",
		"INSERT OR IGNORE INTO " + "record_times (record_id) VALUES (1)",
		"INSERT INTO " + "record_batches (batch_id) VALUES (1)",
		"UPDATE " + "records_meta SET value = '9'",
		"update  " + "record_batches set count = 1",
		"UPDATE OR ROLLBACK " + "records SET type = 'x'",
		"DELETE FROM " + "record_runs",
		"delete\nfrom " + "record_run_artifacts",
		"DROP TABLE " + "record_superseded",
		"drop table if exists " + "records",
		"ALTER TABLE " + "records ADD COLUMN x",
		"REPLACE INTO " + "records (id) VALUES (1)",
		"INSERT INTO \"" + "records\" (id) VALUES (1)",
		"INSERT INTO main." + "records (id) VALUES (1)",
		"UPDATE " + "artifacts SET path = 'x'",
		"INSERT INTO " + "artifacts (id) VALUES ('x')",
	}
	for _, sql := range violating {
		for name, lit := range map[string]string{"interpreted": strconv.Quote(sql), "raw": "`" + sql + "`"} {
			src := "package x\n\nvar q = " + lit + "\n"
			if got := recordTableWrites(src); len(got) != 1 {
				t.Errorf("%s literal %q: scanner found %d violations, want 1", name, sql, len(got))
			}
		}
	}
	harmless := []string{
		"SELECT * FROM " + "records",
		"SELECT count(*) FROM " + "record_runs",
		"UPDATE " + "other_table SET x = 1",
		"INSERT INTO " + "my_records VALUES (1)",
		"INSERT INTO " + "parsers_backup VALUES (1)",
		"DELETE FROM " + "records_archive",
		"UPDATE " + "records_meta_x SET a = 1",
		"SELECT 'insert into' || name FROM " + "records",
		"PRAGMA table_info(" + "records)",
	}
	for _, sql := range harmless {
		src := "package x\n\nvar q = " + strconv.Quote(sql) + "\n"
		if got := recordTableWrites(src); len(got) != 0 {
			t.Errorf("harmless %q was flagged: %v", sql, got)
		}
	}
	// a comment is not a write
	if got := recordTableWrites("package x\n\n// UPDATE " + "records SET x = 1 would be a violation\n/* DELETE FROM " + "records */\nvar q = 1\n"); len(got) != 0 {
		t.Errorf("comments were flagged: %v", got)
	}
	// every table name of the rule is covered
	for _, table := range strings.Split(strings.Trim(recordTables, "(?:)"), "|") {
		src := "package x\n\nvar q = " + strconv.Quote("DELETE FROM "+table) + "\n"
		if len(recordTableWrites(src)) != 1 {
			t.Errorf("table %s is not covered by the rule", table)
		}
	}
}
