package parsers

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	_ "github.com/rbenzing/minutiae/internal/recordtypes/all"
)

// fake is a parser defined in this test file, so its package path is
// .../internal/parsers (the package under test), never a parser package.
type fake struct {
	meta     parse.Meta
	mappings []parse.TableMapping
}

func (f *fake) Meta() parse.Meta { return f.meta }
func (f *fake) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{}, nil
}
func (f *fake) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

// fakeMapper adds the RowMapper capability.
type fakeMapper struct{ fake }

func (f *fakeMapper) Mappings() []parse.TableMapping { return f.mappings }

// fnParser is a func-typed parser.
type fnParser func()

func (fnParser) Meta() parse.Meta { return goodMeta("fn") }
func (fnParser) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{}, nil
}
func (fnParser) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

// fnValue is a parser held by value.
type fnValue struct{}

func (fnValue) Meta() parse.Meta { return goodMeta("val") }
func (fnValue) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{}, nil
}
func (fnValue) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

const (
	typeOK        = "zz_t9_ok"
	typeNoVal     = "zz_t9_noval"
	typeVersioned = "zz_t9_v2"
)

func init() {
	ok := func(map[string]any) error { return nil }
	for _, t := range []records.Type{
		{Name: typeOK, PayloadVersion: 1, Validate: ok},
		{Name: typeNoVal, PayloadVersion: 1},
		{Name: typeVersioned, PayloadVersion: 2, Validate: ok},
	} {
		if err := records.RegisterType(t); err != nil {
			panic(err)
		}
	}
}

func goodMeta(name string) parse.Meta {
	return parse.Meta{
		Name: name, Version: "1.0.0", Title: "T", Platforms: []string{parse.PlatformAndroid},
		Emits:  []parse.Emit{{Type: typeOK, PayloadVersion: 1}},
		Inputs: []parse.InputSpec{{Role: "db", Globs: []string{"android:/data/x.db"}, Required: true}},
	}
}

const (
	base    = "github.com/rbenzing/minutiae/internal/parsers"
	goodPkg = base + "/good"
)

func TestRegistryWellFormed(t *testing.T) {
	if errs := Validate(All()); len(errs) != 0 {
		t.Fatalf("the registry is not well formed: %v", errs)
	}
}

