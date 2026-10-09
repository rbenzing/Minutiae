package records_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// offsetRec is a message record with a byte range (nil = none) on art.
func offsetRec(art string, rng *records.Range) records.Record {
	return records.Record{Type: "message", ArtifactID: art, Summary: "r", Payload: map[string]any{"k": "v"}, Range: rng}
}

func offsetProv(t *testing.T, c *evidence.Case, art evidence.ManifestRecord, rng *records.Range) records.Provenance {
	t.Helper()
	return getOne(t, c, []string{art.ID}, offsetRec(art.ID, rng)).Provenance
}

func extractSrc(parent evidence.ManifestRecord, d evidence.Derivation) evidence.Source {
	d.ParentID, d.ParentSHA256 = parent.ID, parent.SHA256
	d.Partition, d.FSType, d.FSPath, d.FSID = 1, "mtfs", "/f", "nid:5"
	return evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &d}
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251 + 7)
	}
	return b
}

func requireTranslated(t *testing.T, p records.Provenance) {
	t.Helper()
	if p.Offset.State != records.OffsetTranslated || p.Offset.Reason != "" || p.Offset.Coordinates != "media" {
		t.Fatalf("offset %+v problems %+v", p.Offset, p.Problems)
	}
}

func requireUnavailable(t *testing.T, p records.Provenance, reasonPart string) {
	t.Helper()
	if p.Offset.State != records.OffsetUnavailable || p.Offset.Reason == "" || !strings.Contains(p.Offset.Reason, reasonPart) || len(p.Offset.Image) != 0 {
		t.Fatalf("offset %+v, want unavailable with %q and no image extents", p.Offset, reasonPart)
	}
}

func hasNote(p records.Provenance, part string) bool {
	for _, n := range p.Notes {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func TestOffsetNoRange(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(4096))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 8}}}), []byte("abcdefgh"))
	p := offsetProv(t, c, f, nil)
	if p.Offset.State != records.OffsetNoRange || p.Offset.Coordinates != "media" || len(p.Offset.Image) != 0 {
		t.Fatalf("%+v", p.Offset)
	}
}

func TestOffsetNotDerived(t *testing.T) {
	c, art := setup(t)
	p := offsetProv(t, c, art, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, "artifact is not derived")
	if !p.OK() || !hasNote(p, "artifact is not derived: no image offset") {
		t.Fatalf("%+v notes %q", p.Problems, p.Notes)
	}
}

func TestOffsetExtractedFileSingleRun(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(8192))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 4096, Length: 100}}}), patterned(100))
	p := offsetProv(t, c, f, &records.Range{Offset: 10, Length: 20})
	requireTranslated(t, p)
	want := records.ImageExtent{ArtifactOffset: 10, Length: 20, ImageOffset: 4106}
	if len(p.Offset.Image) != 1 || p.Offset.Image[0] != want {
		t.Fatalf("image %+v", p.Offset.Image)
	}
	if len(p.Offset.Hops) != 1 || p.Offset.Hops[0].From != f.ID || p.Offset.Hops[0].To != img.ID || p.Offset.Hops[0].Total != 1 {
		t.Fatalf("hops %+v", p.Offset.Hops)
	}
	if !p.OK() {
		t.Fatalf("%+v", p.Problems)
	}
}

