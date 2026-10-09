package recordstest

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// requireOnlyProblem fails unless verify reports exactly one problem and it contains want.
func requireOnlyProblem(t *testing.T, c *evidence.Case, want string) {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], want) {
		t.Fatalf("problems = %q, want exactly one containing %q", rep.Problems, want)
	}
}

func provenanceCase(t *testing.T) (*evidence.Case, evidence.ManifestRecord, evidence.ManifestRecord) {
	t.Helper()
	c := NewCase(t)
	parent := AddArtifact(t, c, "image.bin", make([]byte, 4096))
	AddRunsSidecar(t, c, parent, "r.jsonl", []evidence.Run{{Offset: 0, Length: 4}})
	bin, sc := AddUnallocatedExport(t, c, parent, []evidence.UnallocRun{{Offset: 0, Length: 8, ImageOffset: 64}}, make([]byte, 8))
	if bin.Source.Derived.RunsArtifact != sc.ID || bin.Source.Kind != "unallocated" {
		t.Fatalf("export %+v does not reference its run map %s", bin.Source, sc.ID)
	}
	return c, parent, bin
}

func TestRecordstestProvenanceHelpers(t *testing.T) {
	c, _, _ := provenanceCase(t)
	rep, err := c.Verify()
	if err != nil || !rep.OK() || len(rep.Problems) != 0 {
		t.Fatalf("helpers must produce a case verify accepts: %v %q", err, rep.Problems)
	}
}

func TestRecordstestSetManifestSourceLeavesOnlyTheAuditDifference(t *testing.T) {
	c, _, bin := provenanceCase(t)
	SetManifestSource(t, c.Dir, bin.ID, func(s *evidence.Source) { s.Derived.FSPath = "/forged" })
	requireOnlyProblem(t, c, "source differs from audit")
}

func TestRecordstestSetArtifactFileBytesLeavesOnlyTheHashMismatch(t *testing.T) {
	c, parent, _ := provenanceCase(t)
	SetArtifactFileBytes(t, c.Dir, parent.ID, make([]byte, 4095))
	requireOnlyProblem(t, c, "hash mismatch")
}
