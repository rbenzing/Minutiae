package sqlitefile_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// parseNonTestFiles parses the non-test .go files of the package under test
// (the test runs in the package directory).
func parseNonTestFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// readQuotedLines reads a file of Go-quoted strings, one per line.
func readQuotedLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\r\n"), "\n") {
		s, err := strconv.Unquote(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("%s: %q: %v", path, line, err)
		}
		out = append(out, s)
	}
	return out
}

// sqlShape matches a string that reads like a statement: a statement verb
// followed, anywhere later, by from, into, table, index or set; plus the
// pragma form. Single tokens ("table", "index", "set") are fine. This keeps
// every literal of the package clear of a later scan that rejects embedded
// statements.
var sqlShape = regexp.MustCompile(`(?is)\b(select|insert|update|delete|create|drop|alter|replace)\b.*?\b(from|into|table|index|set)\b|\bpragma\s+\w`)

// sqlShapedLiterals returns the string literals of src (a Go source file) that
// look like statements.
func sqlShapedLiterals(t *testing.T, src string) (bad []string, literals int) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("snippet does not parse: %v\n%s", err, src)
	}
	return scanLiterals(t, f)
}

func scanLiterals(t *testing.T, f *ast.File) (bad []string, literals int) {
	t.Helper()
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("unquote %s: %v", lit.Value, err)
		}
		literals++
		if sqlShape.MatchString(s) {
			bad = append(bad, s)
		}
		return true
	})
	return bad, literals
}

func TestNoSQLShapedLiterals(t *testing.T) {
	// Self-check: the scanner flags statement shapes and passes ordinary
	// text, so it cannot go vacuous.
	// The snippets live in testdata (Go-quoted, one per line) so this source
	// holds no statement-shaped text itself.
	positives := readQuotedLines(t, "testdata/sqlshape_positive.txt")
	negatives := readQuotedLines(t, "testdata/sqlshape_negative.txt")
	if len(positives) < 10 || len(negatives) < 10 {
		t.Fatalf("snippet tables are too small (%d positive, %d negative)", len(positives), len(negatives))
	}
	for _, s := range positives {
		src := "package p\nvar _ = " + strconv.Quote(s) + "\n"
		if bad, n := sqlShapedLiterals(t, src); len(bad) != 1 || n != 1 {
			t.Errorf("literal %q must be flagged, got %v (%d literals)", s, bad, n)
		}
	}
	for _, s := range negatives {
		src := "package p\nvar _ = " + strconv.Quote(s) + "\n"
		if bad, n := sqlShapedLiterals(t, src); len(bad) != 0 || n != 1 {
			t.Errorf("literal %q must pass, got %v (%d literals)", s, bad, n)
		}
	}
	// Raw strings and literals nested in calls are seen too.
	raw := "package p\nfunc f(string) {}\nfunc g() { f(`" + positives[0] + "`) }\n"
	if bad, _ := sqlShapedLiterals(t, raw); len(bad) != 1 {
		t.Error("a raw string nested in a call must be scanned")
	}

	files := parseNonTestFiles(t)
	if len(files) < 5 {
		t.Fatalf("scanned only %d non-test files; the scan is vacuous", len(files))
	}
	total := 0
	for _, f := range files {
		bad, n := scanLiterals(t, f)
		total += n
		for _, s := range bad {
			t.Errorf("%s: string literal looks like a statement: %q", f.Name.Name, s)
		}
	}
	if total < 10 {
		t.Fatalf("scanned only %d string literals; the scan is vacuous", total)
	}
}