// 4096 runs are stored inline, 4097 in a sidecar (MaxInlineRuns); a range across the last runs
// translates to one extent per run either way.
func TestOffsetFragmentedFileSpansRuns(t *testing.T) {
	for _, n := range []int{3, evidence.MaxInlineRuns, evidence.MaxInlineRuns + 1} {
		c := recordstest.NewCase(t)
		img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
		runs := make([]evidence.Run, n)
		for i := range runs {
			runs[i] = evidence.Run{Offset: int64(2 * i), Length: 1} // never adjacent
		}
		d := evidence.Derivation{}
		if n <= evidence.MaxInlineRuns {
			d.Runs = runs
		} else {
			d.RunsArtifact = recordstest.AddRunsSidecar(t, c, img, "f.bin.runs.jsonl", runs).ID
		}
		f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, d), patterned(n))
		p := offsetProv(t, c, f, &records.Range{Offset: int64(n - 3), Length: 3})
		requireTranslated(t, p)
		if len(p.Offset.Image) != 3 {
			t.Fatalf("n=%d image %+v", n, p.Offset.Image)
		}
		for k, e := range p.Offset.Image {
			if e.ImageOffset != int64(2*(n-3+k)) || e.Length != 1 || e.ArtifactOffset != int64(n-3+k) {
				t.Fatalf("n=%d extent %d: %+v", n, k, e)
			}
		}
		if !p.OK() {
			t.Fatalf("n=%d %+v", n, p.Problems)
		}
	}
}

// The independent oracle: the image bytes at ImageOffset equal the artifact bytes at ArtifactOffset.
func TestOffsetReproducesBytes(t *testing.T) {
	c := recordstest.NewCase(t)
	imgBytes := patterned(1 << 20)
	img := recordstest.AddArtifact(t, c, "img.bin", imgBytes)
	runs := []evidence.Run{{Offset: 300000, Length: 1000}, {Offset: 5000, Length: 2000}, {Offset: 700000, Length: 500}}
	var data []byte
	for _, r := range runs {
		data = append(data, imgBytes[r.Offset:r.Offset+r.Length]...)
	}
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: runs}), data)
	p := offsetProv(t, c, f, &records.Range{Offset: 500, Length: 2000})
	requireTranslated(t, p)
	if len(p.Offset.Image) != 2 {
		t.Fatalf("%+v", p.Offset.Image)
	}
	var total int64
	for _, e := range p.Offset.Image {
		if e.Hole {
			t.Fatalf("hole %+v", e)
		}
		if !bytes.Equal(imgBytes[e.ImageOffset:e.ImageOffset+e.Length], data[e.ArtifactOffset:e.ArtifactOffset+e.Length]) {
			t.Fatalf("extent %+v does not reproduce the artifact bytes", e)
		}
		total += e.Length
	}
	if total != 2000 {
		t.Fatalf("total %d", total)
	}
}

func TestOffsetTwoHopsThroughUnallocatedExport(t *testing.T) {
	c := recordstest.NewCase(t)
	imgBytes := patterned(1 << 16)
	img := recordstest.AddArtifact(t, c, "img.bin", imgBytes)
	lines := []evidence.UnallocRun{{Offset: 0, Length: 100, ImageOffset: 5000}, {Offset: 100, Length: 50, ImageOffset: 9000}}
	var bin []byte
	bin = append(bin, imgBytes[5000:5100]...)
	bin = append(bin, imgBytes[9000:9050]...)
	export, _ := recordstest.AddUnallocatedExport(t, c, img, lines, bin)
	// the carved file: bin[20:80), a hole of 10, bin[120:150)
	carvedRuns := []evidence.Run{{Offset: 20, Length: 60}, {Offset: -1, Length: 10}, {Offset: 120, Length: 30}}
	var data []byte
	data = append(data, bin[20:80]...)
	data = append(data, make([]byte, 10)...)
	data = append(data, bin[120:150]...)
	f := recordstest.AddDerivedWith(t, c, "carved.bin", extractSrc(export, evidence.Derivation{Runs: carvedRuns}), data)

	p := offsetProv(t, c, f, &records.Range{Offset: 50, Length: 30})
	requireTranslated(t, p)
	want := []records.ImageExtent{
		{ArtifactOffset: 50, Length: 10, ImageOffset: 5070},
		{ArtifactOffset: 60, Length: 10, ImageOffset: -1, Hole: true},
		{ArtifactOffset: 70, Length: 10, ImageOffset: 9020},
	}
	if len(p.Offset.Image) != len(want) {
		t.Fatalf("image %+v", p.Offset.Image)
	}
	for i := range want {
		if p.Offset.Image[i] != want[i] {
			t.Fatalf("extent %d: %+v, want %+v", i, p.Offset.Image[i], want[i])
		}
	}
	if len(p.Offset.Hops) != 2 || p.Offset.Hops[0].From != f.ID || p.Offset.Hops[1].From != export.ID || p.Offset.Hops[1].To != img.ID {
		t.Fatalf("hops %+v", p.Offset.Hops)
	}
	for _, e := range p.Offset.Image {
		if !e.Hole && !bytes.Equal(imgBytes[e.ImageOffset:e.ImageOffset+e.Length], data[e.ArtifactOffset:e.ArtifactOffset+e.Length]) {
			t.Fatalf("extent %+v does not reproduce the bytes", e)
		}
	}
	// a range wholly inside the hole stays a hole
	ph := offsetProvAgain(t, c, f, &records.Range{Offset: 61, Length: 5})
	requireTranslated(t, ph)
	if len(ph.Offset.Image) != 1 || !ph.Offset.Image[0].Hole || ph.Offset.Image[0].Length != 5 {
		t.Fatalf("hole %+v", ph.Offset.Image)
	}
}

