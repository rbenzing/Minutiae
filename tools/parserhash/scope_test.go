package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testModule = "example.test/m"

// writeTree writes files (slash paths) under a fresh module root with a go.mod.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	all := map[string]string{"go.mod": "module " + testModule + "\n\ngo 1.26\n"}
	for k, v := range files {
		all[k] = v
	}
	for name, body := range all {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testConfig(root string) Config {
	return Config{ModuleRoot: root, ModulePath: testModule}
}

func readFromDisk(dir, file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
}

// hashTree resolves and hashes the package rel (e.g. "internal/parsers/a").
func hashTree(t *testing.T, root, rel string) string {
	t.Helper()
	s, err := ResolveScope(testConfig(root), testModule+"/"+rel)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	h, err := HashScope(s, readFromDisk)
	if err != nil {
		t.Fatalf("HashScope: %v", err)
	}
	return h
}

func importPaths(s Scope) []string {
	var out []string
	for _, p := range s.Packages {
		out = append(out, p.ImportPath)
	}
	return out
}

func pkgFiles(t *testing.T, s Scope, importPath string) []string {
	t.Helper()
	for _, p := range s.Packages {
		if p.ImportPath == importPath {
			return p.Files
		}
	}
	t.Fatalf("package %s not in scope %v", importPath, importPaths(s))
	return nil
}

func TestScopeFollowsRealImports(t *testing.T) {
	files := map[string]string{
		"internal/parsers/a/a.go":          "package a\n\nimport (\n\t\"example.test/m/internal/decode/x\"\n\t\"example.test/m/internal/records\"\n)\n\nvar _ = x.V\nvar _ = records.V\n",
		"internal/parsers/b/b.go":          "package b\n\nvar V = 1\n",
		"internal/decode/x/x.go":           "package x\n\nvar V = 1\n",
		"internal/records/r.go":            "package records\n\nvar V = 1\n",
		"internal/parsers/a/a_test.go":     "package a\n\nvar T = 1\n",
		"internal/parsers/a/testdata/t.go": "package t\n\nvar T = 1\n",
	}
	root := writeTree(t, files)
	s, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{testModule + "/internal/decode/x", testModule + "/internal/parsers/a"}
	if got := importPaths(s); !slices.Equal(got, want) {
		t.Fatalf("scope packages = %v, want %v", got, want)
	}
	if got := pkgFiles(t, s, testModule+"/internal/parsers/a"); !slices.Equal(got, []string{"a.go"}) {
		t.Fatalf("a files = %v (test files and testdata must not be hashed)", got)
	}
	ha, hb := hashTree(t, root, "internal/parsers/a"), hashTree(t, root, "internal/parsers/b")

	// Changing decoder X changes A only.
	files["internal/decode/x/x.go"] = "package x\n\nvar V = 2\n"
	root2 := writeTree(t, files)
	if hashTree(t, root2, "internal/parsers/a") == ha {
		t.Error("changing an in-scope decoder must change parser A")
	}
	if hashTree(t, root2, "internal/parsers/b") != hb {
		t.Error("changing decoder X must not change parser B")
	}
	// A package outside the prefixes (records) and a test file never enter the scope.
	files["internal/records/r.go"] = "package records\n\nvar V = 99\n"
	files["internal/parsers/a/a_test.go"] = "package a\n\nvar T = 2\n"
	root3 := writeTree(t, files)
	if hashTree(t, root3, "internal/parsers/a") != hashTree(t, root2, "internal/parsers/a") {
		t.Error("out-of-scope package or test file changed the hash")
	}
}

func TestScopeIgnoresBuildConstraints(t *testing.T) {
	files := map[string]string{
		"internal/parsers/a/a.go":         "package a\n\nvar V = 1\n",
		"internal/parsers/a/a_windows.go": "package a\n\nimport _ \"example.test/m/internal/decode/y\"\n",
		"internal/parsers/a/other.go":     "//go:build windows && amd64\n\npackage a\n\nvar W = 1\n",
		"internal/decode/y/y.go":          "package y\n\nvar V = 1\n",
	}
	root := writeTree(t, files)
	s, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
	if err != nil {
		t.Fatal(err)
	}
	if got := pkgFiles(t, s, testModule+"/internal/parsers/a"); !slices.Equal(got, []string{"a.go", "a_windows.go", "other.go"}) {
		t.Fatalf("files = %v", got)
	}
	if !slices.Contains(importPaths(s), testModule+"/internal/decode/y") {
		t.Fatalf("import of the constrained file not followed: %v", importPaths(s))
	}
	h := hashTree(t, root, "internal/parsers/a")
	files["internal/parsers/a/other.go"] = "//go:build windows && amd64\n\npackage a\n\nvar W = 2\n"
	if hashTree(t, writeTree(t, files), "internal/parsers/a") == h {
		t.Error("an edit in a constrained-out file must change the hash")
	}
}

func TestScopeSkipsIgnoreTaggedGeneratorFile(t *testing.T) {
	files := map[string]string{
		"internal/parsers/a/a.go":   "package a\n\nvar V = 1\n",
		"internal/parsers/a/gen.go": "//go:build ignore\n\npackage main\n\nimport _ \"example.test/m/internal/decode/z\"\n\nfunc main() {}\n",
		"internal/decode/z/z.go":    "package z\n",
	}
	root := writeTree(t, files)
	s, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
	if err != nil {
		t.Fatalf("generator file caused an error: %v", err)
	}
	if got := pkgFiles(t, s, testModule+"/internal/parsers/a"); !slices.Equal(got, []string{"a.go"}) {
		t.Fatalf("files = %v, generator must not be hashed", got)
	}
	if slices.Contains(importPaths(s), testModule+"/internal/decode/z") {
		t.Fatal("generator import followed")
	}

	files["internal/parsers/a/b.go"] = "package other\n"
	_, err = ResolveScope(testConfig(writeTree(t, files)), testModule+"/internal/parsers/a")
	if err == nil || !strings.Contains(err.Error(), "package names") {
		t.Fatalf("two package names: err = %v, want a package names error", err)
	}
}

func TestScopeRejectsAssemblyCAndCgoSources(t *testing.T) {
	for _, name := range []string{"x.s", "x.S", "x.c", "x.h", "x.cc", "x.cpp", "x.m", "x.f", "x.syso", "x.swig"} {
		root := writeTree(t, map[string]string{
			"internal/parsers/a/a.go":    "package a\n",
			"internal/parsers/a/" + name: "x",
		})
		_, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want an error naming the file", name, err)
		}
	}
	root := writeTree(t, map[string]string{
		"internal/parsers/a/a.go": "package a\n",
		"internal/parsers/a/c.go": "package a\n\nimport \"C\"\n",
	})
	_, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
	if err == nil || !strings.Contains(err.Error(), "c.go") {
		t.Errorf("import C: err = %v, want an error naming c.go", err)
	}
	// A non-Go file in a directory outside the scope is fine.
	root = writeTree(t, map[string]string{
		"internal/parsers/a/a.go": "package a\n",
		"internal/other/x.c":      "x",
	})
	if _, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a"); err != nil {
		t.Errorf("unrelated .c file: %v", err)
	}
}

