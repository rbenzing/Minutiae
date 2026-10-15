package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestChangedFilesMappedToNoPackageAreReported(t *testing.T) {
	pkgs := fakeGraph(t)
	hit, unmapped := changedPackages([]string{"docs/a.md", "b/fixtures/x.bin", "scripts/run.sh", "b/x.go"}, pkgs)
	if got := sortedKeys(hit); !slices.Equal(got, []string{"b"}) {
		t.Errorf("packages %v, want [b]", got)
	}
	if want := []string{"docs/a.md", "scripts/run.sh"}; !slices.Equal(unmapped, want) {
		t.Errorf("unmapped %v, want %v", unmapped, want)
	}
}

func TestNearestAncestorPackageOwnsNonGoFiles(t *testing.T) {
	pkgs := fakeGraph(t)
	for file, want := range map[string]string{
		"b/fixtures/deep/x.bin": "b",
		"b/schema/s.sql":        "b",
		"g/h/schema/s.sql":      "g/h", // g itself is not a package
	} {
		hit, unmapped := changedPackages([]string{file}, pkgs)
		if got := sortedKeys(hit); !slices.Equal(got, []string{want}) || len(unmapped) != 0 {
			t.Errorf("%s: packages %v unmapped %v, want [%s]", file, got, unmapped, want)
		}
	}
}

func TestUnmappedFileEscalatesAndEveryRunPrintsTheList(t *testing.T) {
	e, _ := fakeEnv(t, []string{"scripts/run.sh", "docs/a.md"}, nil)
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if !p.full || !strings.Contains(p.reason, "scripts/run.sh") {
		t.Fatalf("plan full=%v reason=%q, want escalation naming scripts/run.sh", p.full, p.reason)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-affected", "-base", "abc", "-list"}, &out, &errb, e); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "changed files mapped to no package:\n  docs/a.md\n  scripts/run.sh\n") {
		t.Errorf("list lacks the unmapped files:\n%s", out.String())
	}
	e2, _ := fakeEnv(t, []string{"b/x.go"}, nil)
	out.Reset()
	if code := run([]string{"-affected", "-base", "abc", "-list"}, &out, &errb, e2); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "changed files mapped to no package:") {
		t.Errorf("an all-mapped run omits the unmapped section:\n%s", out.String())
	}
}

func TestEmbedFieldsOfTestsAreRead(t *testing.T) {
	root := t.TempDir()
	data := listJSON(t, root, []map[string]any{
		{"rel": "m", "EmbedFiles": []string{"e.txt"}, "TestEmbedFiles": []string{"te.txt"}, "XTestEmbedFiles": []string{"xe.txt"}},
	})
	pkgs, err := parseGoList(data, testMod, root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"m/e.txt", "m/te.txt", "m/xe.txt"}; !slices.Equal(pkgs[0].Embeds, want) {
		t.Errorf("embeds %v, want %v", pkgs[0].Embeds, want)
	}
}

func TestBlockPrefixBoundary(t *testing.T) {
	b := block{name: "x", patterns: []string{"internal/image/..."}}
	for dir, want := range map[string]bool{
		"internal/image": true, "internal/image/ewf": true, "internal/imagefoo": false,
		"internal/imag": false, "internal": false,
	} {
		if got := b.contains(dir); got != want {
			t.Errorf("contains(%s) = %v, want %v", dir, got, want)
		}
	}
	exact := block{name: "y", patterns: []string{"cmd"}}
	if !exact.contains("cmd") || exact.contains("cmd/x") {
		t.Error("an exact pattern matches only itself")
	}
}

func TestEmptyMergeBaseIsAnError(t *testing.T) {
	e, _ := fakeEnv(t, nil, nil)
	e.git = func(_ ...string) ([]byte, error) { return []byte("  \n"), nil }
	if _, err := buildPlan(options{affected: true}, e); err == nil {
		t.Error("an empty merge-base output was accepted")
	}
}

func TestBaseIsGuardedAgainstOptionInjection(t *testing.T) {
	e, _ := fakeEnv(t, []string{"b/x.go"}, nil)
	_, err := buildPlan(options{affected: true, base: "--output=x"}, e)
	var ue usageError
	if !errors.As(err, &ue) {
		t.Errorf("a base starting with - gave %v, want a usage error", err)
	}
	var diffArgs []string
	inner := e.git
	e.git = func(args ...string) ([]byte, error) {
		if args[0] == "diff" {
			diffArgs = args
		}
		return inner(args...)
	}
	if _, err := buildPlan(options{affected: true, base: "abc"}, e); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(diffArgs, "abc")
	if i < 1 || diffArgs[i-1] != "--end-of-options" {
		t.Errorf("git diff args %v: --end-of-options does not directly precede the base", diffArgs)
	}
}

