package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// recordTables are the tables only internal/evidence and internal/records may
// write: the unified record tables, records_meta, and artifacts (which only
// internal/evidence writes today).
const recordTables = `(?:records_meta|records|record_batches|parsers|record_times|record_runs|record_run_artifacts|record_superseded|artifacts)`

// unknown stands for an operand of a string expression that is not a constant
// (a variable, a call): it may hold a table name, so it counts as one.
const unknown = "\x01"

// placeholder is a printf verb or an unknown operand.
const placeholder = `(?:%(?:\[\d+\])?[-+# 0-9.]*[svq]|\x01)`

// tableRef is a record table name, or a placeholder in table position, with an
// optional main. prefix. Identifier quoting is removed by normalizeSQL first.
const tableRef = `(?:main\s*\.\s*)?(?:\b` + recordTables + `\b|\x01)`

// SQL-shaped patterns over the normalized text (see normalizeSQL), so plain
// English ("failed to update records") is not flagged: each write verb needs its
// structural keyword in SQL order (INSERT .. INTO table, UPDATE table .. SET,
// DELETE FROM table, ALTER TABLE table, DROP TABLE|TRIGGER|INDEX).
var recordWriteRE = regexp.MustCompile(`(?is)\b(?:` +
	`(?:insert|replace)\s+(?:or\s+\w+\s+)?into\s+` + tableRef + `|` +
	`update\s+(?:or\s+\w+\s+)?` + tableRef + `(?:\s+(?:as\s+)?\w+)?(?:\s+indexed\s+by\s+\w+|\s+not\s+indexed)?\s+set\b|` +
	`delete\s+from\s+` + tableRef + `|` +
	`alter\s+table\s+` + tableRef + `|` +
	`drop\s+(?:table|trigger|index)\b)`)

// recordWriteTailRE matches a string that ends in a write verb: the first half
// of SQL a builder finishes with a table name held in a variable.
var recordWriteTailRE = regexp.MustCompile(`(?is)\b(?:insert\s+(?:or\s+\w+\s+)?into|replace\s+into|delete\s+from|alter\s+table)\s*$`)

var (
	placeholderRE   = regexp.MustCompile(placeholder)
	blockCommentRE  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRE   = regexp.MustCompile(`--[^\n]*`)
	quotedIdentREs  = []*regexp.Regexp{regexp.MustCompile(`"([^"]*)"`), regexp.MustCompile("`([^`]*)`"), regexp.MustCompile(`\[([^\]]*)\]`)}
	whitespaceRunRE = regexp.MustCompile(`\s+`)
)

// normalizeSQL prepares a constant SQL string for matching: printf verbs and
// unknown operands become one placeholder, /* */ and -- comments are removed,
// "x", `x` and [x] identifiers are unquoted and whitespace runs collapse to one
// space, so quoting, comments and line breaks cannot hide a statement.
func normalizeSQL(s string) string {
	s = placeholderRE.ReplaceAllString(s, unknown)
	s = blockCommentRE.ReplaceAllString(s, " ")
	s = lineCommentRE.ReplaceAllString(s, " ")
	for _, re := range quotedIdentREs {
		s = re.ReplaceAllString(s, "$1")
	}
	return strings.TrimSpace(whitespaceRunRE.ReplaceAllString(s, " "))
}

// writesRecordTable reports whether a folded string holds SQL that writes a record table.
func writesRecordTable(text string) bool {
	n := normalizeSQL(text)
	return recordWriteRE.MatchString(n) || recordWriteTailRE.MatchString(n)
}

type writeViolation struct {
	File string
	Line int
	Text string
}

// evalString folds a string expression: literals, named string constants (from
// consts), parentheses and `+`. Any other operand becomes `unknown`.
func evalString(e ast.Expr, consts map[string]string) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return s
			}
			return x.Value
		}
	case *ast.ParenExpr:
		return evalString(x.X, consts)
	case *ast.Ident:
		if v, ok := consts[x.Name]; ok {
			return v
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return evalString(x.X, consts) + evalString(x.Y, consts)
		}
	}
	return unknown
}