func TestScopeIncludesEmbeddedFilesOnly(t *testing.T) {
	src := "package a\n\nimport _ \"embed\"\n\n//go:embed data.txt sub\nvar d string\n"
	files := map[string]string{
		"internal/parsers/a/a.go":        src,
		"internal/parsers/a/data.txt":    "one",
		"internal/parsers/a/README.md":   "doc",
		"internal/parsers/a/sub/k.bin":   "k",
		"internal/parsers/a/sub/.hid":    "h",
		"internal/parsers/a/sub/_und":    "u",
		"internal/parsers/a/sub/n/m.bin": "m",
	}
	root := writeTree(t, files)
	s, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.go", "data.txt", "sub/k.bin", "sub/n/m.bin"}
	if got := pkgFiles(t, s, testModule+"/internal/parsers/a"); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	h := hashTree(t, root, "internal/parsers/a")

	files["internal/parsers/a/README.md"] = "changed doc"
	if hashTree(t, writeTree(t, files), "internal/parsers/a") != h {
		t.Error("an unreferenced README changed the hash")
	}
	files["internal/parsers/a/data.txt"] = "two"
	if hashTree(t, writeTree(t, files), "internal/parsers/a") == h {
		t.Error("an embedded data file did not change the hash")
	}

	for _, pat := range []string{"../x", "/abs", "nomatch.txt", "a/../b", "[", "dir/", "./data.txt"} {
		f := map[string]string{
			"internal/parsers/a/a.go":     "package a\n\nimport _ \"embed\"\n\n//go:embed " + pat + "\nvar d string\n",
			"internal/parsers/a/data.txt": "x",
		}
		if _, err := ResolveScope(testConfig(writeTree(t, f)), testModule+"/internal/parsers/a"); err == nil {
			t.Errorf("embed pattern %q: want an error", pat)
		}
	}
}

func TestExpandEmbed(t *testing.T) {
	root := writeTree(t, map[string]string{
		"p/a.txt":    "a",
		"p/b.txt":    "b",
		"p/.dot":     "d",
		"p/_us":      "u",
		"p/d/x":      "x",
		"p/d/.y":     "y",
		"p/d/_z":     "z",
		"p/d/e/w":    "w",
		"p/q/go.mod": "module q\n",
		"p/q/f":      "f",
	})
	dir := filepath.Join(root, "p")
	cases := []struct {
		pats []string
		want []string
	}{
		{[]string{"*.txt"}, []string{"a.txt", "b.txt"}},
		{[]string{"d"}, []string{"d/e/w", "d/x"}},
		{[]string{"all:d"}, []string{"d/.y", "d/_z", "d/e/w", "d/x"}},
		{[]string{".dot", "a.txt", "a.txt"}, []string{".dot", "a.txt"}},
	}
	for _, c := range cases {
		got, err := expandEmbed(dir, c.pats)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%v: got %v, %v; want %v", c.pats, got, err, c.want)
		}
	}
	if _, err := expandEmbed(dir, []string{"q"}); err == nil {
		t.Error("a directory holding go.mod must be an error")
	}
}

