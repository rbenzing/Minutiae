package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

const wholeModuleTests = "go test -short -timeout 30m ./..."

func TestNonGoChangeRunsCachedWholeModuleTests(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		scoped  bool // vet and lint of the affected packages still run
	}{
		{"testdata", []string{"b/testdata/x.bin"}, true},
		{"docs markdown", []string{"docs/testing.md"}, false},
		{"fixture under a non-package directory", []string{"scripts/fixtures/x.json"}, false},
		{"go and non-go together", []string{"b/x.go", "docs/a.md"}, true},
		{"go file under testdata", []string{"b/testdata/gen.go"}, true},
		{"go file mapped to no package", []string{"scripts/tool.go"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := fakeEnv(t, c.changed, nil)
			p, err := buildPlan(options{affected: true, base: "abc"}, e)
			if err != nil {
				t.Fatal(err)
			}
			if p.full {
				t.Fatalf("escalated: %s", p.reason)
			}
			lines := formatSteps(p.steps)
			if !slices.Contains(lines, wholeModuleTests) {
				t.Errorf("no cached whole-module test step in\n%s", strings.Join(lines, "\n"))
			}
			for _, l := range lines {
				if strings.HasPrefix(l, "go test -short -timeout 30m ./b") {
					t.Errorf("a scoped test step runs next to the whole-module one: %s", l)
				}
				if strings.Contains(l, "-count=1") {
					t.Errorf("the test cache is disabled: %s", l)
				}
			}
			if hasVet := slices.ContainsFunc(lines, func(l string) bool {
				return strings.HasPrefix(l, "go vet ") && l != scaleVetLine && l != realdataVetLine
			}); hasVet != c.scoped {
				t.Errorf("vet step present = %v, want %v", hasVet, c.scoped)
			}
			if !strings.Contains(p.testForm, "non-Go") {
				t.Errorf("test form %q does not say why", p.testForm)
			}
		})
	}
}

func TestGoOnlyChangeRunsReverseClosure(t *testing.T) {
	e, _ := fakeEnv(t, []string{"b/x.go"}, []string{"e/new.go"})
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	lines := formatSteps(p.steps)
	if slices.Contains(lines, wholeModuleTests) {
		t.Error("a Go-only change runs the whole module")
	}
	if !slices.Contains(lines, "go test -short -timeout 30m ./b ./c ./d ./e ./g/h") {
		t.Errorf("no reverse-closure test step in\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(p.testForm, "reverse closure") {
		t.Errorf("test form %q", p.testForm)
	}
}

func TestListStatesTheTestFormAndUnmappedFiles(t *testing.T) {
	e, _ := fakeEnv(t, []string{"scripts/run.sh"}, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"-affected", "-base", "abc", "-list"}, &out, &errb, e); code != 0 {
		t.Fatalf("exit %d %s", code, errb.String())
	}
	for _, w := range []string{
		"tests: non-Go file changed: cached ./... test step",
		"changed files mapped to no package:\n  scripts/run.sh\n",
	} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("-list output lacks %q:\n%s", w, out.String())
		}
	}
}

func TestDocConsistencyPackagesAreListed(t *testing.T) {
	root := "../.."
	var dirs []string
	for _, d := range docConsistencyPackages {
		dirs = append(dirs, d.dir)
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(d.dir))); err != nil {
			t.Errorf("listed doc-consistency package %s: %v", d.dir, err)
		}
		if d.why == "" {
			t.Errorf("%s has no reason", d.dir)
		}
	}
	if !slices.Equal(dirs, []string{"internal/archtest", "tools/check"}) {
		t.Errorf("the global doc block is archtest and tools/check only, got %v", dirs)
	}
}

// scaleVetLine is the step that keeps the -tags scale harness compiling (docs/testing.md, ruling D65).
// It was lost once in a merge; every plan must carry it.
const scaleVetLine = "go vet -tags scale ./internal/records/... ./internal/evidence/... ./internal/artparse/..."

func TestEveryPlanVetsTheScaleTag(t *testing.T) {
	if !slices.Contains(formatSteps(fullSteps(false)), scaleVetLine) {
		t.Errorf("the full plan has no %q", scaleVetLine)
	}
	e, _ := fakeEnv(t, []string{"b/x.go"}, nil)
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.full {
		t.Fatalf("a Go-only change escalated: %s", p.reason)
	}
	if !slices.Contains(formatSteps(p.steps), scaleVetLine) {
		t.Errorf("the -affected plan has no %q:\n%s", scaleVetLine, strings.Join(formatSteps(p.steps), "\n"))
	}
	pb, err := buildPlan(options{blocks: []string{"case"}}, e)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(formatSteps(pb.steps), scaleVetLine) {
		t.Errorf("the -block plan has no %q", scaleVetLine)
	}
}

// realdataVetLine keeps the -tags realdata harness (internal/parsers and internal/cli) compiling (4H-R13): the
// default build never sees it and a real-data run is an operator step, so a break would otherwise go unnoticed.
const realdataVetLine = "go vet -tags realdata ./internal/parsers/... ./internal/cli/..."

func TestEveryPlanVetsTheRealDataTag(t *testing.T) {
	if !slices.Contains(formatSteps(fullSteps(false)), realdataVetLine) {
		t.Errorf("the full plan has no %q", realdataVetLine)
	}
	e, _ := fakeEnv(t, []string{"b/x.go"}, nil)
	p, err := buildPlan(options{affected: true, base: "abc"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.full {
		t.Fatalf("a Go-only change escalated: %s", p.reason)
	}
	if !slices.Contains(formatSteps(p.steps), realdataVetLine) {
		t.Errorf("the -affected plan has no %q:\n%s", realdataVetLine, strings.Join(formatSteps(p.steps), "\n"))
	}
	pb, err := buildPlan(options{blocks: []string{"case"}}, e)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(formatSteps(pb.steps), realdataVetLine) {
		t.Errorf("the -block plan has no %q", realdataVetLine)
	}
}
