package examine_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// Two block groups of 64 blocks (1 KiB): group 0 holds data blocks 9..64, group 1 starts at block 65 and its data
// at block 73. A file that does not fit the rest of group 0 goes to group 1, and the root directory (allocated
// after the files) fills a gap of group 0, so an image cut inside group 1's data keeps the directory.
var ext4CutOpts = ext4test.Options{BlockSize: 1024, Extents: true, Groups: 2, BlocksPerGroup: 64, InodesPerGroup: 16}

const ext4CutGroup1Data = 73 // first data block of group 1

// A fragmented ext4 file whose later extents lie beyond the end of a truncated image is extracted as an incomplete
// artifact of the leading bytes the image holds; the runs of that prefix reproduce the captured bytes; the intact
// neighbour is complete; the filesystem's truncation warning is counted apart from the skip.
func TestExtractExt4TruncatedImageIsIncomplete(t *testing.T) {
	const bs = 1024
	content := pattern(80*bs, 21)
	var ps []ext4test.Piece
	for i := range 4 {
		ps = append(ps, ext4test.Piece{Block: 20 * i, Data: content[20*i*bs : 20*(i+1)*bs]})
	}
	img0 := ext4test.Build(ext4CutOpts, []ext4test.File{
		{Path: "/fine.bin", Data: []byte("fine")},
		{Path: "/frag.bin", Pieces: ps, Scatter: true},
	})
	// extents 1 and 2 are in group 0; extent 3 starts in group 1 and keeps its first block: 41 blocks stay mapped
	img := img0[:(ext4CutGroup1Data+1)*bs+10]
	s, data := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	frag, fine := sum.Artifacts[0], sum.Artifacts[1]
	if strings.HasSuffix(frag.Source.RemotePath, "fine.bin") {
		frag, fine = fine, frag
	}
	if !strings.HasSuffix(frag.Source.RemotePath, "frag.bin") || !strings.HasSuffix(fine.Source.RemotePath, "fine.bin") {
		t.Fatalf("artifacts = %q / %q", frag.Source.RemotePath, fine.Source.RemotePath)
	}
	if !frag.Incomplete || frag.Size != 41*bs || !bytes.Equal(readArtifact(t, c, frag), content[:41*bs]) {
		t.Errorf("truncated-file artifact: incomplete=%v size=%d, want an incomplete 41-block prefix", frag.Incomplete, frag.Size)
	}
	d := frag.Source.Derived
	if d == nil || len(d.Runs) == 0 || d.RunsArtifact != "" {
		t.Fatalf("the prefix runs were not recorded: %+v", d)
	}
	var fromRuns []byte
	var covered int64
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Offset+r.Length > int64(len(data)) {
			t.Fatalf("run %+v is not inside the %d-byte image", r, len(data))
		}
		fromRuns = append(fromRuns, data[r.Offset:r.Offset+r.Length]...)
		covered += r.Length
	}
	if covered != frag.Size || !bytes.Equal(fromRuns, readArtifact(t, c, frag)) {
		t.Errorf("recorded runs cover %d bytes and do not reproduce the %d captured bytes", covered, frag.Size)
	}
	if fine.Incomplete || string(readArtifact(t, c, fine)) != "fine" || len(fine.Source.Derived.Runs) == 0 {
		t.Errorf("the intact neighbour was affected: %+v", fine)
	}
	if sum.Skipped != 1 || sum.FSWarnings == 0 {
		t.Errorf("Skipped = %d (want 1: the partial read), FSWarnings = %d (want the truncation warnings)", sum.Skipped, sum.FSWarnings)
	}
	var reasons []string
	for _, w := range sum.Warnings {
		reasons = append(reasons, w.Reason)
	}
	if joined := strings.Join(reasons, "\n"); strings.Contains(joined, "no runs recorded") || !strings.Contains(joined, "partial artifact kept, flagged incomplete") {
		t.Errorf("warnings do not explain the partial extraction: %q", reasons)
	}
	verifyOK(t, c)
}

// A file whose first block lies beyond the end of the image has a size and no readable byte. It is never extracted
// as a complete file and never as zeros.
func TestExtractExt4CutAtFirstBlockIsNeverComplete(t *testing.T) {
	const bs = 1024
	img0 := ext4test.Build(ext4CutOpts, []ext4test.File{
		{Path: "/fill.bin", Data: pattern(54*bs, 22)}, // leaves two blocks in group 0: the next file goes to group 1
		{Path: "/gone.bin", Data: pattern(3*bs, 23)},
	})
	s, _ := imageSession(t, img0[:ext4CutGroup1Data*bs])
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	found := false
	for _, a := range sum.Artifacts {
		if strings.HasSuffix(a.Source.RemotePath, "gone.bin") {
			found = true
			if !a.Incomplete || a.Size != 0 {
				t.Errorf("artifact for the file cut at its first block: incomplete=%v size=%d, want an incomplete empty artifact", a.Incomplete, a.Size)
			}
		}
	}
	if !found || sum.Skipped != 1 || len(sum.Warnings) != 1 || !strings.Contains(sum.Warnings[0].Reason, "partial artifact kept, flagged incomplete") {
		t.Errorf("found=%v summary = %+v, want an incomplete empty artifact and one warning saying so", found, sum)
	}
	var warned bool
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if strings.Contains(string(mustJSON(t, e)), "gone.bin") {
			warned = true
		}
	}
	if !warned {
		t.Error("no analysis.warning names the file cut at its first block")
	}
	verifyOK(t, c)
}
