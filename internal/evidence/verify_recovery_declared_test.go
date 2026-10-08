package evidence_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

const declaredLine = `{"offset":0,"length":64}` + "\n"

// plantDeclared writes a recovered artifact (64 bytes) whose Recovery names declared as
// its declared full run list, and returns it.
func plantDeclared(t *testing.T, c *evidence.Case, parent evidence.ManifestRecord, data []byte, path, declared string) evidence.ManifestRecord {
	t.Helper()
	dd := derivedOf(parent, evidence.Run{Offset: 0, Length: 64})
	dd.Recovery = validRecovery(64)
	dd.Recovery.DeclaredRunsArtifact = declared
	return rawArtifact(t, c, path, evidence.Source{Kind: "recover", Derived: dd}, data[:64], nil)
}

// FB-1: the declared full run list of an interrupted copy stays referenced and is checked like the sidecar
// of the captured runs.
func TestVerifyDeclaredRunsArtifact(t *testing.T) {
	const (
		notIn   = "declared runs artifact"
		notSide = "is not a runs sidecar in the same directory"
		notPar  = "is not a runs sidecar of the artifact's parent"
		twice   = "is referenced by more than one recovered artifact"
		orphan  = "is not referenced by any recovered artifact"
	)
	t.Run("good", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", sc.ID)
		rep := verifyOf(t, c)
		if !rep.OK() {
			t.Fatalf("problems: %v", rep.Problems)
		}
	})
	t.Run("not in the manifest", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", "feedfacefeedfacefeedfacefeedface")
		evidencetest.RequireProblem(t, verifyOf(t, c), notIn)
		evidencetest.RequireProblem(t, verifyOf(t, c), "is not in the manifest")
	})
	t.Run("wrong kind", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "extract", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", sc.ID)
		evidencetest.RequireProblem(t, verifyOf(t, c), notSide)
	})
	t.Run("other directory", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		sc := rawArtifact(t, c, "recovered/p1-mtfs/other/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", sc.ID)
		evidencetest.RequireProblem(t, verifyOf(t, c), notSide)
	})
	t.Run("other parent", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		other := rawArtifact(t, c, "p1-mtfs/o.bin", evidence.Source{Kind: "extract", Derived: derivedOf(parent)}, []byte("x"), nil)
		sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(other)}, []byte(declaredLine), nil)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", sc.ID)
		evidencetest.RequireProblem(t, verifyOf(t, c), notPar)
	})
	t.Run("referenced by two artifacts", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", sc.ID)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000002-b.bin", sc.ID)
		evidencetest.RequireProblem(t, verifyOf(t, c), twice)
	})
	t.Run("also the captured runs sidecar", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		sc := rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
		dd := derivedOf(parent)
		dd.RunsArtifact = sc.ID
		dd.Recovery = validRecovery(64)
		dd.Recovery.DeclaredRunsArtifact = sc.ID
		rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin", evidence.Source{Kind: "recover", Derived: dd}, data[:64], nil)
		evidencetest.RequireProblem(t, verifyOf(t, c), twice)
	})
	t.Run("unreferenced sidecar stays a problem", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		rawArtifact(t, c, "recovered/p1-mtfs/000001-a.bin.runs.jsonl", evidence.Source{Kind: "runs", Derived: derivedOf(parent)}, []byte(declaredLine), nil)
		plantDeclared(t, c, parent, data, "recovered/p1-mtfs/000001-a.bin", "")
		evidencetest.RequireProblem(t, verifyOf(t, c), orphan)
	})
}
