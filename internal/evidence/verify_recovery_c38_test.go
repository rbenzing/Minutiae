package evidence_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// C38: every kind whose bytes are copied from an image must record runs unless it is empty; only the
// derived journal-report is exempt. This is enforced by case verify, not only by CheckRecoveredRuns.
func TestVerifyRecoveredKindsWithoutRunsAreProblems(t *testing.T) {
	const noRuns = "records no runs"
	ten := []byte("0123456789")
	specs := map[string]struct {
		spec     evidencetest.RecoveredSpec
		wantProb bool
	}{
		"slack": {evidencetest.RecoveredSpec{
			Kind: evidence.KindSlack, Class: evidence.ClassSlack, Method: "slack-file", Path: "slack/s1.bin",
			Data: ten, Alloc: &evidence.AllocSummary{Free: 10},
		}, true},
		"journal-block": {evidencetest.RecoveredSpec{
			Kind: evidence.KindJournal, Class: evidence.ClassJournalBlock, Method: "ext4-journal", Path: "journal/j1.bin",
			Data: ten, Alloc: &evidence.AllocSummary{Free: 10},
		}, true},
		"carve": {evidencetest.RecoveredSpec{
			Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-signature", Scope: "raw", Path: "carved/c1.bin",
			Data: ten, Alloc: &evidence.AllocSummary{Free: 10},
		}, true},
		"recover": {evidencetest.RecoveredSpec{Data: ten, Alloc: &evidence.AllocSummary{Free: 10}}, true},
		"journal-report is exempt": {evidencetest.RecoveredSpec{
			Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: "ext4-journal", Path: "reports/r1.json",
			Data: ten, Alloc: &evidence.AllocSummary{},
		}, false},
		"empty slack": {evidencetest.RecoveredSpec{
			Kind: evidence.KindSlack, Class: evidence.ClassSlack, Method: "slack-file", Path: "slack/s2.bin",
			Data: []byte{}, Alloc: &evidence.AllocSummary{},
		}, false},
	}
	for name, tc := range specs {
		t.Run(name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, tc.spec)
			rep := verifyOf(t, c)
			if tc.wantProb {
				evidencetest.RequireProblem(t, rep, noRuns)
			} else {
				evidencetest.RequireNoProblem(t, rep, noRuns)
			}
		})
	}
}

// C39: a recover artifact at a path with only five components (accepted by RecoveredNamespace) is
// reported for its missing ordinal; verify never indexes past the path.
func TestVerifyRecoverFivePartPathIsReportedNotPanicked(t *testing.T) {
	c, parent, data := recoverParent(t)
	rec := evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Path: "recovered/x", Runs: []evidence.Run{{Offset: 0, Length: 64}},
	})
	if n := len(strings.Split(rec.Path, "/")); n != 5 {
		t.Fatalf("test rig: path %q has %d components, want 5", rec.Path, n)
	}
	rep := verifyOf(t, c)
	evidencetest.RequireProblem(t, rep, "file name does not start with an ordinal")
}

// A parent that is not one plain raw image file never bounds the runs by its file size: a derived
// artifact, a multi-segment import, a file of another kind, an unreadable file. (An E01 segment is
// covered by TestVerifyRecoveredRunBoundIsSkippedForNonRawParents.)
func TestVerifyRecoveredRunBoundNeedsASingleRawParent(t *testing.T) {
	const beyond = "lies beyond the parent's"
	data := bytes.Repeat([]byte{9}, 16384)
	for name, tc := range map[string]struct {
		source func(parent evidence.ManifestRecord) evidence.Source
		remove bool
	}{
		"not an import": {source: func(evidence.ManifestRecord) evidence.Source { return evidence.Source{Kind: "extract"} }},
		"derived": {source: func(p evidence.ManifestRecord) evidence.Source {
			return evidence.Source{Kind: "import", Segments: 1, Derived: derivedOf(p)}
		}},
		"multi-segment": {source: func(evidence.ManifestRecord) evidence.Source {
			return evidence.Source{Kind: "import", Segment: 1, Segments: 2}
		}},
		"unreadable parent": {source: func(evidence.ManifestRecord) evidence.Source {
			return evidence.Source{Kind: "import", Segment: 1, Segments: 1}
		}, remove: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := evidencetest.NewCase(t)
			base := evidencetest.AddImage(t, c, data)
			parent := rawArtifact(t, c, "parents/p.bin", tc.source(base), data, nil)
			evidencetest.AddRecovered(t, c, parent, nil, evidencetest.RecoveredSpec{
				Runs: []evidence.Run{{Offset: 16000, Length: 1000}}, Data: bytes.Repeat([]byte{7}, 1000),
			})
			if tc.remove {
				evidencetest.RemoveArtifactFile(t, c.Dir, parent)
			}
			evidencetest.RequireNoProblem(t, verifyOf(t, c), beyond)
		})
	}
	// the control: the same run past a plain single raw parent IS flagged
	c, parent, _ := recoverParent(t)
	evidencetest.AddRecovered(t, c, parent, nil, evidencetest.RecoveredSpec{
		Runs: []evidence.Run{{Offset: 16000, Length: 1000}}, Data: bytes.Repeat([]byte{7}, 1000),
	})
	evidencetest.RequireProblem(t, verifyOf(t, c), beyond)
}

// R3: the runs artifact a recovered artifact names must be a runs sidecar in its own directory.
func TestVerifyRecoveredRunsArtifactMustBeASidecarInTheSameDirectory(t *testing.T) {
	const msg = "is not a runs sidecar in the same directory"
	for name, tc := range map[string]struct {
		kind, path string
		wantProb   bool
	}{
		"wrong kind":      {"extract", "recovered/p1-mtfs/000001-a.bin.runs.jsonl", true},
		"other directory": {"runs", "recovered/p1-mtfs/other/000001-a.bin.runs.jsonl", true},
		"good sidecar":    {"runs", "recovered/p1-mtfs/000001-a.bin.runs.jsonl", false},
	} {
		t.Run(name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			sc := rawArtifact(t, c, tc.path, evidence.Source{Kind: tc.kind, Derived: derivedOf(parent)}, []byte(`{"offset":0,"length":64}`+"\n"), nil)
			dd := derivedOf(parent)
			dd.RunsArtifact = sc.ID
			dd.Recovery = validRecovery(64)
			rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, data[:64], nil)
			rep := verifyOf(t, c)
			if tc.wantProb {
				evidencetest.RequireProblem(t, rep, msg)
			} else {
				evidencetest.RequireNoProblem(t, rep, msg)
			}
		})
	}
}
