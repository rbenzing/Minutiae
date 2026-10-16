package examine_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// corruptChunkAt flips a byte of the stored chunk holding disk offset at.
func corruptChunkAt(at int64) func([][]byte) {
	return func(segs [][]byte) {
		l := ewftest.ChunkLocs(segs)[at/e01Chunk]
		segs[l.Segment-1][l.Offset+l.Length/2] ^= 0x5A
	}
}

// An incomplete extract's gap note is recomputed over the bytes that were
// kept: a gap inside the kept prefix is counted, a gap beyond the unreadable
// chunk (declared in the full run list, not in the kept bytes) is not.
func TestExtractIncompleteGapNoteCoversOnlyTheKeptPrefix(t *testing.T) {
	nodes, _, base := gapNodes()
	corruptAt := int64(base) + 3*e01Chunk // the file's 4th chunk or so is unreadable
	for _, tc := range []struct {
		name      string
		gapAt     int64
		wantBytes int64
	}{
		{"gap inside the kept prefix", int64(base) + 512, 3 * 512},
		{"gap beyond the unreadable chunk", int64(base) + 5*e01Chunk + 512, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCase(t)
			s, _ := e01GapSessionMut(t, c, nodes, tc.gapAt, 3, true, corruptChunkAt(corruptAt))
			sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/big.bin"}})
			var rec evidence.ManifestRecord
			for _, r := range sum.Artifacts {
				if r.Source.Kind == "extract" {
					rec = r
				}
			}
			if !rec.Incomplete || rec.Size == 0 {
				t.Fatalf("expected a non-empty incomplete artifact: %+v", rec)
			}
			n := rec.Source.Derived.AcquisitionGaps
			if tc.wantBytes == 0 {
				if n != nil {
					t.Fatalf("a gap beyond the kept bytes is reported on the partial artifact: %+v", n)
				}
			} else if n == nil || n.Bytes != tc.wantBytes || n.Ranges != 1 || n.Unknown {
				t.Fatalf("note = %+v, want %d bytes in 1 range", n, tc.wantBytes)
			}
			verifyOK(t, c)
		})
	}
}

// The runs sidecar describes runs only: the gap note stays on the content
// artifact.
func TestExtractRunsSidecarCarriesNoGapNote(t *testing.T) {
	frags := evidence.MaxInlineRuns + 2000
	big := pattern(frags*blk, 3)
	nodes := []fstest.Node{{Path: "/big.bin", Data: big, Fragments: frags}}
	base := bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	if base < 0 {
		t.Fatal("cannot find the file data in the disk")
	}
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, int64(base)+2*50*blk, 3, true) // fragments sit one block apart
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/big.bin"}})
	var content, sidecar *evidence.ManifestRecord
	for i, r := range sum.Artifacts {
		switch r.Source.Kind {
		case "extract":
			content = &sum.Artifacts[i]
		case "runs":
			sidecar = &sum.Artifacts[i]
		}
	}
	if content == nil || sidecar == nil {
		t.Fatalf("want an extract and a runs sidecar, got %d artifacts", len(sum.Artifacts))
	}
	if n := content.Source.Derived.AcquisitionGaps; n == nil || n.Bytes == 0 {
		t.Fatalf("the content artifact lost its note: %+v", n)
	}
	if n := sidecar.Source.Derived.AcquisitionGaps; n != nil {
		j, _ := json.Marshal(n)
		t.Fatalf("the runs sidecar carries a gap note: %s", j)
	}
	verifyOK(t, c)
}
