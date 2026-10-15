package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const testMod = "example.com/m"

// listJSON renders the `go list -json` shape for a fake module rooted at root.
func listJSON(t *testing.T, root string, pkgs []map[string]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range pkgs {
		rel := p["rel"].(string)
		delete(p, "rel")
		p["Dir"] = filepath.Join(root, filepath.FromSlash(rel))
		p["ImportPath"] = testMod + "/" + rel
		if err := enc.Encode(p); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func fakeGraph(t *testing.T) []pkg {
	t.Helper()
	root := t.TempDir()
	data := listJSON(t, root, []map[string]any{
		{"rel": "a"},
		{"rel": "b", "Imports": []string{testMod + "/a", "fmt"}},
		{"rel": "c", "TestImports": []string{testMod + "/b"}},
		{"rel": "d", "XTestImports": []string{testMod + "/c"}},
		{"rel": "e", "Imports": []string{"fmt", "os"}},
		{"rel": "f", "TestImports": []string{testMod + "/a"}, "EmbedFiles": []string{"assets/x.txt"}},
		{"rel": "g/h", "Imports": []string{testMod + "/e"}},
	})
	pkgs, err := parseGoList(data, testMod, root)
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

func TestAffectedClosureIncludesTestImporters(t *testing.T) {
	pkgs := fakeGraph(t)
	cases := []struct {
		changed string
		want    []string
	}{
		// b imports a, c's test imports b, d's external test imports c, f's test imports a.
		{"a", []string{"a", "b", "c", "d", "f"}},
		{"b", []string{"b", "c", "d"}},
		{"c", []string{"c", "d"}},
		{"d", []string{"d"}},
		{"e", []string{"e", "g/h"}},
		{"g/h", []string{"g/h"}},
	}
	for _, c := range cases {
		got := affectedClosure(pkgs, map[string]bool{c.changed: true})
		if !slices.Equal(got, c.want) {
			t.Errorf("changing %s: affected %v, want %v", c.changed, got, c.want)
		}
	}
}

func TestAffectedFileInTestdataBelongsToItsPackage(t *testing.T) {
	pkgs := fakeGraph(t)
	cases := []struct {
		file string
		want []string
	}{
		{"b/testdata/img.bin", []string{"b"}},
		{"b/testdata/deep/er/x.json", []string{"b"}},
		{"b/readme.txt", []string{"b"}},
		{"f/assets/x.txt", []string{"f"}},     // embedded
		{"f/assets/other.txt", []string{"f"}}, // nearest ancestor package
		{"b/testdata_not/x", []string{"b"}},   // nearest ancestor package
		{"b/sub/testdata/x", []string{"b"}},   // no package at b/sub: nearest ancestor
		{"g/h/testdata/x", []string{"g/h"}},   // nested package, not g
		{"docs/testing.md", nil},              // belongs to no package
		{"b/x.go", []string{"b"}},
	}
	for _, c := range cases {
		hit, _ := changedPackages([]string{c.file}, pkgs)
		got := sortedKeys(hit)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: packages %v, want %v", c.file, got, c.want)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEscalationRules(t *testing.T) {
	pkgs := fakeGraph(t)
	src := map[string]string{
		"b/cgo.go":           "//go:build cgo\n\npackage b\n",
		"b/nocgo.go":         "// Copyright\n\n//go:build !cgo\n\npackage b\n",
		"b/os.go":            "//go:build linux && !android\n\npackage b\n",
		"b/arch.go":          "//go:build (amd64 || arm64) && foo\n\npackage b\n",
		"b/unixy.go":         "//go:build unix\n\npackage b\n",
		"b/tagged.go":        "//go:build realimages\n\npackage b\n",
		"b/ignored.go":       "//go:build ignore\n\npackage main\n",
		"b/plain.go":         "package b\n\n// go:build cgo in a comment after the clause\n",
		"b/late.go":          "package b\n\n//go:build cgo\n",
		"b/x_darwin.go":      "package b\n",
		"b/x_linux_amd64.go": "package b\n",
		"b/x_test.go":        "package b\n",
		"b/darwin.go":        "package b\n", // a bare name is not a constraint
	}
	exists := func(p string) bool {
		_, ok := src[p]
		return ok || p == "go.mod" || p == "go.sum" || p == ".golangci.yml" ||
			strings.HasPrefix(p, "tools/check/") || strings.HasPrefix(p, "docs/")
	}
	read := func(p string) ([]byte, error) {
		s, ok := src[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(s), nil
	}
	// the content of deleted files, as `git show <base>:<path>` returns it
	deleted := map[string]string{
		"b/gone_cgo.go":   "//go:build cgo\n\npackage b\n",
		"b/gone_unix.go":  "//go:build unix\n\npackage b\n",
		"b/gone_os.go":    "//go:build windows\n\npackage b\n",
		"b/gone_plain.go": "package b\n",
	}
	readBase := func(p string) ([]byte, error) {
		s, ok := deleted[p]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(s), nil
	}
	cases := []struct {
		name string
		file string
		want bool
	}{
		{"deleted cgo file", "b/gone_cgo.go", true},
		{"deleted unix file", "b/gone_unix.go", true},
		{"deleted GOOS file", "b/gone_os.go", true},
		{"deleted plain file of an existing package", "b/gone_plain.go", false},
		{"file mapped to no package, not allow-listed", "scripts/x.sh", true},
		{"allow-listed docs", "docs/deep/x.txt", false},
		{"allow-listed markdown at any depth", "scripts/README.md", false},
		{"allow-listed .gitignore", ".gitignore", false},
		{"allow-listed agents", ".claude/agents/x.yaml", false},
		{"allow-listed LICENSE", "LICENSE-APACHE", false},
		{"go.mod", "go.mod", true},
		{"go.sum", "go.sum", true},
		{"golangci config", ".golangci.yml", true},
		{"tools/check source", "tools/check/main.go", true},
		{"tools/check test", "tools/check/affected_test.go", true},
		{"cgo build line", "b/cgo.go", true},
		{"negated cgo build line", "b/nocgo.go", true},
		{"GOOS build line", "b/os.go", true},
		{"GOARCH build line", "b/arch.go", true},
		{"unix build line", "b/unixy.go", true},
		{"GOOS file name", "b/x_darwin.go", true},
		{"GOOS and GOARCH file name", "b/x_linux_amd64.go", true},
		{"deleted package directory", "zz/gone.go", true},
		{"other build tag", "b/tagged.go", false},
		{"ignore build tag", "b/ignored.go", false},
		{"no build line", "b/plain.go", false},
		{"build line after the package clause", "b/late.go", false},
		{"test file name", "b/x_test.go", false},
		{"bare GOOS name", "b/darwin.go", false},
		{"markdown", "docs/testing.md", false},
		{"deleted testdata go file", "b/testdata/old.go", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, err := escalation([]string{c.file}, pkgs, exists, read, readBase)
			if err != nil {
				t.Fatal(err)
			}
			if (reason != "") != c.want {
				t.Errorf("escalation(%s) = %q, want escalate=%v", c.file, reason, c.want)
			}
		})
	}
	t.Run("read error is an error", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := escalation([]string{"b/cgo.go"}, pkgs, exists, func(string) ([]byte, error) { return nil, boom }, readBase)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want boom", err)
		}
	})
}

// fakeEnv builds an environment over the fake graph with a canned git.
func fakeEnv(t *testing.T, changed, untracked []string) (env, *int) {
	t.Helper()
	pkgs := fakeGraph(t)
	runs := new(int)
	nul := func(l []string) []byte {
		var b []byte
		for _, s := range l {
			b = append(b, s...)
			b = append(b, 0)
		}
		return b
	}
	return env{
		git: func(args ...string) ([]byte, error) {
			switch args[0] {
			case "diff":
				return nul(changed), nil
			case "ls-files":
				return nul(untracked), nil
			case "merge-base":
				return []byte("deadbeef\n"), nil
			}
			return nil, errors.New("unexpected git " + strings.Join(args, " "))
		},
		goList:      func() ([]pkg, error) { return pkgs, nil },
		exists:      func(string) bool { return true },
		read:        func(string) ([]byte, error) { return []byte("package x\n"), nil },
		testSources: func() ([]testSrc, error) { return nil, nil },
		runStep: func(step) error {
			*runs++
			return nil
		},
	}, runs
}

func TestNonPackageFilesAddOnlyTheGlobalBlock(t *testing.T) {
	e, _ := fakeEnv(t, []string{"docs/testing.md", "CLAUDE.md"}, []string{"LICENSE", ".gitignore", ".claude/agents/x.md"})
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.full {
		t.Fatalf("docs-only change escalated: %s", p.reason)
	}
	if len(p.affected) != 0 || len(p.blocks) != 0 {
		t.Errorf("affected %v blocks %v, want none", p.affected, p.blocks)
	}
	want := formatSteps(globalSteps())
	if got := formatSteps(p.steps); !slices.Equal(got, want) {
		t.Errorf("steps\n%s\nwant exactly the global block\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(p.steps) == 0 {
		t.Fatal("an affected run with nothing affected ran nothing")
	}
}

func TestAffectedPlanSteps(t *testing.T) {
	e, _ := fakeEnv(t, []string{"b/x.go"}, []string{"e/new.go"})
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.full {
		t.Fatalf("escalated: %s", p.reason)
	}
	if want := []string{"b", "c", "d", "e", "g/h"}; !slices.Equal(p.affected, want) {
		t.Errorf("affected %v, want %v", p.affected, want)
	}
	lines := formatSteps(p.steps)
	for _, w := range []string{
		"go vet ./b ./c ./d ./e ./g/h",
		"go tool golangci-lint run ./b ./c ./d ./e ./g/h",
		"go test -short -timeout 30m ./b ./c ./d ./e ./g/h",
	} {
		if !slices.Contains(lines, w) {
			t.Errorf("missing step %q in\n%s", w, strings.Join(lines, "\n"))
		}
	}
	for _, w := range formatSteps(globalSteps()) {
		if !slices.Contains(lines, w) {
			t.Errorf("missing global step %q", w)
		}
	}
	if len(p.tested) < 5 || !slices.Equal(p.tested[:5], []string{"b", "c", "d", "e", "g/h"}) {
		t.Errorf("tested %v does not start with the affected packages", p.tested)
	}
}

func TestDefaultBaseIsTheMergeBaseWithMain(t *testing.T) {
	e, _ := fakeEnv(t, nil, nil)
	var got [][]string
	inner := e.git
	e.git = func(args ...string) ([]byte, error) {
		got = append(got, args)
		return inner(args...)
	}
	if _, err := buildPlan(options{affected: true}, e); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || strings.Join(got[0], " ") != "merge-base HEAD main" {
		t.Fatalf("first git call %v, want merge-base HEAD main", got)
	}
	if !slices.ContainsFunc(got, func(a []string) bool { return a[0] == "diff" && slices.Contains(a, "deadbeef") }) {
		t.Errorf("diff was not taken against the merge base: %v", got)
	}
}

func TestGitFailureIsAnErrorNotAnEmptySet(t *testing.T) {
	boom := errors.New("git exploded")
	for _, failing := range []string{"diff", "ls-files", "merge-base"} {
		e, _ := fakeEnv(t, []string{"b/x.go"}, nil)
		inner := e.git
		e.git = func(args ...string) ([]byte, error) {
			if args[0] == failing {
				return nil, boom
			}
			return inner(args...)
		}
		opt := options{affected: true, base: "abc"}
		if failing == "merge-base" {
			opt.base = ""
		}
		if _, err := buildPlan(opt, e); !errors.Is(err, boom) {
			t.Errorf("git %s failing: err = %v, want the git error", failing, err)
		}
	}
}

func TestUnknownBlockIsAnError(t *testing.T) {
	e, _ := fakeEnv(t, nil, nil)
	_, err := buildPlan(options{blocks: []string{"nonesuch"}}, e)
	if err == nil {
		t.Fatal("unknown block accepted")
	}
	for _, b := range blocks {
		if !strings.Contains(err.Error(), b.name) {
			t.Errorf("error %q does not list the valid block %q", err, b.name)
		}
	}
}

func TestBlockPlanRunsBlockPackagesPlusGlobal(t *testing.T) {
	e, _ := fakeEnv(t, nil, nil)
	e.goList = func() ([]pkg, error) {
		return []pkg{{Dir: "internal/filesys"}, {Dir: "internal/filesys/ext4"}, {Dir: "internal/filesystem"}, {Dir: "internal/cli"}}, nil
	}
	p, err := buildPlan(options{blocks: []string{"filesys"}}, e)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/filesys", "internal/filesys/ext4"}; !slices.Equal(p.affected, want) {
		t.Errorf("block packages %v, want %v", p.affected, want)
	}
	lines := formatSteps(p.steps)
	if !slices.Contains(lines, "go test -short -timeout 30m ./internal/filesys ./internal/filesys/ext4") {
		t.Errorf("no test step for the block in\n%s", strings.Join(lines, "\n"))
	}
	for _, w := range formatSteps(globalSteps()) {
		if !slices.Contains(lines, w) {
			t.Errorf("missing global step %q", w)
		}
	}
}

func TestEveryPackageIsInExactlyOneBlock(t *testing.T) {
	t.Chdir("../..")
	pkgs, err := loadPackages()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) < 50 {
		t.Fatalf("only %d packages listed", len(pkgs))
	}
	for _, p := range pkgs {
		var in []string
		for _, b := range blocks {
			if b.contains(p.Dir) {
				in = append(in, b.name)
			}
		}
		if len(in) != 1 {
			t.Errorf("%s is in blocks %v, want exactly one", p.Dir, in)
		}
	}
	for _, b := range blocks {
		if !slices.ContainsFunc(pkgs, func(p pkg) bool { return b.contains(p.Dir) }) {
			t.Errorf("block %s holds no package", b.name)
		}
	}
}

// skipDir reports whether the walk must not descend into a directory.
func skipDir(root, path string, d fs.DirEntry) bool {
	n := d.Name()
	return path != root && (strings.HasPrefix(n, ".") || n == "testdata" || n == "docs" || n == "bin" || n == "cases")
}

// shortGuards finds every test whose first statement is the standard slow-test skip.
func shortGuards(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(root, path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil || len(fn.Body.List) == 0 {
				continue
			}
			if isSlowGuard(fn.Body.List[0]) {
				out[filepath.ToSlash(rel)+" "+fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func isSlowGuard(s ast.Stmt) bool {
	ifs, ok := s.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil || len(ifs.Body.List) != 1 {
		return false
	}
	call, ok := ifs.Cond.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Short" {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "testing" {
		return false
	}
	es, ok := ifs.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	skip, ok := es.X.(*ast.CallExpr)
	if !ok || len(skip.Args) != 1 {
		return false
	}
	ssel, ok := skip.Fun.(*ast.SelectorExpr)
	if !ok || ssel.Sel.Name != "Skip" {
		return false
	}
	lit, ok := skip.Args[0].(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `"slow (> 60 s): runs in the merge gate"`
}

func TestShortTableMatchesGuards(t *testing.T) {
	root := "../.."
	md, err := os.ReadFile(filepath.Join(root, "docs", "testing.md"))
	if err != nil {
		t.Fatal(err)
	}
	table, err := slowTable(string(md))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range slowTableProblems(shortGuards(t, root), table) {
		t.Error(p)
	}
}

func TestEveryModeHasAnExplicitTimeout(t *testing.T) {
	t.Chdir("../..")
	e, _ := fakeEnv(t, []string{"b/x.go"}, nil)
	plans := map[string][]step{
		"full fast": fullSteps(false),
		"full race": fullSteps(true),
	}
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	plans["affected"] = p.steps
	e.goList = func() ([]pkg, error) { return []pkg{{Dir: "internal/filesys"}}, nil }
	if p, err = buildPlan(options{blocks: []string{"filesys"}}, e); err != nil {
		t.Fatal(err)
	}
	plans["block"] = p.steps
	for name, steps := range plans {
		n := 0
		for i, s := range steps {
			args := s.args
			if s.prepare != nil {
				var err error
				if args, err = s.prepare(); err != nil {
					t.Fatalf("%s step %d: %v", name, i, err)
				}
			}
			if len(args) < 2 || args[0] != "go" || args[1] != "test" {
				continue
			}
			n++
			j := slices.Index(args, "-timeout")
			if j < 0 || j+1 >= len(args) || args[j+1] != "30m" {
				t.Errorf("%s: test step without -timeout 30m: %v", name, args)
			}
		}
		if n == 0 {
			t.Errorf("%s has no go test step", name)
		}
	}
}

func TestShortOnlyInPerTaskForms(t *testing.T) {
	for name, steps := range map[string][]step{"full fast": fullSteps(false), "full race": fullSteps(true)} {
		for _, s := range steps {
			if slices.Contains(s.args, "-short") {
				t.Errorf("%s passes -short: %v", name, s.args)
			}
		}
	}
}

func TestListPrintsWithoutRunning(t *testing.T) {
	e, runs := fakeEnv(t, []string{"b/x.go", "docs/testing.md"}, nil)
	var out, errb bytes.Buffer
	code := run([]string{"-affected", "-base", "abc", "-list"}, &out, &errb, e)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if *runs != 0 {
		t.Errorf("-list ran %d steps", *runs)
	}
	for _, w := range []string{"b/x.go", "docs/testing.md", "go vet ./b ./c ./d", "go build ./..."} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("-list output lacks %q:\n%s", w, out.String())
		}
	}
	if strings.Contains(out.String(), "CHECK PASSED") {
		t.Error("-list claims the check passed")
	}
}

func TestRunPrintsSummaryAndPasses(t *testing.T) {
	e, runs := fakeEnv(t, []string{"b/x.go"}, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"-affected", "-base", "abc"}, &out, &errb, e); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if *runs == 0 {
		t.Error("nothing ran")
	}
	if !strings.Contains(out.String(), "tested packages: ") || !strings.Contains(out.String(), "CHECK PASSED") {
		t.Errorf("summary or pass line missing:\n%s", out.String())
	}
}

func TestFailingStepFailsTheRun(t *testing.T) {
	e, _ := fakeEnv(t, []string{"b/x.go"}, nil)
	e.runStep = func(step) error { return errors.New("step failed") }
	var out, errb bytes.Buffer
	if code := run([]string{"-affected", "-base", "abc"}, &out, &errb, e); code == 0 {
		t.Fatal("a failing step passed the run")
	}
	if strings.Contains(out.String(), "CHECK PASSED") {
		t.Error("CHECK PASSED printed after a failure")
	}
}

func TestEscalatedRunUsesTheFullForm(t *testing.T) {
	e, _ := fakeEnv(t, []string{"go.mod"}, nil)
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if !p.full || p.reason == "" {
		t.Fatalf("go.mod change did not escalate: %+v", p)
	}
	if !slices.Contains(formatSteps(p.steps), "go vet ./...") {
		t.Errorf("escalated plan is not the full form:\n%s", strings.Join(formatSteps(p.steps), "\n"))
	}
}

func TestSlowTableParsesRows(t *testing.T) {
	md := "## x\n### Slow tests skipped under -short\ntext\n\n| Test | Package | Seconds |\n|---|---|---|\n| `TestA` | `internal/p` | 75 |\n| `TestB` | `internal/q` | 120 |\n\n## next\n| `TestC` | `z` | 99 |\n"
	got, err := slowTable(md)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"internal/p TestA": 75, "internal/q TestB": 120}
	if len(got) != 2 || got["internal/p TestA"] != 75 || got["internal/q TestB"] != 120 {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := slowTable("# none"); err == nil {
		t.Error("a missing heading is not an error")
	}
}
