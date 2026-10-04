package examine_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// A file whose block map breaks part-way (a data address outside the main
// area) is extracted as a partial artifact flagged incomplete: the runs of the
// trusted prefix are recorded and reproduce exactly the captured bytes, the
// intact neighbour is unaffected, and the filesystem's own warning is counted
// separately from the skip.
func TestExtractF2FSBrokenChainIsIncomplete(t *testing.T) {
	const bs = f2fstest.BlockSize
	o := f2fstest.Options{Segments: 1}
	content := pattern(6*bs, 3)
	img, tree := f2fstest.BuildTree(o, []f2fstest.File{
		{Path: "/BROKEN.BIN", Data: content},
		{Path: "/FINE.TXT", Data: []byte("fine"), Inline: true},
	})
	// i_addr[3] of BROKEN.BIN: block 1, below the main area.
	binary.LittleEndian.PutUint32(img[int(tree.Addr[tree.NID["/BROKEN.BIN"]])*bs+360+3*4:], 1)

	c := newCase(t)
	disc := disk(img)
	recs := importImage(t, c, disc, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // the default registry: the real f2fs driver
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})

	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	broken, fine := sum.Artifacts[0], sum.Artifacts[1]
	if strings.HasSuffix(broken.Source.RemotePath, "FINE.TXT") {
		broken, fine = fine, broken
	}
	if !strings.HasSuffix(broken.Source.RemotePath, "BROKEN.BIN") {
		t.Fatalf("artifacts = %q / %q", broken.Source.RemotePath, fine.Source.RemotePath)
	}
	if !strings.Contains(broken.Path, "/p1-f2fs/") {
		t.Errorf("path %q does not name the filesystem type", broken.Path)
	}
	if !broken.Incomplete || broken.Size != 3*bs || !bytes.Equal(readArtifact(t, c, broken), content[:3*bs]) {
		t.Errorf("broken artifact: incomplete=%v size=%d, want an incomplete 3-block prefix", broken.Incomplete, broken.Size)
	}
	d := broken.Source.Derived
	if d == nil || len(d.Runs) == 0 || d.RunsArtifact != "" {
		t.Fatalf("the prefix runs were not recorded: %+v", d)
	}
	var fromRuns []byte
	var covered int64
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Offset+r.Length > int64(len(disc)) {
			t.Fatalf("run %+v is not inside the %d-byte image", r, len(disc))
		}
		fromRuns = append(fromRuns, disc[r.Offset:r.Offset+r.Length]...)
		covered += r.Length
	}
	if covered != broken.Size || !bytes.Equal(fromRuns, readArtifact(t, c, broken)) {
		t.Errorf("recorded runs cover %d bytes and do not reproduce the %d captured bytes", covered, broken.Size)
	}
	if fine.Incomplete || string(readArtifact(t, c, fine)) != "fine" {
		t.Errorf("the intact neighbour was affected: %+v", fine)
	}
	if sum.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (read failed)", sum.Skipped)
	}
	if sum.FSWarnings != 1 {
		t.Errorf("FSWarnings = %d, want 1 (the block map is broken)", sum.FSWarnings)
	}
	var reasons []string
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if r, _ := e.Details["reason"].(string); r != "" {
			reasons = append(reasons, r)
		}
	}
	joined := strings.Join(reasons, "\n")
	if strings.Contains(joined, "no runs recorded") || !strings.Contains(joined, "partial artifact kept, flagged incomplete") {
		t.Errorf("warnings do not explain the partial extraction: %q", reasons)
	}
	verifyOK(t, c)
}

// TestInfoDetectsF2FSPartition runs the default driver registry over a GPT
// image whose partition holds an F2FS filesystem.
func TestInfoDetectsF2FSPartition(t *testing.T) {
	c := newCase(t)
	fsImg := f2fstest.Build(f2fstest.Options{Segments: 1, Label: "userdata"},
		[]f2fstest.File{{Path: "/hello.txt", Data: []byte("hello"), Inline: true}})
	recs := importImage(t, c, disk(fsImg), 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // nil Drivers = detect.Drivers
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	info := s.Info()
	if len(info.Partitions) != 1 {
		t.Fatalf("partitions = %d, want 1", len(info.Partitions))
	}
	p := info.Partitions[0]
	if p.FSType != "f2fs" || p.FSInfo == nil || p.FSInfo.Type != "f2fs" || p.FSInfo.Label != "userdata" || p.Error != "" {
		t.Errorf("partition = %+v (FSInfo %+v), want an f2fs filesystem labelled userdata", p, p.FSInfo)
	}
}