func TestDeletedFileIsReadFromTheBase(t *testing.T) {
	e, _ := fakeEnv(t, []string{"b/old.go"}, nil)
	e.exists = func(string) bool { return false }
	var shown []string
	inner := e.git
	e.git = func(args ...string) ([]byte, error) {
		if args[0] == "show" {
			shown = args
			return []byte("//go:build cgo\n\npackage b\n"), nil
		}
		return inner(args...)
	}
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if !p.full || !strings.Contains(p.reason, "cgo") {
		t.Errorf("deleted cgo file did not escalate: full=%v reason=%q", p.full, p.reason)
	}
	if want := []string{"show", "--end-of-options", "abc:b/old.go"}; !slices.Equal(shown, want) {
		t.Errorf("git show args %v, want %v", shown, want)
	}
}

func TestRaceDetectorRefusesAffectedAndBlock(t *testing.T) {
	for _, o := range []options{{affected: true, base: "abc"}, {blocks: []string{"filesys"}}} {
		e, _ := fakeEnv(t, nil, nil)
		e.race = true
		_, err := buildPlan(o, e)
		var ue usageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "race gate is always the full form") {
			t.Errorf("%+v with the race detector: err = %v", o, err)
		}
	}
	e, _ := fakeEnv(t, nil, nil)
	e.race = true
	if _, err := buildPlan(options{}, e); err != nil {
		t.Errorf("the full form must still work with the race detector: %v", err)
	}
}

func TestSlowTableProblemsDetectsDrift(t *testing.T) {
	guards := map[string]bool{"p TestA": true, "p TestB": true}
	if got := slowTableProblems(guards, map[string]int{"p TestA": 70, "p TestB": 61}); len(got) != 0 {
		t.Errorf("consistent table reported %v", got)
	}
	if got := slowTableProblems(guards, map[string]int{"p TestA": 70}); len(got) != 1 || !strings.Contains(got[0], "p TestB") {
		t.Errorf("unlisted guard: %v", got)
	}
	if got := slowTableProblems(map[string]bool{}, map[string]int{"p TestA": 70}); len(got) != 1 {
		t.Errorf("listed but unguarded: %v", got)
	}
	if got := slowTableProblems(map[string]bool{"p TestA": true}, map[string]int{"p TestA": 59}); len(got) != 1 {
		t.Errorf("below the threshold: %v", got)
	}
}

