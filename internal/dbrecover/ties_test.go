package dbrecover

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// libraryConsts parses the non-test sources of internal/sqlitefile and returns
// the string values of its exported constants of type Relation and TableBasis
// and of its exported Method* constants, each sorted.
func libraryConsts(t *testing.T) (rels, bases, methods []string) {
	t.Helper()
	entries, err := os.ReadDir("../sqlitefile")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, "../sqlitefile/"+n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !name.IsExported() || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					typ := ""
					if id, ok := vs.Type.(*ast.Ident); ok {
						typ = id.Name
					}
					switch {
					case typ == "Relation":
						rels = append(rels, val)
					case typ == "TableBasis":
						bases = append(bases, val)
					case typ == "" && strings.HasPrefix(name.Name, "Method"):
						methods = append(methods, val)
					}
				}
			}
		}
	}
	slices.Sort(rels)
	slices.Sort(bases)
	slices.Sort(methods)
	return rels, bases, methods
}

func sorted(l []string) []string {
	out := slices.Clone(l)
	slices.Sort(out)
	return out
}

func baseStrings() []string {
	out := make([]string, len(bases))
	for i, b := range bases {
		out[i] = string(b)
	}
	slices.Sort(out)
	return out
}

// TestValidatorListsMatchSQLiteFile (J24, J29) ties the validator's relation
// and basis lists and rules.go's method and basis lists to the constants in
// sqlitefile's SOURCE, in both directions: a constant added or removed on
// either side fails it.
func TestValidatorListsMatchSQLiteFile(t *testing.T) {
	rels, bs, ms := libraryConsts(t)
	if len(rels) == 0 || len(bs) == 0 || len(ms) == 0 {
		t.Fatalf("parsed no constants: relations %v, bases %v, methods %v", rels, bs, ms)
	}
	for _, c := range []struct {
		what      string
		lib, ours []string
	}{
		{"validator relations", rels, sorted(relations)},
		{"validator bases", bs, sorted(tableBases)},
		{"rules.go bases", bs, baseStrings()},
		{"rules.go methods", ms, Methods()},
	} {
		if !slices.Equal(c.lib, c.ours) {
			t.Errorf("%s differ from sqlitefile's constants (update both sides together):\n lib  %v\n ours %v", c.what, c.lib, c.ours)
		}
	}
	// every relation and basis the library names is accepted with its label
	for _, r := range rels {
		p := validPayload()
		p["relation"] = r
		requireAccepted(t, "relation "+r, p)
	}
	for _, b := range bs {
		p := validPayload()
		p["table_basis"] = b
		if b == "none" {
			delete(p, "table")
		}
		requireAccepted(t, "basis "+b, p)
	}
}
