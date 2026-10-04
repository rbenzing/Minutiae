package examine_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// f2fsNoFree is the real F2FS reader plus a stub Unallocated (that part of the
// driver is a separate piece of work); everything else, including the directory
// reader, is the real one.
type f2fsNoFree struct{ *f2fs.FS }

func (f2fsNoFree) Unallocated() ([]filesys.Run, error) { return nil, nil }

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
	s, err := examine.Open(c, recs[0].ID, examine.Options{Drivers: []detect.Driver{{
		Name: "f2fs", Probe: f2fs.Probe,
		Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := f2fs.Open(r, size)
			if err != nil {
				return nil, err
			}
			return f2fsNoFree{fsys}, nil
		},
	}}})
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