// collectConsts gathers the string constants declared (at any level) in the
// parsed files of one package; constants built from other constants are folded
// by repeating until nothing new resolves.
func collectConsts(files []*ast.File) map[string]string {
	type decl struct {
		name string
		expr ast.Expr
	}
	var decls []decl
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
			}
			for _, sp := range gd.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						decls = append(decls, decl{id.Name, vs.Values[i]})
					}
				}
			}
			return true
		})
	}
	consts := map[string]string{}
	for range 8 {
		changed := false
		for _, d := range decls {
			v := evalString(d.expr, consts)
			if strings.Contains(v, unknown) {
				continue
			}
			if old, ok := consts[d.name]; !ok || old != v {
				consts[d.name] = v
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return consts
}

// markOperands records the operands of an already evaluated `+` expression so
// they are not evaluated again on their own.
func markOperands(e ast.Expr, covered map[ast.Node]bool) {
	switch y := e.(type) {
	case *ast.BinaryExpr:
		if y.Op == token.ADD {
			covered[y] = true
			markOperands(y.X, covered)
			markOperands(y.Y, covered)
		}
	case *ast.ParenExpr:
		covered[y] = true
		markOperands(y.X, covered)
	case *ast.BasicLit:
		covered[y] = true
	}
}

// recordTableWrites scans the files of one package (sources by file name) and
// returns every string expression holding SQL that writes a record table.
// Constant string expressions (concatenations of literals and named constants)
// are folded first; a printf verb or a non-constant operand in table position
// counts as a table name; a string ending in a write verb is a builder piece.
// Comments are never inspected.
func recordTableWrites(t testing.TB, srcs map[string]string) []writeViolation {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	names := make([]string, 0, len(srcs))
	for name := range srcs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	consts := collectConsts(files)

	var out []writeViolation
	for _, f := range files {
		covered := map[ast.Node]bool{}
		check := func(n ast.Expr) {
			text := evalString(n, consts)
			if writesRecordTable(text) {
				p := fset.Position(n.Pos())
				out = append(out, writeViolation{File: p.Filename, Line: p.Line, Text: strings.TrimSpace(strings.ReplaceAll(text, unknown, "<?>"))})
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil || covered[n] {
				return true
			}
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.ADD {
					return true
				}
				check(x)
				markOperands(x.X, covered)
				markOperands(x.Y, covered)
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					check(x)
				}
			}
			return true
		})
	}
	return out
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
	byDir := map[string]map[string]string{}
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
		dir := filepath.Dir(p)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][p] = string(src)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Go files: the walk is not reaching the tree", scanned)
	}
	for _, srcs := range byDir {
		for _, v := range recordTableWrites(t, srcs) {
			rel, _ := filepath.Rel(root, v.File)
			if oracleDropExempt(rel, v.Text) {
				continue
			}
			t.Errorf("%s:%d writes a record table with SQL (only internal/evidence and internal/records may): %q", filepath.ToSlash(rel), v.Line, v.Text)
		}
	}
}

// TestRecordTableWriteScannerSelfTest keeps the rule from going vacuous. The
// violating and harmless snippets live in testdata/writes.go.txt (not compiled,
// not scanned by the rule), so this package holds no violation itself: every
// line marked "// want" must be flagged and no other line may be. The data
// holds every evasion of the review (concatenation, named constants, a
// variable in table position, Sprintf verbs, a Builder piece) and plain English
// that must not be flagged.
func TestRecordTableWriteScannerSelfTest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "writes.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	want := map[int]bool{}
	for i, line := range lines {
		if strings.Contains(line, "// want") {
			want[i+1] = true
		}
	}
	if len(want) < 25 {
		t.Fatalf("only %d lines are marked as violations: the test data was damaged", len(want))
	}
	got := map[int]bool{}
	for _, v := range recordTableWrites(t, map[string]string{"writes.go": string(data)}) {
		got[v.Line] = true
		if !want[v.Line] {
			t.Errorf("line %d was flagged but is harmless: %q", v.Line, v.Text)
		}
	}
	for l := range want {
		if !got[l] {
			t.Errorf("line %d was not flagged: %s", l, strings.TrimSpace(lines[l-1]))
		}
	}
	// every table of the rule is covered, with each write verb
	for _, table := range strings.Split(strings.Trim(recordTables, "(?:)"), "|") {
		for _, tmpl := range []string{"DELETE FROM @", "INSERT INTO @ (x) VALUES (1)", "UPDATE @ SET x = 1"} {
			sql := strings.ReplaceAll(tmpl, "@", table)
			src := "package x\n\nvar q = " + strconv.Quote(sql) + "\n"
			if len(recordTableWrites(t, map[string]string{"x.go": src})) != 1 {
				t.Errorf("%q is not flagged", sql)
			}
		}
	}
}

// TestRecordTableWriteScannerSeesConstantsAcrossFiles: a constant declared in
// another file of the package still folds into the statement.
func TestRecordTableWriteScannerSeesConstantsAcrossFiles(t *testing.T) {
	srcs := map[string]string{
		"a.go": "package x\n\nconst table = \"record_runs\"\n",
		"b.go": "package x\n\nvar q = \"DELETE FROM \" + table\n",
	}
	got := recordTableWrites(t, srcs)
	if len(got) != 1 || got[0].File != "b.go" {
		t.Errorf("violations = %+v, want one in b.go", got)
	}
}