// The export's sidecar written in the plain {offset, length} format is refused.
func TestOffsetUnallocatedSidecarInWrongFormatIsRefused(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	plain := recordstest.AddRunsSidecar(t, c, img, "unallocated.runs.jsonl", []evidence.Run{{Offset: 0, Length: 64}})
	src := evidence.Source{Kind: "unallocated", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", RunsArtifact: plain.ID,
	}}
	bin := recordstest.AddDerivedWith(t, c, "unallocated.bin", src, patterned(64))
	p := offsetProv(t, c, bin, &records.Range{Offset: 0, Length: 8})
	requireUnavailable(t, p, "")
	if k := problemKinds(p); k[records.ProblemRunsSidecar] != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestOffsetNoRunsIsNoteNotProblem(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(4096))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{}), []byte("decoded"))
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, "the derived artifact records no runs (content decoded or runs not recorded)")
	if !p.OK() || !hasNote(p, "records no runs") {
		t.Fatalf("%+v notes %q", p.Problems, p.Notes)
	}
}

func sidecarCase(t *testing.T) (c *evidence.Case, f, sc evidence.ManifestRecord) {
	t.Helper()
	c = recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	sc = recordstest.AddRunsSidecar(t, c, img, "f.bin.runs.jsonl", []evidence.Run{{Offset: 100, Length: 8}})
	f = recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{RunsArtifact: sc.ID}), []byte("abcdefgh"))
	return c, f, sc
}

func requireSidecarProblem(t *testing.T, p records.Provenance) {
	t.Helper()
	requireUnavailable(t, p, "")
	if k := problemKinds(p); k[records.ProblemRunsSidecar] != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestOffsetSidecarWorks(t *testing.T) {
	c, f, _ := sidecarCase(t)
	p := offsetProv(t, c, f, &records.Range{Offset: 2, Length: 4})
	requireTranslated(t, p)
	if len(p.Offset.Image) != 1 || p.Offset.Image[0].ImageOffset != 102 {
		t.Fatalf("%+v", p.Offset.Image)
	}
}

func TestOffsetSidecarTamperedRefused(t *testing.T) {
	c, f, sc := sidecarCase(t)
	orig, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(sc.Path)))
	if err != nil {
		t.Fatal(err)
	}
	// a same-size swap: offset 900 instead of 100
	swapped := bytes.Replace(orig, []byte("100"), []byte("900"), 1)
	if len(swapped) != len(orig) || bytes.Equal(swapped, orig) {
		t.Fatalf("test setup: %q", orig)
	}
	recordstest.SetArtifactFileBytes(t, c.Dir, sc.ID, swapped)
	requireSidecarProblem(t, offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4}))
}

func TestOffsetSidecarMissingRefused(t *testing.T) {
	c, f, sc := sidecarCase(t)
	recordstest.RemoveArtifactEverywhere(t, c.Dir, sc.ID)
	requireSidecarProblem(t, offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4}))
}

