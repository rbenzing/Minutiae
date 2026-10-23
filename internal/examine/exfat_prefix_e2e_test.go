package examine_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// partialArtifact finds the artifact whose remote path ends with name.
func partialArtifact(t *testing.T, sum examine.Summary, name string) evidence.ManifestRecord {
	t.Helper()
	for _, a := range sum.Artifacts {
		if strings.HasSuffix(a.Source.RemotePath, name) {
			return a
		}
	}
	t.Fatalf("no artifact for %s in %d artifacts", name, len(sum.Artifacts))
	return evidence.ManifestRecord{}
}

// assertPrefixProvenance checks that the recorded runs, read back from the
// image, are exactly the bytes the artifact captured (the prefix) and nothing more.
func assertPrefixProvenance(t *testing.T, c *evidence.Case, rec evidence.ManifestRecord, image, content []byte) {
	t.Helper()
	got := readArtifact(t, c, rec)
	if !rec.Incomplete || rec.Size != int64(len(got)) || !bytes.Equal(got, content[:len(got)]) || len(got) == 0 || len(got) >= len(content) {
		t.Fatalf("artifact: incomplete=%v size=%d, want an incomplete strict prefix of %d bytes", rec.Incomplete, rec.Size, len(content))
	}
	d := rec.Source.Derived
	if d == nil || len(d.Runs) == 0 || d.RunsArtifact != "" {
		t.Fatalf("the prefix runs were not recorded: %+v", d)
	}
	var fromRuns []byte
	for _, r := range d.Runs {
		if r.Offset < 0 || r.Offset+r.Length > int64(len(image)) {
			t.Fatalf("run %+v is not inside the %d-byte image", r, len(image))
		}
		fromRuns = append(fromRuns, image[r.Offset:r.Offset+r.Length]...)
	}
	if !bytes.Equal(fromRuns, got) {
		t.Errorf("recorded runs (%d bytes) do not reproduce the %d captured bytes", len(fromRuns), len(got))
	}
}

// An exFAT file whose FAT chain is shorter than DataLength is extracted as an
// incomplete artifact of the trusted prefix, with its runs recorded; the
// neighbour is complete.
func TestExtractExfatShortChainIsIncomplete(t *testing.T) {
	content := pattern(4*exfatCS, 9)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/SHORT.BIN", Data: content, FatChain: true},
		{Path: "/FINE.BIN", Data: []byte("fine")},
	})
	cl := lay.Find("/SHORT.BIN", false).Clusters
	for i, b := range []byte{0xFF, 0xFF, 0xFF, 0xFF} {
		img[int(lay.FatEntryOffset(cl[1]))+i] = b // end the chain after two clusters
	}
	s, data := imageSession(t, img)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	short, fine := partialArtifact(t, sum, "SHORT.BIN"), partialArtifact(t, sum, "FINE.BIN")
	if short.Size != 2*exfatCS {
		t.Errorf("size = %d, want the 2-cluster prefix", short.Size)
	}
	assertPrefixProvenance(t, s.Case, short, data, content)
	if fine.Incomplete || string(readArtifact(t, s.Case, fine)) != "fine" {
		t.Errorf("the intact neighbour was affected: %+v", fine)
	}
	if sum.Skipped != 1 || sum.FSWarnings != 1 {
		t.Errorf("Skipped = %d, FSWarnings = %d, want 1 and 1", sum.Skipped, sum.FSWarnings)
	}
	verifyOK(t, s.Case)
}

// A truncated image cuts a fragmented file: the file before the cut is whole,
// the cut one is an incomplete prefix with its runs.
func TestExtractExfatTruncatedImageIsIncomplete(t *testing.T) {
	before, cutData := pattern(2*exfatCS, 3), pattern(4*exfatCS, 4)
	img, lay := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/BEFORE.BIN", Data: before},
		{Path: "/CUT.BIN", Data: cutData, Fragmented: true},
	})
	cl := lay.Find("/CUT.BIN", false).Clusters
	cut := img[:lay.ClusterOffset(cl[1])+exfatCS] // inside the fragmented file, after its second cluster
	s, data := imageSession(t, cut)
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	b, c := partialArtifact(t, sum, "BEFORE.BIN"), partialArtifact(t, sum, "CUT.BIN")
	if b.Incomplete || !bytes.Equal(readArtifact(t, s.Case, b), before) {
		t.Errorf("the file before the cut was affected: %+v", b)
	}
	if c.Size != 2*exfatCS {
		t.Errorf("size = %d, want the 2-cluster prefix", c.Size)
	}
	assertPrefixProvenance(t, s.Case, c, data, cutData)
	verifyOK(t, s.Case)
}
