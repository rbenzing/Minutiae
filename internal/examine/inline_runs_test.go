package examine_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// Content stored inline in filesystem metadata (ext4 inline data, fast
// symlinks) has no dedicated image blocks: File.Runs() is nil. The file is
// still extracted whole and verified, and the derivation records the
// filesystem identity but no image runs.
func TestExtractInlineFileHasNoRuns(t *testing.T) {
	c := newCase(t)
	content := []byte("tiny inline content")
	s, _ := session(t, c, 1,
		fstest.Node{Path: "/inline.txt", Data: content, Inline: true},
		fstest.Node{Path: "/normal.txt", Data: pattern(2*blk, 3)})
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	e, err := fsys.Lookup("/inline.txt")
	if err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	if f.Runs() != nil {
		t.Fatalf("setup: the inline file has runs %v", f.Runs())
	}

	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/inline.txt", "/normal.txt"}})
	if sum.Files != 2 || sum.Skipped != 0 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	rec := sum.Artifacts[0]
	if rec.Source.RemotePath != "/inline.txt" {
		t.Fatalf("artifact order: first is %q", rec.Source.RemotePath)
	}
	if string(readArtifact(t, c, rec)) != string(content) {
		t.Error("inline content not extracted")
	}
	d := rec.Source.Derived
	if d == nil {
		t.Fatal("no derivation recorded")
	}
	if len(d.Runs) != 0 || d.RunsArtifact != "" {
		t.Errorf("inline file recorded runs: inline=%v sidecar=%q", d.Runs, d.RunsArtifact)
	}
	if d.FSID != e.ID || d.FSPath != "/inline.txt" || d.FSType != "mtfs" || d.ParentID != s.Parent.ID {
		t.Errorf("provenance = %+v, want the filesystem identity without runs", d)
	}
	if len(auditByAction(t, c, "analysis.warning")) != 0 {
		t.Error("an inline file must not raise a warning")
	}
	if n := len(sum.Artifacts[1].Source.Derived.Runs); n == 0 {
		t.Error("the normal file next to it lost its runs")
	}
	verifyOK(t, c)
}
