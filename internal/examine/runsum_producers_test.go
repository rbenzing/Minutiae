package examine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// Every real producer of a complete derived artifact (a fragmented extract with inline runs, a plain
// extract, an extract with a runs sidecar, an unallocated export with its own run map) must satisfy
// rule R8: its runs add up to its size.
func TestVerifyRunSumCleanOnRealProducers(t *testing.T) {
	c := newCase(t)
	const many = evidence.MaxInlineRuns + 1 // more than MaxInlineRuns runs: a sidecar
	s, _ := session(t, c, 4,
		fstest.Node{Path: "/frag.bin", Data: pattern(5*blk, 1), Fragments: 3},
		fstest.Node{Path: "/plain.bin", Data: pattern(2*blk, 2)},
		fstest.Node{Path: "/many.bin", Data: pattern(many*blk, 3), Fragments: many})
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/frag.bin", "/plain.bin", "/many.bin"}})
	var inline, sidecar int
	for _, a := range sum.Artifacts {
		if d := a.Source.Derived; d != nil && a.Source.Kind == "extract" {
			if d.RunsArtifact != "" {
				sidecar++
			} else if len(d.Runs) > 0 {
				inline++
			}
		}
	}
	if inline < 2 || sidecar != 1 {
		t.Fatalf("the producers do not cover inline runs (%d) and a sidecar (%d): %+v", inline, sidecar, sum.Artifacts)
	}
	if _, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1}); err != nil {
		t.Fatal(err)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, "runs cover") || strings.Contains(p, "is not valid") {
			t.Fatalf("a real producer broke rule R8: %q", rep.Problems)
		}
	}
	verifyOK(t, c)
}