func TestShortGuardsFindSyntheticGuardedTest(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"pk/a_test.go": `package pk

import "testing"

func TestSlow(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (> 60 s): runs in the merge gate")
	}
}

func TestFast(t *testing.T) { t.Log("x") }

func TestWrongText(t *testing.T) {
	if testing.Short() {
		t.Skip("something else")
	}
}

func TestNotFirst(t *testing.T) {
	t.Log("x")
	if testing.Short() {
		t.Skip("slow (> 60 s): runs in the merge gate")
	}
}
`})
	got := shortGuards(t, root)
	if len(got) != 1 || !got["pk TestSlow"] {
		t.Errorf("guards %v, want only pk TestSlow", got)
	}
	if p := slowTableProblems(got, map[string]int{"pk TestSlow": 90}); len(p) != 0 {
		t.Errorf("synthetic source and table disagree: %v", p)
	}
	if p := slowTableProblems(got, map[string]int{}); len(p) != 1 {
		t.Errorf("synthetic guard missing from the table went unnoticed: %v", p)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- cross-package testdata ----

func TestCrossPackageTestdataReadersAreAffected(t *testing.T) {
	srcs := []testSrc{
		{"p/q/a_test.go", []byte(`package q

import "path/filepath"

var _ = filepath.Join("..", "r", "testdata", "f.bin")
`)},
		{"p/q/b_test.go", []byte("package q\n\nconst c = \"../../s/testdata/z\"\n")},
		{"p/w/c_test.go", []byte(`package w

import "path/filepath"

func f(name string) string { return filepath.Join("..", "kinds", name, "testdata") }
`)},
		{"p/x/d_test.go", []byte("package x\n\nfunc f(n string) string { return \"../\" + n + \"/testdata/img.gz\" }\n")},
		{"p/y/e_test.go", []byte("package y\n\n// fixtures live in internal/m/testdata and example.com/m/internal/n/testdata\nvar _ = 1\n")},
		{"p/z/own_test.go", []byte(`package z

import "path/filepath"

var _ = filepath.Join("testdata", "own.bin")
`)},
	}
	cases := []struct {
		changed string
		want    []string
	}{
		{"p/r", []string{"p/q", "p/x"}},
		{"s", []string{"p/q"}},
		{"p/kinds/a", []string{"p/w"}},
		{"internal/m", []string{"p/y"}},
		{"internal/n", []string{"p/y"}},
		{"p/none", []string{"p/x"}},
		{"unrelated", nil},
	}
	for _, c := range cases {
		got, err := testdataReaders(srcs, []string{c.changed})
		if err != nil {
			t.Fatal(err)
		}
		if g := sortedKeys(got); !slices.Equal(g, c.want) {
			t.Errorf("testdata of %s read by %v, want %v", c.changed, g, c.want)
		}
	}
}

func TestUnparsableTestSourceIsAnError(t *testing.T) {
	if _, err := testdataReaders([]testSrc{{"p/a_test.go", []byte("package (")}}, []string{"x"}); err == nil {
		t.Error("an unparsable test file was skipped silently")
	}
}

func TestChangedTestdataAddsItsReaders(t *testing.T) {
	e, _ := fakeEnv(t, []string{"a/testdata/f.bin"}, nil)
	e.testSources = func() ([]testSrc, error) {
		return []testSrc{{"e/x_test.go", []byte("package e\n\nconst c = \"../a/testdata/f.bin\"\n")}}, nil
	}
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	// a changed; b, c, d, f import it; e reads its testdata (and g/h imports e)
	if want := []string{"a", "b", "c", "d", "e", "f", "g/h"}; !slices.Equal(p.affected, want) {
		t.Errorf("affected %v, want %v", p.affected, want)
	}
}

func TestEveryRelativeTestdataReferenceInTheRepoIsFound(t *testing.T) {
	t.Chdir("../..")
	pkgs, err := loadPackages()
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := loadTestSources(pkgs)
	if err != nil {
		t.Fatal(err)
	}
	// An independent, textual search for ".."-relative testdata references.
	re := regexp.MustCompile(`\.\./[A-Za-z0-9_./-]*testdata|"\.\."[^)\n]*"testdata"`)
	n := 0
	for _, s := range srcs {
		if strings.HasPrefix(s.Path, "tools/check/") || !re.Match(s.Src) { // the check's own tests quote such paths
			continue
		}
		n++
		refs, err := scanFile(path.Dir(s.Path), s.Src)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) == 0 {
			t.Errorf("%s has a relative testdata reference the scan did not resolve", s.Path)
		}
	}
	if n < 10 {
		t.Fatalf("only %d files with relative testdata references; the search is broken", n)
	}
	for changed, readers := range map[string][]string{
		"internal/sqlitefile":      {"internal/dbrecover", "internal/decode/sqlitedb"},
		"internal/filesys/apfs":    {"internal/examine", "internal/filesys/detect"},
		"internal/filesys/ext4":    {"internal/examine", "internal/filesys/detect"},
		"internal/filesys/hfsplus": {"internal/examine", "internal/filesys/detect"},
		"internal/image/ewf":       {"internal/examine"},
	} {
		got, err := testdataReaders(srcs, []string{changed})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range readers {
			if !got[r] {
				t.Errorf("testdata of %s: reader %s not found (got %v)", changed, r, sortedKeys(got))
			}
		}
	}
}

// ---- doc-consistency packages ----

