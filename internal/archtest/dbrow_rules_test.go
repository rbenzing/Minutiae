package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dbRowFree are the trees whose non-test files must not name the db_row record type: it belongs to
// internal/dbrecover (recovered raw rows), never to a parser or a record-type package.
var dbRowFree = []string{"internal/parsers", "internal/recordtypes"}

// foldString returns the constant string an expression of string literals and
// "+" denotes, and whether it is one.
func foldString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return foldString(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok1 := foldString(x.X)
		r, ok2 := foldString(x.Y)
		return l + r, ok1 && ok2
	}
	return "", false
}

// mentionsDBRow reports whether src holds a string constant naming db_row: an
// interpreted or raw literal, or literals joined with "+".
func mentionsDBRow(src string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		return strings.Contains(src, "db_row") // unparsable: be strict
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok {
			if s, ok := foldString(e); ok && strings.Contains(s, "db_row") {
				found = true
			}
		}
		return !found
	})
	return found
}

// dbRowOffenders lists the non-test files under dirs (relative to root) that name db_row.
func dbRowOffenders(t *testing.T, root string, dirs []string) (offenders []string, scanned int) {
	t.Helper()
	for _, d := range dirs {
		files := nonTestGoFiles(t, root, filepath.FromSlash(d))
		scanned += len(files)
		for _, p := range files {
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if mentionsDBRow(string(src)) {
				rel, _ := filepath.Rel(root, p)
				offenders = append(offenders, filepath.ToSlash(rel))
			}
		}
	}
	return offenders, scanned
}

func TestDBRowTypeStaysOutOfParsers(t *testing.T) {
	offenders, scanned := dbRowOffenders(t, repoRoot(t), dbRowFree)
	for _, o := range offenders {
		t.Errorf("%s names the \"db_row\" record type; only internal/dbrecover may", o)
	}
	if scanned < 10 {
		t.Fatalf("scanned only %d files: the walk is broken", scanned)
	}
}

func TestDBRowScannerSelfTest(t *testing.T) {
	for src, want := range map[string]bool{
		"package p\nconst T = \"db_row\"\n":                true,
		"package p\nconst T = `db_row`\n":                  true,
		"package p\nconst T = \"db\" + \"_row\"\n":         true,
		"package p\nvar x = []string{\"a\", \"db_row\"}\n": true,
		"package p\nconst T = \"db_rows\"\n":               true,
		"package p\nconst T = \"message\"\n":               false,
		"package p\n// db_row in a comment\n":              false,
		"package p\n":                                      false,
	} {
		if got := mentionsDBRow(src); got != want {
			t.Errorf("mentionsDBRow(%q) = %v, want %v", src, got, want)
		}
	}
	// the walk itself: plant a file in a scratch tree and see it flagged
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "parsers", "x")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"bad.go":      "package x\nconst T = `db_row`\n",
		"good.go":     "package x\nconst T = \"ok\"\n",
		"bad_test.go": "package x\nconst T = \"db_row\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, scanned := dbRowOffenders(t, root, []string{"internal/parsers"})
	if scanned != 2 || len(got) != 1 || got[0] != "internal/parsers/x/bad.go" {
		t.Errorf("planted tree: offenders %v, scanned %d", got, scanned)
	}
}
