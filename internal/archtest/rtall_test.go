package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allPackageProblems reports what makes a source of internal/recordtypes/all more
// than a list of blank imports: a non-blank import or any declaration other than
// imports.
func allPackageProblems(name, src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.SkipObjectResolution)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			out = append(out, name+": declares something other than imports")
			continue
		}
		for _, s := range gd.Specs {
			if is := s.(*ast.ImportSpec); is.Name == nil || is.Name.Name != "_" {
				out = append(out, name+": import "+is.Path.Value+" is not blank")
			}
		}
	}
	return out
}

// TestRecordtypesAllHoldsOnlyBlankImports: the aggregation package is a list of
// blank imports and nothing else, so it cannot grow behaviour of its own.
func TestRecordtypesAllHoldsOnlyBlankImports(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "recordtypes", "all")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		seen++
		for _, p := range allPackageProblems(e.Name(), string(b)) {
			t.Error(p)
		}
	}
	if seen == 0 {
		t.Error("no non-test source file found in internal/recordtypes/all")
	}
}

func TestRecordtypesAllRuleSelfTest(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"blank imports only": {"package all\nimport (\n_ \"a\"\n_ \"b\"\n)\n", 0},
		"named import":       {"package all\nimport x \"a\"\n", 1},
		"plain import":       {"package all\nimport \"a\"\n", 1},
		"func":               {"package all\nimport _ \"a\"\nfunc F() {}\n", 1},
		"init":               {"package all\nfunc init() {}\n", 1},
		"var":                {"package all\nvar X = 1\n", 1},
		"const and type":     {"package all\nconst C = 1\ntype T int\n", 2},
	} {
		if got := len(allPackageProblems("x.go", tc.src)); got != tc.want {
			t.Errorf("%s: %d problems, want %d", name, got, tc.want)
		}
	}
}
