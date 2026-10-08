package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Two programs that differ only in a token whose position the dump leaves out must still dump
// differently: the presence of the token is syntax.
func TestPresenceOfPositionOnlyTokensIsHashed(t *testing.T) {
	pairs := []struct{ name, a, b string }{
		{"ellipsis at a call", "package p\nfunc f(a []any) string { return fmt.Sprint(a...) }\n", "package p\nfunc f(a []any) string { return fmt.Sprint(a) }\n"},
		{"alias vs definition", "package p\ntype T = []byte\n", "package p\ntype T []byte\n"},
	}
	for _, p := range pairs {
		a, err := NormalizeGo([]byte(p.a), "a.go")
		if err != nil {
			t.Fatal(err)
		}
		b, err := NormalizeGo([]byte(p.b), "b.go")
		if err != nil {
			t.Fatal(err)
		}
		if string(a) == string(b) {
			t.Errorf("%s: both programs dump alike:\n%s", p.name, a)
		}
	}
}

// Layout never changes the dump even though presence of a token does.
func TestPositionsStillDoNotMatter(t *testing.T) {
	a, _ := NormalizeGo([]byte("package p\nfunc f(a []any) string { return fmt.Sprint(a...) }\n"), "a.go")
	b, _ := NormalizeGo([]byte("package p\n\n\nfunc f(a []any) string {\n\treturn fmt.Sprint(\n\t\ta...,\n\t)\n}\n"), "b.go")
	if string(a) != string(b) {
		t.Errorf("layout changed the dump:\n%s\n%s", a, b)
	}
}

// astNodes holds one value of every go/ast struct type that has a token.Pos field. The next test
// proves the list complete against the go/ast source of the toolchain in use, so a Go release that
// adds such a field cannot go unnoticed.
var astNodes = []any{
	ast.ArrayType{},
	ast.AssignStmt{},
	ast.BadDecl{},
	ast.BadExpr{},
	ast.BadStmt{},
	ast.BasicLit{},
	ast.BinaryExpr{},
	ast.BlockStmt{},
	ast.BranchStmt{},
	ast.CallExpr{},
	ast.CaseClause{},
	ast.ChanType{},
	ast.CommClause{},
	ast.Comment{},
	ast.CompositeLit{},
	ast.DeclStmt{},
	ast.DeferStmt{},
	ast.Ellipsis{},
	ast.EmptyStmt{},
	ast.FieldList{},
	ast.File{},
	ast.ForStmt{},
	ast.FuncDecl{},
	ast.FuncType{},
	ast.GenDecl{},
	ast.GoStmt{},
	ast.Ident{},
	ast.IfStmt{},
	ast.ImportSpec{},
	ast.IncDecStmt{},
	ast.IndexExpr{},
	ast.IndexListExpr{},
	ast.InterfaceType{},
	ast.KeyValueExpr{},
	ast.LabeledStmt{},
	ast.MapType{},
	ast.ParenExpr{},
	ast.RangeStmt{},
	ast.ReturnStmt{},
	ast.SelectStmt{},
	ast.SelectorExpr{},
	ast.SendStmt{},
	ast.SliceExpr{},
	ast.StarExpr{},
	ast.StructType{},
	ast.SwitchStmt{},
	ast.TypeAssertExpr{},
	ast.TypeSpec{},
	ast.TypeSwitchStmt{},
	ast.UnaryExpr{},
	ast.ValueSpec{},
	ast.Directive{},
	ast.DirectiveArg{},
}

func posFields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Type == posType {
			out = append(out, t.Field(i))
		}
	}
	return out
}

func TestEveryPosFieldIsHashedOrListedAsLayout(t *testing.T) {
	covered := map[string]bool{}
	for _, n := range astNodes {
		typ := reflect.TypeOf(n)
		covered[typ.Name()] = true
		for _, sf := range posFields(typ) {
			key := typ.Name() + "." + sf.Name
			if _, skip := layoutOnlyPos[key]; skip {
				continue
			}
			zero := reflect.New(typ).Elem()
			set := reflect.New(typ).Elem()
			set.FieldByName(sf.Name).SetInt(1)
			var a, b dumpBuf
			a.dump(zero)
			b.dump(set)
			if a.String() == b.String() {
				t.Errorf("%s: toggling the presence of the position does not change the dump and the field is not listed in layoutOnly", key)
			}
		}
	}

	// completeness against the go/ast source of this toolchain
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skipf("go env GOROOT: %v", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "src", "go", "ast")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("go/ast source not readable: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		files = append(files, f)
	}
	{
		for _, f := range files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, s := range gd.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, fl := range st.Fields.List {
						sel, ok := fl.Type.(*ast.SelectorExpr)
						if ok && sel.Sel.Name == "Pos" && !covered[ts.Name.Name] {
							t.Errorf("go/ast.%s has a token.Pos field and is missing from astNodes", ts.Name.Name)
						}
					}
				}
			}
		}
	}
}

// dumpBuf dumps one reflect value with the production dumper.
type dumpBuf struct{ bytes.Buffer }

func (d *dumpBuf) dump(v reflect.Value) { dumpValue(&d.Buffer, v) }
