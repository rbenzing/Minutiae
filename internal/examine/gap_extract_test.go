package examine_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func gapNodes() (nodes []fstest.Node, big []byte, base int) {
	big = pattern(6*e01Chunk+100, 3)
	nodes = []fstest.Node{
		{Path: "/big.bin", Data: big},
		{Path: "/other.txt", Data: pattern(blk, 9)},
	}
	base = bytes.Index(disk(fstest.Build(fstest.BuildSpec{Label: "L", FreeBlocks: 2, Nodes: nodes})), big[:64])
	return nodes, big, base
}

func TestExtractFlagsFileOverlappingAcquirerGap(t *testing.T) {
	nodes, big, base := gapNodes()
	c := newCase(t)
	gapAt := int64(base) + 512 // inside big.bin, sector aligned
	s, _ := e01GapSession(t, c, nodes, gapAt, 3, true)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})

	byPath := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		byPath[r.Source.RemotePath] = r
	}
	n := byPath["/big.bin"].Source.Derived.AcquisitionGaps
	if n == nil || n.Bytes != 3*512 || n.Ranges != 1 || n.Unknown || len(n.First) != 1 ||
		n.First[0] != (evidence.Run{Offset: gapAt, Length: 3 * 512}) {
		t.Fatalf("big.bin note = %+v", n)
	}
	if o := byPath["/other.txt"].Source.Derived.AcquisitionGaps; o != nil {
		t.Fatalf("other.txt does not touch the gap but carries %+v", o)
	}
	// The extracted bytes are the zero-filled media, exactly as stored: flagged, not repaired.
	got := readArtifact(t, c, byPath["/big.bin"])
	want := bytes.Clone(big)
	clear(want[512 : 512+3*512])
	if !bytes.Equal(got, want) {
		t.Fatal("extracted bytes differ from the stored (zero-filled) media")
	}
	verifyOK(t, c)
}

func TestExtractCleanE01HasNoGapNotes(t *testing.T) {
	c := newCase(t)
	s, _ := e01ChunkSession(t, c, []fstest.Node{{Path: "/a.txt", Data: []byte("hello")}}, -1)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/a.txt"}})
	for _, r := range sum.Artifacts {
		if d := r.Source.Derived; d != nil && d.AcquisitionGaps != nil {
			t.Fatalf("%s carries %+v on an image without gaps", r.Path, d.AcquisitionGaps)
		}
	}
}

func TestGapNoteUnknownWhenRunsAreNotRecorded(t *testing.T) {
	nodes := []fstest.Node{
		{Path: "/inline.txt", Data: []byte("tiny"), Inline: true},
		{Path: "/empty.txt", Data: nil},
	}
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, 20*512, 1, true) // a gap somewhere; the inline file has no runs
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/inline.txt", "/empty.txt"}})
	byPath := map[string]evidence.ManifestRecord{}
	for _, r := range sum.Artifacts {
		byPath[r.Source.RemotePath] = r
	}
	if n := byPath["/inline.txt"].Source.Derived.AcquisitionGaps; n == nil || !n.Unknown || n.Bytes != 0 {
		t.Fatalf("inline file note = %+v; want unknown (no runs, so no overlap can be decided)", n)
	}
	if e, ok := byPath["/empty.txt"]; ok && e.Source.Derived.AcquisitionGaps != nil {
		t.Fatalf("an empty file has no bytes to flag: %+v", e.Source.Derived.AcquisitionGaps)
	}
	verifyOK(t, c)
}

// The whole disk is declared unreadable, so every byte of the filesystem's free
// space lies in a gap: the note must exist and equal the export's size exactly.
func TestUnallocExportFlagsGapOverlap(t *testing.T) {
	nodes, _, _ := gapNodes()
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, 0, -1, false)
	sum, err := s.ExportUnallocated(t.Context(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatalf("ExportUnallocated: %v", err)
	}
	var bins int
	for _, r := range sum.Artifacts {
		if r.Source.Kind != "unallocated" {
			continue
		}
		bins++
		n := r.Source.Derived.AcquisitionGaps
		if n == nil || n.Unknown || n.Bytes != r.Size || n.Ranges <= 0 {
			t.Fatalf("unallocated note = %+v for an export of %d bytes, all inside the declared gap", n, r.Size)
		}
	}
	if bins != 1 {
		t.Fatalf("%d unallocated artifacts, want 1", bins)
	}
	for _, r := range sum.Artifacts {
		if r.Source.Kind == "runs" && r.Source.Derived.AcquisitionGaps != nil {
			t.Fatalf("the runs sidecar carries a gap note: %+v", r.Source.Derived.AcquisitionGaps)
		}
	}
	verifyOK(t, c)
}

// An image whose error2 section is damaged has UNKNOWN acquisition gaps: its
// artifacts carry an unknown note, never none. An intact empty section and an
// absent one declare no gaps, so no note.
func TestGapNoteUnknownWhenErr2Damaged(t *testing.T) {
	nodes, _, _ := gapNodes()
	damage := func(segs [][]byte) {
		for _, s := range segs {
			for _, sec := range ewftest.Sections(s) {
				if sec.Type == "error2" {
					s[sec.Offset+76+3] ^= 0xFF
				}
			}
		}
	}
	c := newCase(t)
	s, _ := e01GapSessionMut(t, c, nodes, 20*512, 1, true, damage)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/other.txt"}})
	var seen int
	for _, r := range sum.Artifacts {
		if r.Source.Kind != "extract" {
			continue
		}
		seen++
		n := r.Source.Derived.AcquisitionGaps
		if n == nil || !n.Unknown {
			t.Fatalf("extract from an image with a damaged error2 carries %+v; want unknown", n)
		}
	}
	if seen != 1 {
		t.Fatalf("%d extract artifacts", seen)
	}
	verifyOK(t, c)
}

func TestGapNoteJSONKeepsUnknown(t *testing.T) {
	b, err := json.Marshal(evidence.AcquisitionGapNote{Unknown: true})
	if err != nil || !strings.Contains(string(b), `"unknown":true`) {
		t.Fatalf("%s %v", b, err)
	}
}

// A declared range of zero sectors is nothing unreadable: no note.
func TestGapNoteAbsentWhenOnlyEmptyRangeDeclared(t *testing.T) {
	nodes, _, _ := gapNodes()
	c := newCase(t)
	s, _ := e01GapSession(t, c, nodes, 20*512, 0, false)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/other.txt"}})
	for _, r := range sum.Artifacts {
		if d := r.Source.Derived; d != nil && d.AcquisitionGaps != nil {
			t.Fatalf("%s carries %+v with no unreadable range", r.Path, d.AcquisitionGaps)
		}
	}
}
