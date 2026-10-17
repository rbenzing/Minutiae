package evidence_test

import (
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func extractSrc(parent evidence.ManifestRecord, d evidence.Derivation) evidence.Source {
	d.ParentID, d.ParentSHA256, d.Partition, d.FSType = parent.ID, parent.SHA256, 1, "mtfs"
	return evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &d}
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func TestVerifyRunSumRule(t *testing.T) {
	rows := []struct {
		name string
		do   func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord)
		want string // "" = clean of R8
	}{
		{"inline runs equal the size", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 4}, {Offset: 100, Length: 4}}}), patterned(8))
		}, ""},
		{"inline runs below the size", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 4}}}), patterned(8))
		}, "runs cover 4 bytes but the artifact holds 8"},
		{"inline runs above the size", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 16}}}), patterned(8))
		}, "runs cover 16 bytes but the artifact holds 8"},
		{"a sparse hole counts as covered bytes", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: -1, Length: 4}, {Offset: 0, Length: 3}}}), patterned(7))
		}, ""},
		{"sidecar runs equal the size", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			sc := recordstest.AddRunsSidecar(t, c, img, "f.bin.runs.jsonl", []evidence.Run{{Offset: 0, Length: 8}})
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{RunsArtifact: sc.ID}), patterned(8))
		}, ""},
		{"sidecar runs differ from the size", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			sc := recordstest.AddRunsSidecar(t, c, img, "f.bin.runs.jsonl", []evidence.Run{{Offset: 0, Length: 5}})
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{RunsArtifact: sc.ID}), patterned(8))
		}, "runs cover 5 bytes but the artifact holds 8"},
		{"no runs recorded is unknown, not damaged", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{}), patterned(8))
		}, ""},
		{"an invalid run is a problem", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 0}, {Offset: 0, Length: 8}}}), patterned(8))
		}, "run 0 is not valid"},
		{"lengths that overflow int64 are a problem, not a wrap", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: -1, Length: math.MaxInt64}, {Offset: -1, Length: 9}}}), patterned(8))
		}, "run lengths add up to more than"},
		{"an unallocated export with its own run map", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddUnallocatedExport(t, c, img, []evidence.UnallocRun{{Offset: 0, Length: 4, ImageOffset: 100}, {Offset: 4, Length: 4, ImageOffset: 300}}, patterned(8))
		}, ""},
		{"an unallocated export that does not add up", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			recordstest.AddUnallocatedExport(t, c, img, []evidence.UnallocRun{{Offset: 0, Length: 4, ImageOffset: 100}, {Offset: 4, Length: 4, ImageOffset: 300}}, patterned(6))
		}, "runs cover 8 bytes but the artifact holds 6"},
		{"more inline runs than MaxInlineRuns (D100)", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			runs := make([]evidence.Run, evidence.MaxInlineRuns+1)
			for i := range runs {
				runs[i] = evidence.Run{Offset: int64(i) * 2, Length: 1}
			}
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: runs}), patterned(len(runs)))
		}, "inline runs (more than 4096"},
		{"inline runs together with a sidecar (D101)", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			sc := recordstest.AddRunsSidecar(t, c, img, "f.bin.runs.jsonl", []evidence.Run{{Offset: 0, Length: 8}})
			recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{RunsArtifact: sc.ID, Runs: []evidence.Run{{Offset: 0, Length: 8}}}), patterned(8))
		}, "records both inline runs and a runs sidecar"},
		{"an incomplete artifact may have runs beyond its bytes", func(t *testing.T, c *evidence.Case, img evidence.ManifestRecord) {
			f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 16}}}), patterned(8))
			evidencetest.RewriteManifest(t, c.Dir, f.ID, func(r *evidence.ManifestRecord) { r.Incomplete, r.Error = true, "cut" })
		}, ""},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
			tc.do(t, c, img)
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				evidencetest.RequireNoProblem(t, rep, "runs cover")
				evidencetest.RequireNoProblem(t, rep, "is not valid")
				evidencetest.RequireNoProblem(t, rep, "add up to more than")
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
			evidencetest.RequireProblem(t, rep, "artifact \"") // the artifact is named
			if rep.OK() {
				t.Fatal("a failing R8 must make the verify report not OK")
			}
		})
	}
}

// A runs sidecar that cannot be read is reported, not skipped.
func TestVerifyRunSumRuleReportsAnUnreadableSidecar(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	sc := recordstest.AddDerivedWith(t, c, "f.bin.runs.jsonl", evidence.Source{Kind: "runs", DeviceID: "dev1", Derived: &evidence.Derivation{ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs"}}, []byte("not json\n"))
	recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{RunsArtifact: sc.ID}), patterned(8))
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "runs unreadable")
}

// A recovered artifact is judged by R4 alone: the sum problem is reported once, not twice.
func TestVerifyRunSumRuleLeavesRecoveredKindsToR4(t *testing.T) {
	c := evidencetest.NewCase(t)
	img := evidencetest.AddImage(t, c, patterned(1<<12))
	evidencetest.AddRecovered(t, c, img, patterned(1<<12), evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 8}}, Data: []byte("abcd")})
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range rep.Problems {
		if strings.Contains(p, "runs cover 8 bytes but the artifact holds 4") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the recovered sum problem is listed %d times: %v", n, rep.Problems)
	}
}