func TestOffsetSidecarOfOtherParentRefused(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	other := recordstest.AddArtifact(t, c, "other.bin", patterned(4096))
	sc := recordstest.AddRunsSidecar(t, c, other, "f.bin.runs.jsonl", []evidence.Run{{Offset: 100, Length: 8}})
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{RunsArtifact: sc.ID}), []byte("abcdefgh"))
	requireSidecarProblem(t, offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4}))
}

// A complete artifact whose runs do not add up to its size: Get and case verify (R4) agree.
func TestOffsetRunsSumDisagreesRefused(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	rv := &evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(60),
		Basis: []string{"dentry:1:1:1"}, Alloc: evidence.AllocSummary{Free: 8}, Algorithm: evidence.AlgorithmRecover,
	}
	src := evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/a", FSID: "dentry:1:1:1",
		Runs: []evidence.Run{{Offset: 0, Length: 8}}, Recovery: rv,
	}}
	f := recordstest.AddDerivedWith(t, c, "recovered/p1-mtfs/000001-a.bin", src, []byte("abcd"))
	r := recoveredRec(f.ID, ip(50))
	r.Range = &records.Range{Offset: 0, Length: 2}
	p := getOne(t, c, []string{f.ID}, r).Provenance
	requireUnavailable(t, p, "runs cover 8 bytes but the artifact holds 4")
	if k := problemKinds(p); k[records.ProblemRunsInconsistent] != 1 {
		t.Fatalf("%+v", p.Problems)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	requireProblem(t, rep, "runs cover 8 bytes but the artifact holds 4")
}

// A range beyond the artifact: Get and case verify (P9) agree.
func TestOffsetRangeBeyondArtifactRefused(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 8}}}), []byte("abcdefgh"))
	p := tamperedRangeProv(t, c, f, 4, 20)
	requireUnavailable(t, p, "outside the artifact")
	if k := problemKinds(p); k[records.ProblemRangeOutside] != 1 || k[records.ProblemRunsInconsistent] != 0 {
		t.Fatalf("%+v", p.Problems)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	requireProblem(t, rep, "range beyond artifact")
}

// incompleteDerived stores data as an incomplete (cut off) derived artifact.
func incompleteDerived(t *testing.T, c *evidence.Case, name string, src evidence.Source, data []byte) evidence.ManifestRecord {
	t.Helper()
	rec, err := c.Capture("dev1", "acq-derived", name, src, func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		return errors.New("test: copy cut off")
	})
	if err == nil || !rec.Incomplete {
		t.Fatalf("record %+v err %v", rec, err)
	}
	return rec
}

// E9: a cancelled copy keeps the full 1000-byte run list but holds 100 bytes.
func TestOffsetIncompleteCancelledCopy(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := incompleteDerived(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 2000, Length: 1000}}}), patterned(100))
	p := offsetProv(t, c, f, &records.Range{Offset: 10, Length: 20})
	requireTranslated(t, p)
	if len(p.Offset.Image) != 1 || p.Offset.Image[0].ImageOffset != 2010 {
		t.Fatalf("%+v", p.Offset.Image)
	}
	if len(p.Offset.Hops) != 1 || !p.Offset.Hops[0].RunsExceedSize || !hasNote(p, "runs describe more than the bytes held (incomplete)") {
		t.Fatalf("hops %+v notes %q", p.Offset.Hops, p.Notes)
	}
	if k := problemKinds(p); k[records.ProblemRunsInconsistent] != 0 {
		t.Fatalf("%+v", p.Problems)
	}
	// a range that reaches past the bytes held
	pp := tamperedRangeProv(t, c, f, 90, 50)
	requireUnavailable(t, pp, "outside the artifact")
	if k := problemKinds(pp); k[records.ProblemRangeOutside] != 1 || k[records.ProblemRunsInconsistent] != 0 {
		t.Fatalf("%+v", pp.Problems)
	}
}

