package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sqliteFilePackages are the packages that must stay free of any SQL engine:
// the reader reads evidence bytes itself and never links a driver.
var sqliteFilePackages = []string{"internal/sqlitefile", "internal/sqlitefile/sqlitetest"}

// engineImport reports whether an import path is a SQL engine or the
// database/sql layer (modernc.org/* drivers, database/sql and its driver
// sub-package).
func engineImport(path string) bool {
	return strings.HasPrefix(path, "modernc.org/") || path == "database/sql" || strings.HasPrefix(path, "database/sql/")
}

// engineImportsInSource parses src (imports only) and returns the banned
// import paths it holds.
func engineImportsInSource(name, src string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range f.Imports {
		if p := strings.Trim(imp.Path.Value, "`\""); engineImport(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// engineImportsInDir returns "file: import" for every banned import in the
// non-test .go files directly inside dir, and the number of files examined.
// _test.go files are exempt (tests may open the engine as an oracle).
func engineImportsInDir(dir string) (offenders []string, files int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, 0, err
		}
		files++
		bad, err := engineImportsInSource(n, string(src))
		if err != nil {
			return nil, 0, err
		}
		for _, b := range bad {
			offenders = append(offenders, n+": "+b)
		}
	}
	return offenders, files, nil
}

// TestSQLiteFileUsesNoEngine: non-test files of internal/sqlitefile and
// internal/sqlitefile/sqlitetest import neither modernc.org/* nor
// database/sql. It is separate from, and does not replace, the wider purity
// rule of the parser packages.
func TestSQLiteFileUsesNoEngine(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range sqliteFilePackages {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		offenders, files, err := engineImportsInDir(dir)
		if err != nil {
			if os.IsNotExist(err) && pkg != sqliteFilePackages[0] {
				continue // the test-only builder arrives in a later task
			}
			t.Fatal(err)
		}
		if files == 0 {
			t.Errorf("%s: no non-test files were examined; the check is vacuous", pkg)
		}
		for _, o := range offenders {
			t.Errorf("%s imports a SQL engine from a non-test file: %s", pkg, o)
		}
	}
}

// TestSQLiteFileEngineRuleSelfCheck: the rule is not vacuous. Each snippet is
// flagged or passed as stated, and _test.go files are skipped on disk.
func TestSQLiteFileEngineRuleSelfCheck(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"modernc driver", "package p\nimport _ \"modernc.org/sqlite\"\n", []string{"modernc.org/sqlite"}},
		{"modernc lib", "package p\nimport \"modernc.org/libc\"\n", []string{"modernc.org/libc"}},
		{"database/sql", "package p\nimport \"database/sql\"\n", []string{"database/sql"}},
		{"database/sql/driver", "package p\nimport \"database/sql/driver\"\n", []string{"database/sql/driver"}},
		{"grouped", "package p\nimport (\n\t\"fmt\"\n\t_ \"modernc.org/sqlite\"\n\t\"database/sql\"\n)\n", []string{"modernc.org/sqlite", "database/sql"}},
		{"clean", "package p\nimport (\n\t\"errors\"\n\t\"fmt\"\n\t\"sync\"\n)\n", nil},
		{"lookalike prefix", "package p\nimport \"example.com/modernc.org/x\"\nimport \"database/sqlx\"\n", nil},
		{"no imports", "package p\n", nil},
	}
	for _, c := range cases {
		got, err := engineImportsInSource("x.go", c.src)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: banned imports = %q, want %q", c.name, got, c.want)
		}
	}

	dir := t.TempDir()
	bad := "package x\nimport _ \"modernc.org/sqlite\"\n"
	for name, src := range map[string]string{"prod.go": bad, "x_test.go": bad, "ok.go": "package x\n", "notes.txt": "modernc.org/sqlite"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	offenders, files, err := engineImportsInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || len(offenders) != 1 || offenders[0] != "prod.go: modernc.org/sqlite" {
		t.Fatalf("dir scan: files=%d offenders=%q; want 2 files and only prod.go flagged", files, offenders)
	}
}
