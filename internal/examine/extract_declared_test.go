package examine_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// A file with more runs than fit inline, cut short by an unreadable chunk: the
// full run list stays in the case AND the partial artifact names it.
func TestExtractIncompleteArtifactReferencesFullRunList(t *testing.T) {
	frags := evidence.MaxInlineRuns + 2000
	size := frags * blk
	big := pattern(size, 3)
	nodes := []fstest.Node{{Path: "/big.bin", Data: big, Fragments: frags}}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	if base < 0 {
		t.Fatal("cannot find the file data in the disk")
	}
	corruptAt := int64(base + (frags-200)*2*blk) // 200 fragments from the end; fragments sit one block apart
	c := newCase(t)
	s, _ := e01ChunkSessionOpts(t, c, nodes, corruptAt, mtfsOpts())
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/big.bin"}})

	var partial evidence.ManifestRecord
	byID := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		byID[r.ID] = r
		if r.Source.Kind == "extract" {
			partial = r
		}
	}
	if !partial.Incomplete {
		t.Fatalf("the artifact is not incomplete: %+v", partial)
	}
	d := partial.Source.Derived
	if d.DeclaredRunsArtifact == "" {
		t.Fatalf("the partial extract does not reference the full run list; its error says %q", partial.Error)
	}
	full, ok := byID[d.DeclaredRunsArtifact]
	if !ok || full.Source.Kind != "runs" {
		t.Fatalf("declared runs artifact %q is not a runs sidecar of this run", d.DeclaredRunsArtifact)
	}
	if d.DeclaredRunsArtifact == d.RunsArtifact {
		t.Fatal("the declared full list must not be the captured prefix list")
	}
	if !strings.Contains(partial.Error, d.DeclaredRunsArtifact) {
		t.Errorf("the error no longer names the full list: %q", partial.Error)
	}
	lines := bytes.Count(readArtifact(t, c, full), []byte("\n"))
	if lines < frags-1 { // the filesystem may merge adjacent fragments; it never lists fewer than the file's runs minus a few
		t.Fatalf("the full list holds %d runs, want about %d", lines, frags)
	}
	verifyOK(t, c)
}

// An inline prefix (few kept runs) still links the full list: the sidecar must
// not be left unreferenced because the prefix needed no sidecar of its own.
func TestExtractIncompleteInlinePrefixStillLinksFullRunList(t *testing.T) {
	frags := evidence.MaxInlineRuns + 2000
	size := frags * blk
	big := pattern(size, 3)
	nodes := []fstest.Node{{Path: "/big.bin", Data: big, Fragments: frags}}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	if base < 0 {
		t.Fatal("cannot find the file data in the disk")
	}
	corruptAt := int64(base + 40*2*blk) // early: the kept prefix has fewer runs than MaxInlineRuns
	c := newCase(t)
	s, _ := e01ChunkSessionOpts(t, c, nodes, corruptAt, mtfsOpts())
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/big.bin"}})
	var seen bool
	for _, r := range sum.Artifacts {
		if r.Source.Kind != "extract" {
			continue
		}
		seen = true
		d := r.Source.Derived
		if !r.Incomplete || len(d.Runs) == 0 || d.RunsArtifact != "" || d.DeclaredRunsArtifact == "" {
			t.Fatalf("incomplete %v, inline runs %d, runs sidecar %q, declared %q", r.Incomplete, len(d.Runs), d.RunsArtifact, d.DeclaredRunsArtifact)
		}
	}
	if !seen {
		t.Fatal("no extract artifact")
	}
	verifyOK(t, c)
}

func TestExtractCompleteArtifactHasNoDeclaredLink(t *testing.T) {
	c := newCase(t)
	s, _ := e01ChunkSession(t, c, []fstest.Node{{Path: "/a.txt", Data: pattern(3*blk, 1)}}, -1)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	for _, r := range sum.Artifacts {
		if d := r.Source.Derived; d != nil && d.DeclaredRunsArtifact != "" {
			t.Fatalf("complete artifact %s declares %q", r.Path, d.DeclaredRunsArtifact)
		}
	}
}
