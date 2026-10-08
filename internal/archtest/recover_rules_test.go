package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file holds the rules of plan 3A (deleted-data recovery foundation). Every
// rule is a scanner over a parsed Go file and has a self-test over a
// testdata/*.go.txt snippet in which each line that must be flagged carries the
// marker "// want": a scanner that silently stops finding anything fails its
// self-test.

// The evidence.Source kinds only internal/evidence and internal/examine may
// write: the identifiers and their string values.
var (
	recoveredKindIdents = map[string]bool{"KindRecover": true, "KindCarve": true, "KindSlack": true, "KindJournal": true, "KindReport": true}
	recoveredKindValues = map[string]bool{"recover": true, "carve": true, "slack": true, "journal": true, "report": true}
)

// isRecoveredKindExpr reports whether e names one of the recovered kinds: a
// KindX identifier (bare or qualified) or a string literal with its value.
func isRecoveredKindExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return recoveredKindIdents[v.Name]
	case *ast.SelectorExpr:
		return recoveredKindIdents[v.Sel.Name]
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return false
		}
		s, err := strconv.Unquote(v.Value)
		return err == nil && recoveredKindValues[s]
	case *ast.ParenExpr:
		return isRecoveredKindExpr(v.X)
	}
	return false
}

// scanRecoveredKinds returns the lines of src that set a field named Kind (a
// composite literal key or an assignment target) to a recovered kind. A map key
// that merely is the string "recover" is not a Kind field and is not flagged.
func scanRecoveredKinds(name string, src []byte) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.KeyValueExpr:
			if id, ok := v.Key.(*ast.Ident); ok && id.Name == "Kind" && isRecoveredKindExpr(v.Value) {
				lines = append(lines, fset.Position(v.Pos()).Line)
			}
		case *ast.AssignStmt:
			for i, l := range v.Lhs {
				sel, ok := l.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Kind" {
					continue
				}
				if len(v.Rhs) == len(v.Lhs) && isRecoveredKindExpr(v.Rhs[i]) {
					lines = append(lines, fset.Position(v.Pos()).Line)
				}
			}
		}
		return true
	})
	return lines, nil
}

// wantLines returns the 1-based numbers of the lines of a testdata snippet that
// carry the marker "// want".
func wantLines(src []byte) []int {
	var out []int
	for i, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "// want") {
			out = append(out, i+1)
		}
	}
	return out
}

func sameInts(a, b []int) bool {
	a = append([]int(nil), a...)
	b = append([]int(nil), b...)
	sort.Ints(a)
	sort.Ints(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// nonTestGoFiles lists the non-test .go files under dir (relative to root),
// skipping testdata.
func nonTestGoFiles(t *testing.T, root, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRecoveredKindsOnlyWrittenByEvidenceAndExamine: a recovered kind in a
// Source is the claim "this artifact was recovered"; only the code that
// creates such artifacts (evidence validates them, examine writes them, and the
// test-only fixtures) may make it. Anyone else building one would forge
// provenance.
func TestRecoveredKindsOnlyWrittenByEvidenceAndExamine(t *testing.T) {
	root := repoRoot(t)
	files := nonTestGoFiles(t, root, "internal")
	scanned := 0
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		skip := false
		// evidencetest, recordstest and fstest are test-only packages (never
		// linked into the binary, see TestTestOnlyPackagesAreImportedFromTestsOnly).
		for _, ok := range []string{"internal/evidence/", "internal/examine/", "internal/records/recordstest/", "internal/filesys/fstest/"} {
			if strings.HasPrefix(rel, ok) {
				skip = true
			}
		}
		if skip {
			continue
		}
		scanned++
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := scanRecoveredKinds(p, src)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			t.Errorf("%s:%d sets a recovered Source kind; only internal/evidence and internal/examine may", rel, l)
		}
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files: the walk is broken", scanned)
	}
}

func TestRecoveredKindsScannerSelfTest(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "kinds.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanRecoveredKinds("kinds.go.txt", src)
	if err != nil {
		t.Fatal(err)
	}
	want := wantLines(src)
	if len(want) < 10 {
		t.Fatalf("self-test data holds only %d violating lines", len(want))
	}
	if !sameInts(got, want) {
		t.Errorf("flagged lines %v, want %v", got, want)
	}
}

// examineAllowedOS are the only identifiers internal/examine may select from
// package os: reading the source or probing paths, never writing.
var examineAllowedOS = map[string]bool{
	"File": true, "Open": true, "Stat": true, "SameFile": true, "FileInfo": true,
	"ErrNotExist": true, "IsNotExist": true,
}

// scanOSWrites returns the lines of src that use package os beyond the
// read-only allow-list, or import io/ioutil.
func scanOSWrites(name string, src []byte) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	osName := ""
	var lines []int
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		switch p {
		case "io/ioutil":
			lines = append(lines, fset.Position(imp.Pos()).Line)
		case "os":
			osName = "os"
			if imp.Name != nil {
				osName = imp.Name.Name
			}
		}
	}
	if osName == "" || osName == "_" {
		return lines, nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == osName && !examineAllowedOS[sel.Sel.Name] {
			lines = append(lines, fset.Position(sel.Pos()).Line)
		}
		return true
	})
	return lines, nil
}

