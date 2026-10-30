package evidence_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// TestVerifyCarveArtifactScopeIsNotTrusted: the scope "artifact" in a record
// is not trusted. A carve of an unallocated export is made through its parent
// image and never has scope artifact, so a relabelled one (which would skip
// the allocation recheck) is a problem; the scope directory must be raw. An
// imported image stays a legitimate parent: a carve of an artifact reads it
// raw (whole-branch finding 2).
func TestVerifyCarveArtifactScopeIsNotTrusted(t *testing.T) {
	const want = "but the parent"
	t.Run("unallocated export parent is flagged", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.RewriteManifest(t, c.Dir, parent.ID, func(r *evidence.ManifestRecord) { r.Source.Kind = "unallocated" })
		evidencetest.AddRecovered(t, c, parent, data, carveSpec("artifact", evidence.AllocSummary{Unknown: 64}))
		evidencetest.RequireProblem(t, verifyOf(t, c), want)
	})
	t.Run("imported parent is clean", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddRecovered(t, c, parent, data, carveSpec("artifact", evidence.AllocSummary{Unknown: 64}))
		evidencetest.RequireNoProblem(t, verifyOf(t, c), want)
	})
	t.Run("other scope directory is flagged", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		spec := carveSpec("artifact", evidence.AllocSummary{Unknown: 64})
		spec.Path = "carved/volume/0000000000000000-png.png"
		evidencetest.AddRecovered(t, c, parent, data, spec)
		evidencetest.RequireProblem(t, verifyOf(t, c), `scope directory "volume" does not match the recovery scope "artifact"`)
	})
}
