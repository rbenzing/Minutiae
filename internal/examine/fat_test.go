package examine_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// imageSession imports img as a whole-disk partitioned image and opens it with
// the default driver registry.
func imageSession(t *testing.T, img []byte) (*examine.Session, []byte) {
	t.Helper()
	c := newCase(t)
	data := disk(img)
	recs := importImage(t, c, data, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // default driver registry
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, data
}

// A FAT file whose cluster chain is shorter than its size is extracted as a
// partial artifact flagged incomplete: the runs of the chain's prefix are
// recorded (they are the exact provenance of the captured bytes), the read
// stops with a corrupt-chain error, and the filesystem's own warning is
// counted separately from the skip.
func TestExtractFATShortChainIsIncomplete(t *testing.T) {
	o := fattest.Options{Type: 16}
	g := fattest.Layout(o)
	content := pattern(4*blk, 9)
	img := fattest.Build(o, []fattest.File{
		{Path: "SHORT.BIN", Data: content},
		{Path: "FINE.BIN", Data: []byte("fine")},
	})
	// SHORT.BIN owns clusters 2..5 (a FAT16 root is outside the data area). End its chain after two clusters.
	for n := range g.NumFATs {
		binary.LittleEndian.PutUint16(img[int(g.FATStart(n))*g.SectorSize+3*2:], 0xFFFF)
	}
	s, data := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})

	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	short, fine := sum.Artifacts[0], sum.Artifacts[1]
	if strings.HasSuffix(short.Source.RemotePath, "FINE.BIN") {
		short, fine = fine, short
	}
	if !strings.HasSuffix(short.Source.RemotePath, "SHORT.BIN") {
		t.Fatalf("artifacts = %q / %q", short.Source.RemotePath, fine.Source.RemotePath)
	}
	if !strings.Contains(short.Path, "/p1-fat16/") {
		t.Errorf("path %q does not name the filesystem type", short.Path)
	}
	if !short.Incomplete || short.Size != 2*blk || !bytes.Equal(readArtifact(t, c, short), content[:2*blk]) {
		t.Errorf("short-chain artifact: incomplete=%v size=%d, want an incomplete 2-cluster prefix", short.Incomplete, short.Size)
	}
	d := short.Source.Derived
	if d == nil || len(d.Runs) == 0 || d.RunsArtifact != "" {
		t.Fatalf("the prefix runs of the truncated chain were not recorded: %+v", d)
	}
	// Provenance: the recorded runs, read back from the image, are exactly the
	// bytes the artifact captured (the prefix), and nothing more.
	var fromRuns []byte
	var covered int64
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Offset+r.Length > int64(len(data)) {
			t.Fatalf("run %+v is not inside the %d-byte image", r, len(data))
		}
		fromRuns = append(fromRuns, data[r.Offset:r.Offset+r.Length]...)
		covered += r.Length
	}
	if covered != short.Size || !bytes.Equal(fromRuns, readArtifact(t, c, short)) {
		t.Errorf("recorded runs cover %d bytes and do not reproduce the %d captured bytes", covered, short.Size)
	}
	if fine.Incomplete || string(readArtifact(t, c, fine)) != "fine" || len(fine.Source.Derived.Runs) == 0 {
		t.Errorf("the intact neighbour was affected: %+v", fine)
	}
	// One skip is the examine-side warning (partial read); runs were recorded ...
	if sum.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (read failed)", sum.Skipped)
	}
	// ... and the reader's own live warning about the chain is a filesystem warning.
	if sum.FSWarnings != 1 {
		t.Errorf("FSWarnings = %d, want 1 (the chain is shorter than the file)", sum.FSWarnings)
	}
	var reasons []string
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if r, _ := e.Details["reason"].(string); r != "" {
			reasons = append(reasons, r)
		}
	}
	joined := strings.Join(reasons, "\n")
	if strings.Contains(joined, "no runs recorded") || !strings.Contains(joined, "partial artifact kept, flagged incomplete") || !strings.Contains(joined, "chain shorter than file size") {
		t.Errorf("warnings do not explain the partial extraction: %q", reasons)
	}
	verifyOK(t, c)
}

// A 255-character CJK long name (765 bytes in UTF-8) is extracted under a
// capped local name and never aborts the run.
func TestExtractFATLongCJKNameNeverAborts(t *testing.T) {
	name := strings.Repeat("日", 251) + ".txt" // 255 UTF-16 units
	o := fattest.Options{Type: 16}
	img := fattest.Build(o, []fattest.File{
		{Path: name, Data: []byte("long name content"), LongName: true},
		{Path: "NEXT.TXT", Data: []byte("next")},
	})
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 2 || sum.Skipped != 0 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	for _, r := range sum.Artifacts {
		for _, comp := range strings.Split(r.Path, "/") {
			if len(comp) > 255 {
				t.Errorf("component of %d bytes in %q", len(comp), r.Path)
			}
		}
		want := "next"
		if r.Source.RemotePath == "/"+name {
			want = "long name content"
		}
		if string(readArtifact(t, c, r)) != want {
			t.Errorf("artifact %q has wrong content", r.Source.RemotePath)
		}
	}
	verifyOK(t, c)
}
