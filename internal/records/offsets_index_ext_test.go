package records_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// A hop whose pieces are all holes validates no runs: the parent's invalid runs are never looked at.
func TestResolveOffsetsValidatesRunsOnlyForNonHolePieces(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<12))
	// the middle artifact holds INVALID runs (a run of length 0)
	mid := recordstest.AddDerivedWith(t, c, "mid.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 0}}}), []byte("midmidmid"))
	// the file lies wholly in a hole of the middle artifact
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(mid, evidence.Derivation{Runs: []evidence.Run{{Offset: -1, Length: 9}}}), make([]byte, 9))
	p := offsetProv(t, c, f, &records.Range{Offset: 2, Length: 4})
	t.Logf("state %v reason %q problems %+v image %+v hops %+v", p.Offset.State, p.Offset.Reason, p.Problems, p.Offset.Image, p.Offset.Hops)
	requireTranslated(t, p)
	if len(p.Problems) != 0 {
		t.Fatalf("problems %+v", p.Problems)
	}
	if len(p.Offset.Image) != 1 || !p.Offset.Image[0].Hole || p.Offset.Image[0].Length != 4 || p.Offset.Image[0].ArtifactOffset != 2 {
		t.Fatalf("image %+v", p.Offset.Image)
	}
}