func TestOffsetIncompletePartialUnallocated(t *testing.T) {
	c := recordstest.NewCase(t)
	imgBytes := patterned(1 << 14)
	img := recordstest.AddArtifact(t, c, "img.bin", imgBytes)
	// the run map lists 1000 bytes, the partial unallocated.bin holds 100
	lines := []evidence.UnallocRun{{Offset: 0, Length: 600, ImageOffset: 2000}, {Offset: 600, Length: 400, ImageOffset: 8000}}
	_, sc := recordstest.AddUnallocatedExport(t, c, img, lines, imgBytes[2000:2100])
	src := evidence.Source{Kind: "unallocated", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", RunsArtifact: sc.ID,
	}}
	part := incompleteDerived(t, c, "unallocated.partial.bin", src, imgBytes[2000:2100])
	p := offsetProv(t, c, part, &records.Range{Offset: 10, Length: 20})
	requireTranslated(t, p)
	if len(p.Offset.Image) != 1 || p.Offset.Image[0].ImageOffset != 2010 {
		t.Fatalf("%+v %+v", p.Offset.Image, p.Problems)
	}
	if k := problemKinds(p); k[records.ProblemRunsInconsistent] != 0 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestOffsetIncompleteChunkPrefix(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := incompleteDerived(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 2000, Length: 100}}}), patterned(100))
	p := offsetProv(t, c, f, &records.Range{Offset: 90, Length: 10})
	requireTranslated(t, p)
	if p.Offset.Hops[0].RunsExceedSize || hasNote(p, "more than the bytes held") {
		t.Fatalf("hops %+v notes %q", p.Offset.Hops, p.Notes)
	}
}

func TestOffsetCompleteArtifactWithLongerRunsStaysAProblem(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 2000, Length: 1000}}}), patterned(100))
	p := offsetProv(t, c, f, &records.Range{Offset: 10, Length: 20})
	requireUnavailable(t, p, "runs cover 1000 bytes but the artifact has 100")
	if k := problemKinds(p); k[records.ProblemRunsInconsistent] != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

func TestOffsetStopsAtBrokenChain(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	mid := recordstest.AddDerivedWith(t, c, "mid.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 100, Length: 64}}}), patterned(64))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(mid, evidence.Derivation{Runs: []evidence.Run{{Offset: 8, Length: 16}}}), patterned(16))
	recordstest.RemoveArtifactEverywhere(t, c.Dir, img.ID)
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, mid.ID)
	if len(p.Offset.Hops) == 0 || p.Offset.Hops[0].From != f.ID {
		t.Fatalf("hops so far are kept: %+v", p.Offset.Hops)
	}
}

func TestOffsetParentHashDiffersIsNeverTranslated(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 100, Length: 8}}}), []byte("abcdefgh"))
	recordstest.SetManifestSource(t, c.Dir, f.ID, func(s *evidence.Source) { s.Derived.ParentSHA256 = strings.Repeat("0", 64) })
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, "parent hash differs; offsets refer to unproven bytes")
	if len(p.Offset.Hops) != 1 || len(p.Offset.Hops[0].Extents) != 1 {
		t.Fatalf("hop extents are kept: %+v", p.Offset.Hops)
	}
}

func TestOffsetBadSegmentsAreNeverTranslated(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	d := evidence.Derivation{
		Runs:           []evidence.Run{{Offset: 100, Length: 8}},
		ParentSegments: []evidence.SegmentRef{{ID: img.ID, SHA256: strings.Repeat("0", 64)}},
	}
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, d), []byte("abcdefgh"))
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 4})
	requireUnavailable(t, p, "parent hash differs; offsets refer to unproven bytes")
}

// A truncated LAST hop (more than 256 fragments) is translated and says so; a truncated hop that
// must be translated on stops explicitly.
func TestOffsetTruncatedLastHopIsTranslatedAndSaysSo(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	runs := make([]evidence.Run, 300)
	for i := range runs {
		runs[i] = evidence.Run{Offset: int64(2 * i), Length: 1}
	}
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(img, evidence.Derivation{Runs: runs}), patterned(300))
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 300})
	requireTranslated(t, p)
	if len(p.Offset.Image) != records.MaxExtentsPerHop || len(p.Offset.Hops) != 1 ||
		!p.Offset.Hops[0].Truncated || p.Offset.Hops[0].Total != 300 {
		t.Fatalf("image %d hops %+v", len(p.Offset.Image), p.Offset.Hops)
	}
}