func TestRegistryValidateCatchesBadSets(t *testing.T) {
	mk := func(name string, edit func(*parse.Meta)) *fake {
		m := goodMeta(name)
		if edit != nil {
			edit(&m)
		}
		return &fake{meta: m}
	}
	withClaim := func(m *parse.Meta) { m.Claims = []parse.TableClaim{{Role: "db", Table: "calls"}} }
	mapper := func(m parse.Meta, tables ...string) *fakeMapper {
		var tm []parse.TableMapping
		for _, tb := range tables {
			tm = append(tm, parse.TableMapping{Table: tb})
		}
		return &fakeMapper{fake{meta: m, mappings: tm}}
	}
	claimMeta := func() parse.Meta {
		m := goodMeta("a")
		withClaim(&m)
		return m
	}
	wantPkg := "is not " + base + "/<name>"
	for name, tc := range map[string]struct {
		set    []parse.Parser
		pkg    func(parse.Parser) string
		noHash bool
		claims bool
		want   string
	}{
		"nil parser":      {set: []parse.Parser{nil}, want: "entry 0 is nil"},
		"bad version":     {set: []parse.Parser{mk("a", func(m *parse.Meta) { m.Version = "1.0" })}, want: "version"},
		"duplicate names": {set: []parse.Parser{mk("dup", nil), mk("dup", nil)}, want: `duplicate parser name "dup"`},
		"unregistered type": {
			set:  []parse.Parser{mk("a", func(m *parse.Meta) { m.Emits = []parse.Emit{{Type: "zz_nope", PayloadVersion: 1}} })},
			want: `emits type "zz_nope" that is not registered`,
		},
		"wrong payload version": {
			set:  []parse.Parser{mk("a", func(m *parse.Meta) { m.Emits = []parse.Emit{{Type: typeVersioned, PayloadVersion: 1}} })},
			want: `emits type "` + typeVersioned + `" at payload version 1, registered at 2`,
		},
		"type without validator": {
			set:  []parse.Parser{mk("a", func(m *parse.Meta) { m.Emits = []parse.Emit{{Type: typeNoVal, PayloadVersion: 1}} })},
			want: `emits type "` + typeNoVal + `", which has no validator`,
		},
		"package outside internal/parsers": {
			set: []parse.Parser{mk("a", nil)}, pkg: func(parse.Parser) string { return "github.com/rbenzing/minutiae/internal/other/x" }, want: wantPkg,
		},
		"the parsers package itself":          {set: []parse.Parser{mk("a", nil)}, pkg: func(parse.Parser) string { return base }, want: wantPkg},
		"parsertest":                          {set: []parse.Parser{mk("a", nil)}, pkg: func(parse.Parser) string { return base + "/parsertest" }, want: wantPkg},
		"nested directory":                    {set: []parse.Parser{mk("a", nil)}, pkg: func(parse.Parser) string { return base + "/ios/sms" }, want: wantPkg},
		"directory that is not an identifier": {set: []parse.Parser{mk("a", nil)}, pkg: func(parse.Parser) string { return base + "/my-parser" }, want: wantPkg},
		"directory that starts with a digit":  {set: []parse.Parser{mk("a", nil)}, pkg: func(parse.Parser) string { return base + "/1abc" }, want: wantPkg},
		"func-typed parser has no package":    {set: []parse.Parser{fnParser(nil)}, pkg: PackagePath, want: wantPkg},
		"claims while disabled":               {set: []parse.Parser{mk("a", withClaim)}, want: "declares claims while claims are disabled"},
		"claims outside mappings":             {set: []parse.Parser{mapper(claimMeta(), "messages")}, claims: true, want: `claims table "calls" that no mapping handles`},
		"missing hash":                        {set: []parse.Parser{mk("a", nil)}, noHash: true, want: "has no generated source hash"},
	} {
		t.Run(name, func(t *testing.T) {
			r := rules{
				base:          base,
				pkgPath:       func(parse.Parser) string { return goodPkg },
				hashOf:        func(parse.Parser, parse.Meta) (string, bool) { return "h", true },
				claimsEnabled: tc.claims,
			}
			if tc.pkg != nil {
				r.pkgPath = tc.pkg
			}
			if tc.noHash {
				r.hashOf = func(parse.Parser, parse.Meta) (string, bool) { return "", false }
			}
			errs := r.validate(tc.set)
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tc.want) {
				t.Fatalf("errors = %v, want exactly one containing %q", errs, tc.want)
			}
		})
	}

	t.Run("a good set and claims inside mappings pass", func(t *testing.T) {
		r := rules{
			base: base, pkgPath: func(parse.Parser) string { return goodPkg },
			hashOf: func(parse.Parser, parse.Meta) (string, bool) { return "h", true }, claimsEnabled: true,
		}
		if errs := r.validate([]parse.Parser{mk("plain", nil), mapper(claimMeta(), "CALLS")}); len(errs) != 0 {
			t.Errorf("errors = %v", errs)
		}
	})
	t.Run("the shipped rules refuse a fake that has no package and no identity", func(t *testing.T) {
		errs := Validate([]parse.Parser{mk("a", nil)})
		if len(errs) < 2 {
			t.Fatalf("Validate = %v, want a package error and a hash error", errs)
		}
	})
}

func TestHashOfLooksUpNameAtVersionAndPackage(t *testing.T) {
	p := &fake{meta: goodMeta("x.y")}
	table := map[string]identity{"x.y@1.0.0": {Package: PackagePath(p), Hash: "sha256:abc"}}
	if h, ok := hashOf(p, table); !ok || h != "sha256:abc" {
		t.Errorf("hashOf(own package) = %q, %v", h, ok)
	}
	// the same name and version defined in another package gets nothing
	elsewhere := map[string]identity{"x.y@1.0.0": {Package: base + "/elsewhere", Hash: "h"}}
	if h, ok := hashOf(p, elsewhere); ok {
		t.Errorf("a parser of another package inherited an identity: %q", h)
	}
	// another version of the same name gets nothing
	q := &fake{meta: goodMeta("x.y")}
	q.meta.Version = "1.0.1"
	if _, ok := hashOf(q, table); ok {
		t.Error("another version inherited the identity")
	}
	if _, ok := hashOf(nil, table); ok {
		t.Error("nil has an identity")
	}
	if _, ok := HashOf(p); ok {
		t.Error("HashOf found an identity in the empty generated table")
	}
}

func TestPackagePath(t *testing.T) {
	want := reflect.TypeOf(fake{}).PkgPath()
	if !strings.HasSuffix(want, "/internal/parsers") {
		t.Fatalf("unexpected package path %q", want)
	}
	for name, tc := range map[string]struct {
		p    parse.Parser
		want string
	}{
		"pointer":    {&fake{}, want},
		"value":      {fnValue{}, want},
		"pointer to": {&fakeMapper{}, want},
		"func-typed": {fnParser(nil), ""},
		"nil":        {nil, ""},
	} {
		if got := PackagePath(tc.p); got != tc.want {
			t.Errorf("%s: PackagePath = %q, want %q", name, got, tc.want)
		}
	}
}