func TestFindSum(t *testing.T) {
	sum := []byte("example.org/x v1.2.3 h1:AAAA=\nexample.org/x v1.2.3/go.mod h1:BBBB=\nexample.org/y v1.0.0/go.mod h1:CCCC=\n")
	got, err := findSum(sum, "example.org/x", "v1.2.3")
	if err != nil || got != "h1:AAAA=" {
		t.Fatalf("findSum = %q, %v", got, err)
	}
	for _, c := range [][2]string{{"example.org/y", "v1.0.0"}, {"example.org/x", "v1.2.4"}, {"example.org/z", "v1.0.0"}} {
		if s, err := findSum(sum, c[0], c[1]); err == nil {
			t.Errorf("findSum(%v) = %q, want an error (a /go.mod line is never used)", c, s)
		}
	}
}

func TestScopeThirdPartyModuleLines(t *testing.T) {
	old := listModule
	defer func() { listModule = old }()
	listModule = func(_, importPath string) (string, string, error) {
		if !strings.HasPrefix(importPath, "example.org/dep") {
			t.Fatalf("unexpected list of %s", importPath)
		}
		return "example.org/dep", "v1.2.3", nil
	}
	files := map[string]string{
		"internal/parsers/a/a.go": "package a\n\nimport (\n\t\"fmt\"\n\t\"example.org/dep/sub\"\n)\n\nvar _ = fmt.Sprint\nvar _ = sub.V\n",
		"go.sum":                  "example.org/dep v1.2.3 h1:AAAA=\nexample.org/dep v1.2.3/go.mod h1:BBBB=\n",
	}
	s, err := ResolveScope(testConfig(writeTree(t, files)), testModule+"/internal/parsers/a")
	if err != nil {
		t.Fatal(err)
	}
	want := []ModuleRef{{Path: "example.org/dep", Version: "v1.2.3", Sum: "h1:AAAA="}}
	if !slices.Equal(s.Modules, want) {
		t.Fatalf("modules = %v, want %v", s.Modules, want)
	}
	read := func(string, string) ([]byte, error) { return []byte("package a\n"), nil }
	h1, err := HashScope(s, read)
	if err != nil {
		t.Fatal(err)
	}
	s2 := s
	s2.Modules = []ModuleRef{{Path: "example.org/dep", Version: "v1.2.4", Sum: "h1:AAAA="}}
	h2, _ := HashScope(s2, read)
	s3 := s
	s3.Modules = []ModuleRef{{Path: "example.org/dep", Version: "v1.2.3", Sum: "h1:ZZZZ="}}
	h3, _ := HashScope(s3, read)
	if h1 == h2 || h1 == h3 || h2 == h3 || h2 == "" || h3 == "" {
		t.Errorf("version or sum change must change the hash: %s %s %s", h1, h2, h3)
	}

	delete(files, "go.sum")
	if _, err := ResolveScope(testConfig(writeTree(t, files)), testModule+"/internal/parsers/a"); err == nil {
		t.Error("a missing go.sum line must be an error")
	}
}

func TestListModuleReal(t *testing.T) {
	dep := writeTree(t, map[string]string{"go.mod": "module example.org/dep\n\ngo 1.26\n", "dep.go": "package dep\n"})
	root := writeTree(t, map[string]string{
		"go.mod": "module " + testModule + "\n\ngo 1.26\n\nrequire example.org/dep v0.0.0\n\nreplace example.org/dep => " + filepath.ToSlash(dep) + "\n",
		"m.go":   "package m\n\nimport _ \"example.org/dep\"\n",
	})
	path, _, err := listModule(root, "example.org/dep")
	if err != nil || path != "example.org/dep" {
		t.Fatalf("listModule = %q %v", path, err)
	}
	if p, v, err := listModule(root, "fmt"); err != nil || p != "" || v != "" {
		t.Fatalf("stdlib listModule = %q %q %v", p, v, err)
	}
}

func TestGoDirectiveRead(t *testing.T) {
	root := writeTree(t, map[string]string{"internal/parsers/a/a.go": "package a\n"})
	s, err := ResolveScope(testConfig(root), testModule+"/internal/parsers/a")
	if err != nil || s.GoDirective != "1.26" {
		t.Fatalf("GoDirective = %q, %v", s.GoDirective, err)
	}
}
