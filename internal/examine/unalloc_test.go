package examine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

type unallocLine struct {
	Offset      int64 `json:"offset"`
	Length      int64 `json:"length"`
	ImageOffset int64 `json:"image_offset"`
}

// checkUnalloc verifies the two artifacts of an export against the image:
// the sidecar lines are contiguous in unallocated.bin and each maps to the
// image bytes it holds.
func checkUnalloc(t *testing.T, c *evidence.Case, img []byte, sum examine.Summary, wantImageRuns []evidence.Run, dir string) (bin evidence.ManifestRecord) {
	t.Helper()
	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	side, bin := sum.Artifacts[0], sum.Artifacts[1]
	if side.Source.Kind != "runs" || !strings.HasSuffix(side.Path, "/"+sum.AnalysisID+"/"+dir+"/unallocated.runs.jsonl") ||
		!strings.HasSuffix(bin.Path, "/"+sum.AnalysisID+"/"+dir+"/unallocated.bin") {
		t.Fatalf("paths: sidecar %q (%s) bin %q", side.Path, side.Source.Kind, bin.Path)
	}
	var lines []unallocLine
	for _, l := range strings.Split(strings.TrimSuffix(string(readArtifact(t, c, side)), "\n"), "\n") {
		var u unallocLine
		if err := json.Unmarshal([]byte(l), &u); err != nil {
			t.Fatalf("sidecar line %q: %v", l, err)
		}
		lines = append(lines, u)
	}
	if len(lines) != len(wantImageRuns) {
		t.Fatalf("sidecar lines = %v, want runs %v", lines, wantImageRuns)
	}
	got := readArtifact(t, c, bin)
	var want []byte
	var off int64
	for i, u := range lines {
		if u.Offset != off || u.Length != wantImageRuns[i].Length || u.ImageOffset != wantImageRuns[i].Offset {
			t.Errorf("line %d = %+v, want offset %d and image run %+v", i, u, off, wantImageRuns[i])
		}
		if !bytes.Equal(got[u.Offset:u.Offset+u.Length], img[u.ImageOffset:u.ImageOffset+u.Length]) {
			t.Errorf("line %d: bin bytes differ from the image at %d", i, u.ImageOffset)
		}
		want = append(want, img[u.ImageOffset:u.ImageOffset+u.Length]...)
		off += u.Length
	}
	if !bytes.Equal(got, want) || bin.SHA256 != sha256hex(want) || bin.Size != off {
		t.Errorf("unallocated.bin (%d bytes) is not the concatenation of the runs (%d bytes)", len(got), len(want))
	}
	d := bin.Source.Derived
	if bin.Source.Kind != "unallocated" || d == nil || d.RunsArtifact != side.ID || d.Runs != nil ||
		d.ParentID != s0(c).ID || d.ParentSHA256 != s0(c).SHA256 {
		t.Errorf("bin source = %+v derived = %+v", bin.Source, d)
	}
	verifyOK(t, c)
	return bin
}

// s0 is the first manifest record (the imported image in these tests).
func s0(c *evidence.Case) evidence.ManifestRecord {
	recs, err := c.Manifest()
	if err != nil || len(recs) == 0 {
		panic("no manifest")
	}
	return recs[0]
}

func TestExportUnallocatedFS(t *testing.T) {
	c := newCase(t)
	s, img := session(t, c, 5,
		fstest.Node{Path: "/a", Data: pattern(3*blk, 1), Fragments: 2},
		fstest.Node{Path: "/b", Data: pattern(blk, 2)})
	// Fill the free space with a marker so a wrong offset is visible.
	part := s.Table.Partitions[0]
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	fsRuns, err := fsys.Unallocated()
	if err != nil || len(fsRuns) == 0 {
		t.Fatalf("setup: Unallocated = %v, %v", fsRuns, err)
	}
	for _, r := range fsRuns {
		for i := int64(0); i < r.Length; i++ {
			img[part.Start+r.Offset+i] = 0xA5
		}
	}
	// Re-import the marked image into a fresh case.
	c2 := newCase(t)
	recs := importImage(t, c2, img, 1)
	s2 := openSession(t, c2, recs[0].ID)
	var want []evidence.Run
	for _, r := range fsRuns {
		want = append(want, evidence.Run{Offset: r.Offset + part.Start, Length: r.Length})
	}
	var done, total int64
	sum, err := s2.ExportUnallocated(context.Background(), examine.UnallocOptions{
		Partition: -1,
		Progress:  func(d, tot int64) { done, total = d, tot },
	})
	if err != nil {
		t.Fatal(err)
	}
	bin := checkUnalloc(t, c2, img, sum, want, "p1-mtfs")
	if bin.Source.Derived.Partition != 1 || bin.Source.Derived.PartitionOffset != part.Start || bin.Source.Derived.FSType != "mtfs" {
		t.Errorf("derived = %+v", bin.Source.Derived)
	}
	if done != bin.Size || total != bin.Size {
		t.Errorf("progress = %d/%d, want %d/%d", done, total, bin.Size, bin.Size)
	}
	if !bytes.Equal(readArtifact(t, c2, bin), bytes.Repeat([]byte{0xA5}, int(bin.Size))) {
		t.Error("unallocated.bin is not exactly the marked free space")
	}
	if sum.Bytes != bin.Size {
		t.Errorf("Bytes = %d", sum.Bytes)
	}
	st := auditByAction(t, c2, "analysis.start")
	if len(st) != 1 || st[0].Details["op"] != "unalloc" || st[0].Details["analysis_id"] != sum.AnalysisID {
		t.Errorf("start = %+v", st)
	}
	if en := auditByAction(t, c2, "analysis.end"); len(en) != 1 || detailNum(en[0].Details["files"]) != 1 {
		t.Errorf("end = %+v", en)
	}
}

