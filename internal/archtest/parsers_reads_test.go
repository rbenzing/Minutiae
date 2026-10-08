package archtest

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

// parsersRecordsAllowed are the only identifiers internal/parsers may take from
// internal/records: reads of the type registry. No writer, no registration.
var parsersRecordsAllowed = map[string]bool{"Types": true, "TypeInfo": true, "LookupType": true}

const recordsImport = "github.com/rbenzing/minutiae/internal/records"

// recordsSelectorProblems lists the uses of internal/records in one source that
// are not registry reads: a selector outside parsersRecordsAllowed, a dot import
// or a blank import.
func recordsSelectorProblems(name, src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	locals := map[string]bool{}
	var out []string
	for _, is := range f.Imports {
		p, _ := strconv.Unquote(is.Path.Value)
		if p != recordsImport {
			continue
		}
		local := "records"
		if is.Name != nil {
			local = is.Name.Name
		}
		if local == "." || local == "_" {
			out = append(out, name+": imports internal/records as "+local+" (only qualified registry reads are allowed)")
			continue
		}
		locals[local] = true
	}
	if len(locals) == 0 {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := se.X.(*ast.Ident); ok && locals[id.Name] && !parsersRecordsAllowed[se.Sel.Name] {
				out = append(out, fset.Position(se.Pos()).String()+": records."+se.Sel.Name+" is not a registry read (allowed: Types, TypeInfo, LookupType)")
			}
		}
		return true
	})
	return out
}

// TestParsersUsesOnlyRegistryReads: internal/parsers imports internal/records for
// the type registry only, so it can never write a record or register a type.
func TestParsersUsesOnlyRegistryReads(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "parsers")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no non-test source file in internal/parsers")
	}
	used := false
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), recordsImport) {
			used = true
		}
		for _, p := range recordsSelectorProblems(n, string(b)) {
			t.Error(p)
		}
	}
	if !used {
		t.Error("internal/parsers no longer imports internal/records: the rule would pass vacuously")
	}
}

func TestParsersRegistryReadsRuleSelfTest(t *testing.T) {
	const head = "package p\nimport \"" + recordsImport + "\"\n"
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"LookupType":      {head + "var _ = records.LookupType\n", 0},
		"Types":           {head + "var _ = records.Types\n", 0},
		"TypeInfo":        {head + "var _ records.TypeInfo\n", 0},
		"NewWriter":       {head + "var _ = records.NewWriter\n", 1},
		"RegisterType":    {head + "func f() { _ = records.RegisterType(records.Type{}) }\n", 2},
		"SetValidator":    {head + "func f() { records.SetValidator(\"x\", nil) }\n", 1},
		"aliased":         {"package p\nimport r \"" + recordsImport + "\"\nvar _ = r.NewWriter\n", 1},
		"two aliases":     {"package p\nimport r \"" + recordsImport + "\"\nimport records \"" + recordsImport + "\"\nvar _ = records.Types\nvar _ = r.NewWriter\n", 1},
		"dot import":      {"package p\nimport . \"" + recordsImport + "\"\n", 1},
		"blank import":    {"package p\nimport _ \"" + recordsImport + "\"\n", 1},
		"other package":   {"package p\nimport \"fmt\"\nvar _ = fmt.Sprint\n", 0},
		"unrelated ident": {head + "var records2 = 1\nvar _ = records2\n", 0},
	} {
		if got := len(recordsSelectorProblems("x.go", tc.src)); got != tc.want {
			t.Errorf("%s: %d problems, want %d", name, got, tc.want)
		}
	}
}