// docReaders returns the directories with a test that reads docs/ or CLAUDE.md at run time: a call
// of os.ReadFile, Open, OpenFile, ReadDir or Stat with such a path among its arguments.
func docReaders(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, p, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isOSFileCall(call) {
				return true
			}
			ast.Inspect(call, func(m ast.Node) bool {
				lit, ok := m.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && isDocPath(lit.Value) {
					rel, _ := filepath.Rel(root, filepath.Dir(p))
					found[filepath.ToSlash(rel)] = true
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func isOSFileCall(c *ast.CallExpr) bool {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "os" && slices.Contains([]string{"ReadFile", "Open", "OpenFile", "ReadDir", "Stat"}, sel.Sel.Name)
}

func isDocPath(quoted string) bool {
	s := strings.Trim(quoted, "\"`")
	return s == "docs" || s == "CLAUDE.md" || strings.HasPrefix(s, "docs/") || strings.HasSuffix(s, "/CLAUDE.md")
}

func TestDocConsistencyPackagesAreListed(t *testing.T) {
	root := "../.."
	found := docReaders(t, root)
	listed := map[string]bool{}
	for _, d := range docConsistencyPackages {
		listed[d.dir] = true
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d.dir))); err != nil {
			t.Errorf("listed doc-consistency package %s: %v", d.dir, err)
		}
		if d.why == "" {
			t.Errorf("%s has no reason", d.dir)
		}
		if d.readsDocs && !found[d.dir] {
			t.Errorf("%s is listed as reading docs at run time but no test reads docs/ or CLAUDE.md", d.dir)
		}
	}
	for dir := range found {
		if !listed[dir] {
			t.Errorf("%s has a test that reads docs/ or CLAUDE.md but is not in docConsistencyPackages", dir)
		}
	}
	if len(docConsistencyPackages) != 2 || !listed["internal/archtest"] || !listed["tools/check"] {
		t.Errorf("the global doc block is archtest and tools/check only, got %v", docConsistencyPackages)
	}
}

func TestDocReaderDetectionSeesRuntimeReadsNotComments(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"reads/a_test.go":   "package r\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n)\n\nvar _, _ = os.ReadFile(filepath.Join(\"..\", \"docs\", \"x.md\"))\n",
		"claude/a_test.go":  "package r\n\nimport \"os\"\n\nvar _, _ = os.Open(\"../CLAUDE.md\")\n",
		"comment/a_test.go": "package r\n\n// see docs/testing.md and CLAUDE.md\nvar _ = \"docs/testing.md\"\n",
	})
	got := docReaders(t, root)
	if len(got) != 2 || !got["reads"] || !got["claude"] {
		t.Errorf("readers %v, want reads and claude only", got)
	}
}

// slowTableProblems compares the guarded tests found in the source with the docs table.
func slowTableProblems(guards map[string]bool, table map[string]int) []string {
	var out []string
	for k := range guards {
		if _, ok := table[k]; !ok {
			out = append(out, k+" has the slow-test Short guard but is not in the docs/testing.md table")
		}
	}
	for k, secs := range table {
		if !guards[k] {
			out = append(out, "docs/testing.md lists "+k+" as slow but its first statement is not the Short guard")
		}
		if secs < 60 {
			out = append(out, fmt.Sprintf("%s is listed with %d s, below the 60 s threshold", k, secs))
		}
	}
	sort.Strings(out)
	return out
}

func TestUnresolvedTestdataMentionsAreReported(t *testing.T) {
	src := []byte(`package p

import "path/filepath"

func f(root, n string) {
	_ = filepath.Join(root, "x", "testdata", "f")
	_ = filepath.Join("testdata", "own")
	_ = "testdata/own.bin"
	_ = filepath.Join("..", "x", "testdata", "f")
	_ = "../" + n + "/testdata/img"
	_ = root + "/testdata/z"
	_ = "a message about testdata fixtures"
}
`)
	got, err := unresolvedTestdata("p/q", src)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, m := range got {
		lines = append(lines, m.Line)
	}
	if want := []int{6, 11}; !slices.Equal(lines, want) {
		t.Errorf("unresolved mentions at lines %v, want %v", lines, want)
	}
}

func TestEveryTestdataReferenceIsResolved(t *testing.T) {
	t.Chdir("../..")
	pkgs, err := loadPackages()
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := loadTestSources(pkgs)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, k := range knownTestdataReaders {
		if k.reader == "" || k.owner == "" || k.reason == "" {
			t.Errorf("incomplete knownTestdataReaders entry %+v", k)
		}
		known[k.reader] = true
	}
	used := map[string]bool{}
	for _, s := range srcs {
		if strings.HasPrefix(s.Path, "tools/check/") { // its tests quote such paths
			continue
		}
		ms, err := unresolvedTestdata(path.Dir(s.Path), s.Src)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if known[path.Dir(s.Path)] {
				used[path.Dir(s.Path)] = true
				continue
			}
			t.Errorf("%s:%d: testdata path %s is neither resolved by the scan nor the package's own; resolve it or add it to knownTestdataReaders", s.Path, m.Line, m.Text)
		}
	}
	for k := range known {
		if !used[k] {
			t.Errorf("knownTestdataReaders lists %s but it has no unresolved testdata mention", k)
		}
	}
}