func TestOffsetTruncatedMidChainStopsExplicitly(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	mid := recordstest.AddDerivedWith(t, c, "mid.bin", extractSrc(img, evidence.Derivation{Runs: []evidence.Run{{Offset: 0, Length: 1024}}}), patterned(1024))
	runs := make([]evidence.Run, 300)
	for i := range runs {
		runs[i] = evidence.Run{Offset: int64(2 * i), Length: 1}
	}
	f := recordstest.AddDerivedWith(t, c, "f.bin", extractSrc(mid, evidence.Derivation{Runs: runs}), patterned(300))
	p := offsetProv(t, c, f, &records.Range{Offset: 0, Length: 300})
	requireUnavailable(t, p, "more than 256 fragments at "+f.ID+"; translation stopped")
	if len(p.Offset.Hops) != 1 || !p.Offset.Hops[0].Truncated {
		t.Fatalf("hops %+v", p.Offset.Hops)
	}
}

// A runs sidecar that is the declared list (not the captured runs) is never translated.
func TestOffsetRunsArtifactEqualToDeclaredListStopsTranslation(t *testing.T) {
	const dir = "recovered/p1-mtfs/"
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "img.bin", patterned(1<<14))
	s := recordstest.AddRunsSidecar(t, c, img, dir+"000001-a.bin.runs.jsonl", []evidence.Run{{Offset: 0, Length: 8}})
	rv := &evidence.Recovery{
		Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(60),
		Basis: []string{"dentry:1:1:1"}, Alloc: evidence.AllocSummary{Free: 8}, Algorithm: evidence.AlgorithmRecover,
		DeclaredRunsArtifact: s.ID,
	}
	src := evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/a", FSID: "dentry:1:1:1",
		RunsArtifact: s.ID, Recovery: rv,
	}}
	f := recordstest.AddDerivedWith(t, c, dir+"000001-a.bin", src, []byte("abcdefgh"))
	r := recoveredRec(f.ID, ip(50))
	r.Range = &records.Range{Offset: 0, Length: 4}
	p := getOne(t, c, []string{f.ID}, r).Provenance
	requireUnavailable(t, p, "runs artifact is the declared list, not captured runs")
	if k := problemKinds(p); k[records.ProblemDeclaredRuns] != 1 {
		t.Fatalf("%+v", p.Problems)
	}
}

// tamperedRangeProv ingests a valid record on art and then rewrites its stored byte range, which the
// writer would refuse (case verify P9 reports the same row).
func tamperedRangeProv(t *testing.T, c *evidence.Case, art evidence.ManifestRecord, off, length int64) records.Provenance {
	t.Helper()
	res := recordstest.Ingest(t, c, secondParser, []string{art.ID}, []records.Record{offsetRec(art.ID, &records.Range{Offset: 0, Length: 1})})
	recordstest.SetRecordColumns(t, c.Dir, res.FirstID, map[string]any{"src_offset": off, "src_length": length})
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	return full.Provenance
}

var secondParser = records.Parser{Name: "sms-parser-2", Version: "1.0", Hash: "abc123"}

// offsetProvAgain is offsetProv for an artifact an earlier ingest already covered.
func offsetProvAgain(t *testing.T, c *evidence.Case, art evidence.ManifestRecord, rng *records.Range) records.Provenance {
	t.Helper()
	res := recordstest.Ingest(t, c, secondParser, []string{art.ID}, []records.Record{offsetRec(art.ID, rng)})
	full, err := newReader(t, c).Get(ctx, res.FirstID)
	if err != nil {
		t.Fatal(err)
	}
	return full.Provenance
}
