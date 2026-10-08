package recordstest

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestRecordstestProvenanceHelpers(t *testing.T) {
	c := NewCase(t)
	parent := AddArtifact(t, c, "image.bin", make([]byte, 4096))
	AddRunsSidecar(t, c, parent, "r.jsonl", []evidence.Run{{Offset: 0, Length: 4}})
	bin, sc := AddUnallocatedExport(t, c, parent, []evidence.UnallocRun{{Offset: 0, Length: 8, ImageOffset: 64}}, make([]byte, 8))
	if bin.Source.Derived.RunsArtifact != sc.ID || bin.Source.Kind != "unallocated" {
		t.Fatalf("export %+v does not reference its run map %s", bin.Source, sc.ID)
	}
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("helpers must produce a case verify accepts: %v %v", err, rep.Problems)
	}
	SetManifestSource(t, c.Dir, bin.ID, func(s *evidence.Source) { s.Derived.FSPath = "/forged" })
	rep, err = c.Verify()
	if err != nil || rep.OK() {
		t.Fatalf("a rewritten manifest source must make verify complain: %v %v", err, rep.OK())
	}
}

func TestRecordstestSetArtifactFileBytes(t *testing.T) {
	c := NewCase(t)
	parent := AddArtifact(t, c, "image.bin", []byte("original"))
	SetArtifactFileBytes(t, c.Dir, parent.ID, []byte("tampered"))
	rep, err := c.Verify()
	if err != nil || rep.OK() {
		t.Fatalf("a changed file must make verify complain: %v %v", err, rep.OK())
	}
}