// TestRecoveredBytesReachDiskOnlyViaNewArtifact is the spec invariant: examine
// is the code that copies bytes out of an image, so it has no way to put them
// on disk except through the evidence case (Case.NewArtifact/Capture). Package
// os is allowed there for reading only.
func TestRecoveredBytesReachDiskOnlyViaNewArtifact(t *testing.T) {
	root := repoRoot(t)
	files := nonTestGoFiles(t, root, filepath.Join("internal", "examine"))
	if len(files) < 5 {
		t.Fatalf("scanned only %d examine files: the walk is broken", len(files))
	}
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := scanOSWrites(p, src)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		for _, l := range lines {
			t.Errorf("%s:%d uses a write-capable os identifier (or io/ioutil); bytes reach disk only via evidence.Case.NewArtifact/Capture", filepath.ToSlash(rel), l)
		}
	}
}

func TestOSWriteScannerSelfTest(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "oswrite.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanOSWrites("oswrite.go.txt", src)
	if err != nil {
		t.Fatal(err)
	}
	want := wantLines(src)
	if len(want) < 10 {
		t.Fatalf("self-test data holds only %d violating lines", len(want))
	}
	if !sameInts(got, want) {
		t.Errorf("flagged lines %v, want %v", got, want)
	}
}

// tamperTestFiles are the tamper-test files of the recovery checks; each must
// exist, so renaming one does not silently drop it from the rule.
var tamperTestFiles = []string{
	"internal/evidence/verify_recovery_test.go",
	"internal/records/verify_recovered_test.go",
	"internal/examine/verify_recovered_test.go",
	"internal/cli/case_verify_recovered_test.go",
}

var problemAssertions = map[string]bool{"RequireProblem": true, "RequireProblems": true, "requireProblemLine": true}

// isOKFalseExpr reports whether e is `!x.OK()` or `x.OK() == false`.
func isOKFalseExpr(e ast.Expr) bool {
	isOKCall := func(x ast.Expr) bool {
		c, ok := x.(*ast.CallExpr)
		if !ok {
			return false
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		return ok && s.Sel.Name == "OK" && len(c.Args) == 0
	}
	isFalse := func(x ast.Expr) bool { id, ok := x.(*ast.Ident); return ok && id.Name == "false" }
	switch v := e.(type) {
	case *ast.UnaryExpr:
		return v.Op == token.NOT && isOKCall(v.X)
	case *ast.BinaryExpr:
		return v.Op == token.EQL && ((isOKCall(v.X) && isFalse(v.Y)) || (isOKCall(v.Y) && isFalse(v.X)))
	}
	return false
}

// scanOKAlone returns the lines, in the functions of src that assert a report is
// not OK, where no specific problem is asserted in the same function.
func scanOKAlone(name string, src []byte) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var lines []int
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		var okFalse []int
		specific := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok && isOKFalseExpr(e) {
				okFalse = append(okFalse, fset.Position(e.Pos()).Line)
			}
			if c, ok := n.(*ast.CallExpr); ok {
				switch fn := c.Fun.(type) {
				case *ast.Ident:
					specific = specific || problemAssertions[fn.Name]
				case *ast.SelectorExpr:
					specific = specific || problemAssertions[fn.Sel.Name]
				}
			}
			return true
		})
		if len(okFalse) > 0 && !specific {
			lines = append(lines, okFalse...)
		}
	}
	return lines, nil
}

// TestTamperTestsNeverCheckOKAlone: a tamper test that only proves "the report
// is not OK" passes for any unrelated problem; it must name its specific one.
func TestTamperTestsNeverCheckOKAlone(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range tamperTestFiles {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s must exist (the rule lists it by name): %v", rel, err)
			continue
		}
		lines, err := scanOKAlone(rel, src)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			t.Errorf("%s:%d asserts the report is not OK without RequireProblem/RequireProblems/requireProblemLine in the same function", rel, l)
		}
	}
}

func TestOKAloneScannerSelfTest(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "okalone.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanOKAlone("okalone.go.txt", src)
	if err != nil {
		t.Fatal(err)
	}
	want := wantLines(src)
	if len(want) < 4 {
		t.Fatalf("self-test data holds only %d violating lines", len(want))
	}
	if !sameInts(got, want) {
		t.Errorf("flagged lines %v, want %v", got, want)
	}
}

// TestRecoveryDependencyEntries pins the entries the recovery work relies on:
// examine still lists no internal/records, and evidencetest imports only
// evidence (it is the fixture of evidence's own tests).
func TestRecoveryDependencyEntries(t *testing.T) {
	ex, ok := allowed["internal/examine"]
	if !ok {
		t.Fatal("internal/examine is not listed")
	}
	for _, imp := range ex {
		if strings.HasPrefix(imp, "internal/records") {
			t.Errorf("internal/examine lists %s", imp)
		}
	}
	got, ok := allowed["internal/evidence/evidencetest"]
	if !ok || len(got) != 1 || got[0] != "internal/evidence" {
		t.Errorf("evidencetest must list exactly internal/evidence, has %v", got)
	}
}
