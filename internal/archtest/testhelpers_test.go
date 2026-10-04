package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testOnlyPackages may be imported only from _test.go files: they hold raw-SQL
// tamper helpers and fixtures that must never be linked into the binary (a
// non-test import from internal/cli, which may import anything, would ship them).
var testOnlyPackages = []string{"internal/records/recordstest", "internal/sqlitefile/sqlitetest"}

// nonTestImporters returns the non-_test.go files under root that import the
// package module+pkg.
func nonTestImporters(root, pkg string) ([]string, error) {
	var out []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); strings.HasPrefix(name, ".") || name == "docs" || name == "bin" || name == "cases" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == module+pkg {
				rel, _ := filepath.Rel(root, p)
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	return out, err
}

func TestTestOnlyPackagesAreImportedFromTestsOnly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range testOnlyPackages {
		files, err := nonTestImporters(root, pkg)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			t.Errorf("%s imports %s from a non-test file: it is test-only and would be linked into the binary", f, pkg)
		}
	}
}

// TestTestOnlyRuleFlagsNonTestImport: the rule is not vacuous: for every
// test-only package, a non-test file importing it (from any package, such as
// internal/cli) is found, a _test.go file importing it is not.
func TestTestOnlyRuleFlagsNonTestImport(t *testing.T) {
	for _, must := range []string{"internal/records/recordstest", "internal/sqlitefile/sqlitetest"} {
		listed := false
		for _, pkg := range testOnlyPackages {
			listed = listed || pkg == must
		}
		if !listed {
			t.Errorf("%s must be listed in testOnlyPackages", must)
		}
	}
	for _, pkg := range testOnlyPackages {
		dir := t.TempDir()
		imp := `package x

import _ "` + module + pkg + `"
`
		if err := os.WriteFile(filepath.Join(dir, "prod.go"), []byte(imp), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(imp), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := nonTestImporters(dir, pkg)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "prod.go" {
			t.Fatalf("%s: importers = %q, want only prod.go", pkg, got)
		}
	}
}
