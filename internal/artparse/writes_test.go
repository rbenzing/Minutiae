package artparse_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bannedOS are the os functions that create, change or remove files and
// directories: artparse reaches the case only through evidence and records.
func bannedOS(name string) bool {
	switch name {
	case "Create", "OpenFile", "Rename", "WriteFile", "Truncate", "Chmod", "CreateTemp", "Symlink", "Link":
		return true
	}
	return strings.HasPrefix(name, "Remove") || strings.HasPrefix(name, "Mkdir")
}

// scanWrites returns what in one Go source file is a case-write primitive: a
// selector of a banned os function (under whatever name os was imported), a dot
// import of os, or an import of database/sql or modernc.org/....
func scanWrites(src string) []string {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		return []string{"does not parse: " + err.Error()}
	}
	var out []string
	osNames := map[string]bool{}
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		name := ""
		if is.Name != nil {
			name = is.Name.Name
		}
		switch {
		case p == "database/sql" || strings.HasPrefix(p, "modernc.org/"):
			out = append(out, "import "+p)
		case p == "os":
			if name == "." {
				out = append(out, "dot import of os")
			} else if name != "_" {
				if name == "" {
					name = "os"
				}
				osNames[name] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && osNames[id.Name] && bannedOS(sel.Sel.Name) {
			out = append(out, id.Name+"."+sel.Sel.Name)
		}
		return true
	})
	sort.Strings(out)
	return out
}

func TestArtparseHasNoCaseWritePrimitives(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f) //nolint:gosec // the package's own source
		if err != nil {
			t.Fatal(err)
		}
		if found := scanWrites(string(b)); len(found) != 0 {
			t.Errorf("%s uses case-write primitives: %v", f, found)
		}
	}
}

func TestWriteScannerSelfTest(t *testing.T) {
	flagged := map[string]string{
		"os.Create":           "package a\nimport \"os\"\nfunc f() { os.Create(\"x\") }\n",
		"os.OpenFile":         "package a\nimport \"os\"\nfunc f() { _, _ = os.OpenFile(\"x\", 0, 0) }\n",
		"os.Rename":           "package a\nimport \"os\"\nfunc f() { _ = os.Rename(\"a\", \"b\") }\n",
		"os.Remove":           "package a\nimport \"os\"\nfunc f() { _ = os.Remove(\"a\") }\n",
		"os.RemoveAll":        "package a\nimport \"os\"\nfunc f() { _ = os.RemoveAll(\"a\") }\n",
		"os.Mkdir":            "package a\nimport \"os\"\nfunc f() { _ = os.Mkdir(\"a\", 0) }\n",
		"os.MkdirAll":         "package a\nimport \"os\"\nfunc f() { _ = os.MkdirAll(\"a\", 0) }\n",
		"os.MkdirTemp":        "package a\nimport \"os\"\nfunc f() { _, _ = os.MkdirTemp(\"\", \"\") }\n",
		"os.WriteFile":        "package a\nimport \"os\"\nfunc f() { _ = os.WriteFile(\"a\", nil, 0) }\n",
		"os.Truncate":         "package a\nimport \"os\"\nfunc f() { _ = os.Truncate(\"a\", 0) }\n",
		"os.Chmod":            "package a\nimport \"os\"\nfunc f() { _ = os.Chmod(\"a\", 0) }\n",
		"method value":        "package a\nimport \"os\"\nvar f = os.Create\n",
		"aliased import":      "package a\nimport o \"os\"\nfunc f() { o.Create(\"x\") }\n",
		"dot import":          "package a\nimport . \"os\"\nfunc f() { Create(\"x\") }\n",
		"database/sql":        "package a\nimport _ \"database/sql\"\n",
		"modernc driver":      "package a\nimport _ \"modernc.org/sqlite\"\n",
		"modernc sub-package": "package a\nimport \"modernc.org/sqlite/lib\"\nvar _ = lib.X\n",
	}
	for name, src := range flagged {
		if got := scanWrites(src); len(got) == 0 {
			t.Errorf("%s: not flagged", name)
		}
	}
	harmless := map[string]string{
		"os.ReadFile":    "package a\nimport \"os\"\nfunc f() { _, _ = os.ReadFile(\"x\") }\n",
		"os.Getenv":      "package a\nimport \"os\"\nvar _ = os.Getenv\n",
		"os.ErrNotExist": "package a\nimport \"os\"\nvar _ = os.ErrNotExist\n",
		"comment":        "package a\n// os.Create(\"x\") and database/sql\nfunc f() {}\n",
		"string":         "package a\nvar s = \"os.Create\"\n",
		"other package":  "package a\nimport \"strings\"\nvar _ = strings.Builder{}\nfunc g() { var x struct{ Create func() }; x.Create() }\n",
		"local named os": "package a\ntype T struct{ Create func() }\nfunc f(os T) { os.Create() }\n",
	}
	for name, src := range harmless {
		if got := scanWrites(src); len(got) != 0 {
			t.Errorf("%s: flagged %v", name, got)
		}
	}
}