func TestExportUnallocatedVolume(t *testing.T) {
	c := newCase(t)
	s, img := session(t, c, 1, fstest.Node{Path: "/a", Data: []byte("a")})
	if len(s.Table.Unallocated) == 0 {
		t.Fatal("setup: no volume-level unallocated space")
	}
	var want []evidence.Run
	for _, r := range s.Table.Unallocated {
		want = append(want, evidence.Run{Offset: r.Offset, Length: r.Length})
	}
	sum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: 99, Volume: true})
	if err != nil {
		t.Fatal(err)
	}
	bin := checkUnalloc(t, c, img, sum, want, "volume")
	if d := bin.Source.Derived; d.Partition != 0 || d.PartitionOffset != 0 || d.FSType != "" {
		t.Errorf("derived = %+v", d)
	}
}

func TestExportUnallocatedNone(t *testing.T) {
	c := newCase(t)
	// A bare filesystem image: no partition table, so no volume-level gaps.
	recs := importImage(t, c, fstest.Build(fstest.BuildSpec{Nodes: []fstest.Node{{Path: "/a", Data: []byte("a")}}}), 1)
	s := openSession(t, c, recs[0].ID)
	sum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Volume: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 0 || len(sum.Artifacts) != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	ws := auditByAction(t, c, "analysis.warning")
	if len(ws) != 1 || !strings.Contains(ws[0].Details["reason"].(string), "no unallocated space") {
		t.Errorf("warnings = %+v", ws)
	}
	if len(auditByAction(t, c, "analysis.end")) != 1 {
		t.Error("no analysis.end")
	}
	verifyOK(t, c)
}

func TestExportUnallocatedCancelledKeepsPartial(t *testing.T) {
	c := newCase(t)
	s, _ := session(t, c, 400, fstest.Node{Path: "/a", Data: []byte("a")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sum, err := s.ExportUnallocated(ctx, examine.UnallocOptions{Partition: -1, Progress: func(d, _ int64) {
		if d > 0 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(sum.Artifacts) != 2 || !sum.Artifacts[1].Incomplete || sum.Artifacts[0].Incomplete {
		t.Fatalf("artifacts = %+v", sum.Artifacts)
	}
	if len(auditByAction(t, c, "analysis.error")) != 1 {
		t.Error("no analysis.error")
	}
	verifyOK(t, c)
}

func TestExportUnallocatedDropsInvalidFSRuns(t *testing.T) {
	// A filesystem reporting a run outside itself must not yield wrong provenance.
	c2 := newCase(t)
	s2, _ := sessionHook(t, c2, hookFS{mapUnalloc: func(rs []filesys.Run) []filesys.Run {
		return append(rs, filesys.Run{Offset: -1, Length: 5}, filesys.Run{Offset: 1 << 40, Length: 512})
	}}, 2, fstest.Node{Path: "/a", Data: []byte("a")})
	sum, err := s2.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Files != 1 || sum.Skipped < 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if ws := auditByAction(t, c2, "analysis.warning"); len(ws) != 1 {
		t.Errorf("warnings = %+v", ws)
	}
	verifyOK(t, c2)
}

func TestExportUnallocatedMergesOverlappingFSRuns(t *testing.T) {
	c := newCase(t)
	var orig []filesys.Run
	s, img := sessionHook(t, c, hookFS{mapUnalloc: func(rs []filesys.Run) []filesys.Run {
		orig = append([]filesys.Run(nil), rs...)
		// A hostile filesystem repeats and overlaps its free space, in unsorted order.
		return append([]filesys.Run{{Offset: rs[0].Offset + 10, Length: 100}}, append(rs, rs[0], filesys.Run{Offset: rs[0].Offset, Length: 0})...)
	}}, 5, fstest.Node{Path: "/a", Data: pattern(2*blk, 1)})
	sum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatal(err)
	}
	start := s.Table.Partitions[0].Start
	var want []evidence.Run
	var total int64
	for _, r := range orig {
		want = append(want, evidence.Run{Offset: r.Offset + start, Length: r.Length})
		total += r.Length
	}
	bin := checkUnalloc(t, c, img, sum, want, "p1-mtfs")
	if bin.Size != total {
		t.Errorf("unallocated.bin = %d bytes, want the %d bytes of the distinct free space", bin.Size, total)
	}
	ws := auditByAction(t, c, "analysis.warning")
	if len(ws) != 1 || !strings.Contains(ws[0].Details["reason"].(string), "ignored 1 empty") || strings.Contains(ws[0].Details["reason"].(string), "ignored 1 unallocated") {
		t.Errorf("warnings = %+v", ws)
	}
}
