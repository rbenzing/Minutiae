package evidence_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// carveSpec is a carved artifact of the given scope over 64 bytes of the parent.
func carveSpec(scope string, a evidence.AllocSummary) evidencetest.RecoveredSpec {
	dir := map[string]string{"raw": "raw", "volume": "volume", "artifact": "raw", "unallocated": "p1-mtfs"}[scope]
	return evidencetest.RecoveredSpec{
		Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-validated", Confidence: ip(70), Scope: scope,
		Path: "carved/" + dir + "/0000000000000000-png.png", Runs: []evidence.Run{{Offset: 0, Length: 64}}, Alloc: &a,
	}
}

func TestVerifyCarvedVolumeScopeMustBeFree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alloc evidence.AllocSummary
		want  string
	}{
		{"free", evidence.AllocSummary{Free: 64}, ""},
		{"allocated", evidence.AllocSummary{Free: 32, Allocated: 32}, "scope volume but 32 allocated and 0 unknown bytes (class \"carved\")"},
		{"unknown", evidence.AllocSummary{Free: 32, Unknown: 32}, "scope volume but 0 allocated and 32 unknown bytes (class \"carved\")"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, carveSpec("volume", tc.alloc))
			rep := verifyOf(t, c)
			if tc.want == "" {
				evidencetest.RequireNoProblem(t, rep, "scope volume")
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

func TestVerifyCarvedArtifactScopeHasNoAllocState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alloc evidence.AllocSummary
		want  string
	}{
		{"unknown", evidence.AllocSummary{Unknown: 64}, ""},
		{"free", evidence.AllocSummary{Free: 32, Unknown: 32}, "scope artifact but 32 free and 0 allocated bytes (an artifact has no allocation state)"},
		{"allocated", evidence.AllocSummary{Allocated: 64}, "scope artifact but 0 free and 64 allocated bytes (an artifact has no allocation state)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, carveSpec("artifact", tc.alloc))
			rep := verifyOf(t, c)
			if tc.want == "" {
				evidencetest.RequireNoProblem(t, rep, "scope artifact")
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

func carveNote() *evidence.AcquisitionGapNote {
	return &evidence.AcquisitionGapNote{Bytes: 32, Ranges: 1, First: []evidence.Run{{Offset: 0, Length: 32}}}
}

func addCarveWithNote(t *testing.T, scope string, alloc evidence.AllocSummary) evidence.VerifyReport {
	t.Helper()
	c, parent, data := recoverParent(t)
	rec := evidencetest.AddRecovered(t, c, parent, data, carveSpec(scope, alloc))
	evidencetest.RewriteManifest(t, c.Dir, rec.ID, func(r *evidence.ManifestRecord) { r.Source.Derived.AcquisitionGaps = carveNote() })
	return verifyOf(t, c)
}

func TestVerifyCarveArtifactScopeRefusesGapNote(t *testing.T) {
	rep := addCarveWithNote(t, "artifact", evidence.AllocSummary{Unknown: 64})
	evidencetest.RequireProblem(t, rep, "acquisition gap note on a carve of scope artifact (its parent is not a container)")
}

func TestVerifyCarveMayCarryGapNote(t *testing.T) {
	for _, scope := range []string{"unallocated", "volume", "raw"} {
		rep := addCarveWithNote(t, scope, evidence.AllocSummary{Free: 64})
		evidencetest.RequireNoProblem(t, rep, "acquisition gap note")
	}
}

func TestVerifyNonCarveGapNoteTextNamesCarve(t *testing.T) {
	c, parent, data := recoverParent(t)
	rec := evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 64}}})
	evidencetest.RewriteManifest(t, c.Dir, rec.ID, func(r *evidence.ManifestRecord) {
		r.Source.Kind = "runs"
		r.Source.Derived.AcquisitionGaps = carveNote()
	})
	rep := verifyOf(t, c)
	evidencetest.RequireProblem(t, rep, "(only extract, unallocated and carve artifacts take one)")
}