func TestIdentityGeneratedStartsEmpty(t *testing.T) {
	if len(generated) != 0 || len(All()) != 0 {
		t.Errorf("4A ships no parsers: generated=%d All=%d", len(generated), len(All()))
	}
}

// explicitProblems checks the sources of one package: no func init, and no
// assignment (=, op=, ++, --, &x) to a package-level variable anywhere, so the
// registry is a literal list and nothing registers itself.
func explicitProblems(srcs map[string]string) []string {
	fset := token.NewFileSet()
	var files []*ast.File
	var out []string
	pkgVars := map[string]bool{}
	for n, src := range srcs {
		f, err := parser.ParseFile(fset, n, src, parser.SkipObjectResolution)
		if err != nil {
			return []string{n + ": " + err.Error()}
		}
		files = append(files, f)
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, s := range gd.Specs {
					for _, id := range s.(*ast.ValueSpec).Names {
						pkgVars[id.Name] = true
					}
				}
			}
		}
	}
	root := func(e ast.Expr) string {
		for {
			switch x := e.(type) {
			case *ast.Ident:
				return x.Name
			case *ast.IndexExpr:
				e = x.X
			case *ast.SelectorExpr:
				e = x.X
			case *ast.StarExpr:
				e = x.X
			case *ast.ParenExpr:
				e = x.X
			default:
				return ""
			}
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Recv == nil && fd.Name.Name == "init" {
				out = append(out, fset.Position(fd.Pos()).String()+": func init")
			}
			if fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					if x.Tok == token.DEFINE {
						return true
					}
					for _, l := range x.Lhs {
						if pkgVars[root(l)] {
							out = append(out, fset.Position(x.Pos()).String()+": assigns to package variable "+root(l))
						}
					}
				case *ast.IncDecStmt:
					if pkgVars[root(x.X)] {
						out = append(out, fset.Position(x.Pos()).String()+": changes package variable "+root(x.X))
					}
				case *ast.UnaryExpr:
					if x.Op == token.AND && pkgVars[root(x.X)] {
						out = append(out, fset.Position(x.Pos()).String()+": takes the address of package variable "+root(x.X))
					}
				}
				return true
			})
		}
	}
	return out
}

func TestRegistryIsExplicit(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		srcs[e.Name()] = string(b)
	}
	if len(srcs) == 0 {
		t.Fatal("no source files found")
	}
	for _, p := range explicitProblems(srcs) {
		t.Error(p)
	}
}

func TestExplicitRuleSelfTest(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"literal list":      {"package p\nvar generated = map[string]int{}\nfunc All() []int { return []int{1} }\n", 0},
		"func init":         {"package p\nfunc init() {}\n", 1},
		"assign":            {"package p\nvar v int\nfunc F() { v = 2 }\n", 1},
		"op assign":         {"package p\nvar v int\nfunc F() { v += 2 }\n", 1},
		"increment":         {"package p\nvar v int\nfunc F() { v++ }\n", 1},
		"map element":       {"package p\nvar m = map[string]int{}\nfunc F() { m[\"a\"] = 1 }\n", 1},
		"address taken":     {"package p\nvar v int\nfunc F() *int { return &v }\n", 1},
		"local define ok":   {"package p\nvar v int\nfunc F() { w := 1; w = 2; _ = w }\n", 0},
		"method named init": {"package p\ntype T int\nfunc (T) init() {}\n", 0},
	} {
		if got := len(explicitProblems(map[string]string{"x.go": tc.src})); got != tc.want {
			t.Errorf("%s: %d problems, want %d", name, got, tc.want)
		}
	}
}

// countingParser counts Meta calls; with boom set Meta panics.
type countingParser struct {
	fake
	calls *int
	boom  bool
}

func (c *countingParser) Meta() parse.Meta {
	*c.calls++
	if c.boom {
		panic("hostile meta")
	}
	return c.meta
}

func TestValidateCallsMetaOncePerParser(t *testing.T) {
	n := 0
	p := &countingParser{fake: fake{meta: goodMeta("once")}, calls: &n}
	Validate([]parse.Parser{p})
	if n != 1 {
		t.Errorf("Meta called %d times, want 1", n)
	}
}

func TestValidateRecoversMetaPanicNamingPackage(t *testing.T) {
	n := 0
	p := &countingParser{fake: fake{meta: goodMeta("boom")}, calls: &n, boom: true}
	var errs []error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Validate panicked: %v", r)
			}
		}()
		errs = Validate([]parse.Parser{p, &fake{meta: goodMeta("next")}})
	}()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "Meta panicked") && strings.Contains(e.Error(), base) {
			found = true
		}
	}
	if !found {
		t.Errorf("errors = %v, want one naming the package and the Meta panic", errs)
	}
	if n != 1 {
		t.Errorf("Meta called %d times after a panic, want 1", n)
	}
}
