package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// calendarDirs are the directories of the calendar code: the payload package and,
// as their plans add them, the calendar parsers. A stored zone name (Android
// eventTimezone, iOS start_tz) is stated as stored and never applied: applying it
// needs the host's zone database, and a zone's rules at the event's date are not
// the rules of today (plan 4I3, spec 5.1). Each listed directory must exist.
var calendarDirs = []string{
	"internal/recordtypes/calendar",
}

// zoneFuncs are the functions of package time that make or load a zone.
var zoneFuncs = map[string]bool{"FixedZone": true, "LoadLocation": true, "LoadLocationFromTZData": true}

// calendarZoneProblems reports every use of a zone in a source: a call or value of
// time.FixedZone, time.LoadLocation or time.LoadLocationFromTZData (under any
// import name, and a dot import of time, which would hide them), and any selector
// named In (a call, a method value or a method expression such as time.Time.In;
// types are not resolved, so a field of that name is flagged too).
func calendarZoneProblems(name, src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return []string{name + ": " + err.Error()}
	}
	var out []string
	at := func(p token.Pos, what string) { out = append(out, fset.Position(p).String()+": "+what) }
	timeNames := map[string]bool{}
	for _, spec := range f.Imports {
		if importPath(spec) != "time" {
			continue
		}
		switch n := localName(spec); n {
		case ".":
			at(spec.Pos(), "dot import of time hides its zone functions")
		case "_":
		default:
			timeNames[n] = true
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "In" {
			at(sel.Sel.Pos(), "a selector named In converts a time into a zone")
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && timeNames[id.Name] && zoneFuncs[sel.Sel.Name] {
			at(sel.Pos(), "time."+sel.Sel.Name+" makes or loads a zone")
		}
		return true
	})
	return out
}

// TestCalendarZoneNameIsNeverApplied: no non-test source of the calendar code
// makes, loads or applies a zone.
func TestCalendarZoneNameIsNeverApplied(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range calendarDirs {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("%s: %v (a listed calendar directory must exist)", rel, err)
			continue
		}
		seen := 0
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			seen++
			for _, p := range calendarZoneProblems(rel+"/"+e.Name(), string(b)) {
				t.Error(p)
			}
		}
		if seen == 0 {
			t.Errorf("%s holds no non-test Go file", rel)
		}
	}
}

// TestCalendarZoneRuleSelfTest: the scan is not vacuous: each form is caught, and
// UTC readings are not.
func TestCalendarZoneRuleSelfTest(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"FixedZone call":              {"import \"time\"\n\nvar z = time.FixedZone(\"x\", 3600)\n", 1},
		"FixedZone value":             {"import \"time\"\n\nvar z = time.FixedZone\n", 1},
		"LoadLocation call":           {"import \"time\"\n\nvar z, _ = time.LoadLocation(\"Europe/Berlin\")\n", 1},
		"LoadLocation value":          {"import \"time\"\n\nvar z = time.LoadLocation\n", 1},
		"LoadLocationFromTZData":      {"import \"time\"\n\nvar z, _ = time.LoadLocationFromTZData(\"x\", nil)\n", 1},
		"aliased FixedZone":           {"import tm \"time\"\n\nvar z = tm.FixedZone(\"x\", 0)\n", 1},
		"dot import":                  {"import . \"time\"\n\nvar z = FixedZone(\"x\", 0)\n", 1},
		"In call":                     {"import \"time\"\n\nfunc f(t time.Time) time.Time { return t.In(time.UTC) }\n", 1},
		"In method value":             {"import \"time\"\n\nfunc f(t time.Time) { g := t.In; _ = g }\n", 1},
		"In method expression":        {"import \"time\"\n\nvar f = time.Time.In\n", 1},
		"In on any expression":        {"func f(x interface{ In(int) int }) int { return x.In(1) }\n", 1},
		"two uses":                    {"import \"time\"\n\nvar a = time.FixedZone(\"x\", 0)\nvar b = time.Time{}.In(a)\n", 2},
		"UTC readings are fine":       {"import \"time\"\n\nfunc f(t time.Time) string { return t.UTC().Format(time.DateOnly) }\n", 0},
		"time.Date in UTC is fine":    {"import \"time\"\n\nvar d = time.Date(2023, 11, 15, 0, 0, 0, 0, time.UTC)\n", 0},
		"another package's FixedZone": {"import \"other\"\n\nvar z = other.FixedZone\n", 0},
	} {
		if got := calendarZoneProblems("x.go", "package x\n\n"+tc.src); len(got) != tc.want {
			t.Errorf("%s: %d problems %v, want %d", name, len(got), got, tc.want)
		}
	}
}
