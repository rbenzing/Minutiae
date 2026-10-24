package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonWriterViolations lists "file:func" for every call of json.Marshal, json.MarshalIndent or
// json.NewEncoder in src (a file called name) that the allow-list does not cover.
func jsonWriterViolations(t *testing.T, name string, src any, allowed map[string]bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "json" {
				return true
			}
			switch sel.Sel.Name {
			case "Marshal", "MarshalIndent", "NewEncoder":
				site := filepath.Base(name) + ":" + fd.Name.Name
				if !allowed[site] {
					bad = append(bad, site)
				}
			}
			return true
		})
	}
	return bad
}

func TestCLIJSONGoesThroughTheSafeWriter(t *testing.T) {
	allowed := map[string]bool{"records.go:quoteJSONString": true} // text mode, passed through escapeText
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "safejson.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range jsonWriterViolations(t, name, src, allowed) {
			t.Errorf("%s encodes JSON outside safejson.go; use marshalSafe or writeJSON", v)
		}
	}
}

func TestJSONWriterCheckerFindsPlantedCalls(t *testing.T) {
	got := jsonWriterViolations(t, "planted.go", "package cli\nimport \"encoding/json\"\nfunc f() { json.Marshal(1) }\nfunc g() { _ = json.NewEncoder(nil) }\nfunc h() { json.Unmarshal(nil, nil) }\n", nil)
	if len(got) != 2 || got[0] != "planted.go:f" || got[1] != "planted.go:g" {
		t.Errorf("violations %v, want f and g", got)
	}
	allow := map[string]bool{"planted.go:f": true}
	if got := jsonWriterViolations(t, "planted.go", "package cli\nimport \"encoding/json\"\nfunc f() { json.Marshal(1) }\n", allow); len(got) != 0 {
		t.Errorf("allowed site flagged: %v", got)
	}
}
