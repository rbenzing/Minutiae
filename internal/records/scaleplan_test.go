package records_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The scale harness (scale_test.go, build tag scale) must never measure or mutate a case it should not:
// its phase rules are plain functions so they are pinned without a 10 million record case.

func phaseSet(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestScalePhasePlanRules(t *testing.T) {
	cases := []struct {
		name     string
		phases   map[string]bool
		pristine bool
		wantErr  bool
		copyOnly bool
		mutating bool
	}{
		{"copy alone", phaseSet("copy"), false, false, true, false},
		{"copy with queries", phaseSet("copy", "queries"), false, true, false, false},
		{"copy with a mutating phase", phaseSet("copy", "supersede", "tamper"), false, true, false, false},
		{"copy with ingest", phaseSet("copy", "ingest"), false, true, false, false},
		{"mutating on a pristine copy", phaseSet("supersede", "superqueries", "tamper"), true, false, false, true},
		{"tamper alone on a pristine copy", phaseSet("tamper"), true, false, false, true},
		{"mutating without the marker", phaseSet("supersede", "superqueries", "tamper"), false, true, false, false},
		{"mutating with ingest", phaseSet("ingest", "supersede"), true, true, false, false},
		{"reindex mutates the copy", phaseSet("reindex"), true, false, false, true},
		{"reindex without the marker", phaseSet("reindex"), false, true, false, false},
		{"reindex with ingest", phaseSet("ingest", "reindex"), true, true, false, false},
		{"clean phases need no marker", phaseSet("queries", "search", "stats", "untouched", "verify"), false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := planScalePhases(tc.phases, tc.pristine)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error %v, want error %v", err, tc.wantErr)
			}
			if err == nil && (p.copyOnly != tc.copyOnly || p.mutating != tc.mutating) {
				t.Fatalf("plan %+v, want copyOnly %v mutating %v", p, tc.copyOnly, tc.mutating)
			}
		})
	}
}

func TestPristineMarkerIsWrittenOnceAndConsumedOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mut")
	if pristineMarkerExists(root) {
		t.Fatal("a marker exists before any copy")
	}
	if err := consumePristineMarker(root); err == nil {
		t.Fatal("consuming a marker that was never written succeeded")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePristineMarker(root); err != nil {
		t.Fatal(err)
	}
	if !pristineMarkerExists(root) {
		t.Fatal("the marker is not there after it was written")
	}
	if err := consumePristineMarker(root); err != nil {
		t.Fatal(err)
	}
	if pristineMarkerExists(root) {
		t.Fatal("the marker survived its consumption")
	}
	if err := consumePristineMarker(root); err == nil {
		t.Fatal("a marker was consumed twice")
	}
}

// scalePlan is what a phase list asks of one TestScale run.
type scalePlan struct{ copyOnly, mutating bool }

// planScalePhases decides what a run may do. The copy phase is a run of its own (ruling D43): with
// any other phase it is refused, never silently turned into a copy-only run. A mutating phase
// (supersede, superqueries, tamper, reindex) needs the pristine marker the copy left (ruling D44), and never
// shares a run with ingest.
func planScalePhases(phases map[string]bool, pristine bool) (scalePlan, error) {
	mutating := phases["supersede"] || phases["superqueries"] || phases["tamper"] || phases["reindex"]
	switch {
	case phases["copy"] && len(phases) > 1:
		return scalePlan{}, errors.New("the copy phase is a run of its own: run it with no other phase")
	case mutating && phases["ingest"]:
		return scalePlan{}, errors.New("ingest and the mutating phases (supersede, superqueries, tamper, reindex) never share a run")
	case mutating && !pristine:
		return scalePlan{}, errors.New("a mutating phase (supersede, superqueries, tamper, reindex) needs a pristine copy: run the copy phase first; a copy is mutated by one run only")
	}
	return scalePlan{copyOnly: phases["copy"], mutating: mutating}, nil
}

// The marker sits beside the copy (in the mut root, not inside the case), is written when the copy
// is made and consumed when the first mutating run starts: a second mutating run, or a run on a copy
// somebody else made, is refused (the clean measurements and the mutations never share a case).
const pristineMarkerName = "pristine.marker"

func pristineMarkerExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, pristineMarkerName)) //nolint:gosec // a scale case path chosen by the operator
	return err == nil
}

func writePristineMarker(root string) error {
	return os.WriteFile(filepath.Join(root, pristineMarkerName), []byte("a copy of the clean case no run has mutated\n"), 0o600) //nolint:gosec // a scale case path chosen by the operator
}

func consumePristineMarker(root string) error {
	return os.Remove(filepath.Join(root, pristineMarkerName)) //nolint:gosec // a scale case path chosen by the operator
}
