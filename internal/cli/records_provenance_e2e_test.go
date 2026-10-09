package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/apfs/apfstest"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// patternBytes is n bytes with no zero byte, so an image offset that is off cannot match by luck.
func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131+i/251+i/65521*17) | 1
	}
	return b
}

// provImg is a case holding a built image imported as segs raw segment files. raw is the image read
// back from the segment files on disk: the independent oracle for image offsets.
type provImg struct {
	e   *imgEnv
	raw []byte
	src string // the directory holding the import source files, outside the case
}

func newProvImg(t *testing.T, segs int, nodes ...fstest.Node) *provImg {
	t.Helper()
	disk := imgDisk(append(defaultNodes(), nodes...)...)
	dir := t.TempDir()
	var paths []string
	per := (len(disk) + segs - 1) / segs
	for i := 0; i < segs; i++ {
		p := filepath.Join(dir, "disk.00"+strconv.Itoa(i+1))
		if err := os.WriteFile(p, disk[i*per:min(len(disk), (i+1)*per)], 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	e := &imgEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t)}
	args := append([]string{"image", "import", "--case", e.c, "--json"}, paths...)
	code, out := run(t, e.d, args...)
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); code != 0 || err != nil || len(recs) != segs {
		t.Fatalf("image import: %d %s (%v)", code, out, err)
	}
	e.rec, e.ref = recs[0], recs[0].ID
	var raw []byte
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // a test reading the file it wrote
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
	}
	return &provImg{e: e, raw: raw, src: dir}
}

// extractedRecord runs the real `image extract` and returns the manifest record of the new artifact.
func extractedRecord(t *testing.T, e *imgEnv, fsPath string) evidence.ManifestRecord {
	t.Helper()
	if code, out := e.image(t, "extract", e.ref, fsPath); code != 0 {
		t.Fatalf("image extract %s: %d\n%s", fsPath, code, out)
	}
	return lastDerived(t, e.c, "extract", fsPath)
}

func lastDerived(t *testing.T, caseDir, kind, fsPath string) evidence.ManifestRecord {
	t.Helper()
	recs, err := manifest(caseDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if d := recs[i].Source.Derived; recs[i].Source.Kind == kind && d != nil && (fsPath == "" || d.FSPath == fsPath) {
			return recs[i]
		}
	}
	t.Fatalf("no %s artifact for %q in the manifest", kind, fsPath)
	return evidence.ManifestRecord{}
}

func artifactFile(t *testing.T, caseDir string, rec evidence.ManifestRecord) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(caseDir, filepath.FromSlash(rec.Path))) //nolint:gosec // a test reading its own temp case
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ingestNote stores one note on art with the range and returns its record id.
func ingestNote(t *testing.T, caseDir string, art evidence.ManifestRecord, rng *records.Range) string {
	t.Helper()
	c, err := evidence.Open(caseDir)
	if err != nil {
		t.Fatal(err)
	}
	res := recordstest.Ingest(t, c, recParser, []string{art.ID}, []records.Record{{
		Type: "note", ArtifactID: art.ID, Summary: "e2e note", Payload: map[string]any{}, Range: rng,
	}})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(res.FirstID, 10)
}

// requireOracle checks that every shown extent of [off, off+length) of the artifact reproduces the
// artifact's own bytes when read from the original image, that the extents tile the range in order,
// and returns how many extents there were.
func requireOracle(t *testing.T, p provTestJSON, art, media []byte, off, length int64) int {
	t.Helper()
	if p.Offset.State != "translated" || p.Offset.Coordinates != "media" || p.Offset.Reason != "" {
		t.Fatalf("offset %+v problems %+v", p.Offset, p.Problems)
	}
	pos, total := off, int64(0)
	for _, e := range p.Offset.Image {
		if e.Hole {
			t.Fatalf("unexpected hole %+v", e)
		}
		if e.ArtifactOffset != pos || e.Length <= 0 {
			t.Fatalf("extent %+v does not continue at artifact offset %d", e, pos)
		}
		if !bytes.Equal(media[e.ImageOffset:e.ImageOffset+e.Length], art[e.ArtifactOffset:e.ArtifactOffset+e.Length]) {
			t.Fatalf("extent %+v: the image bytes there are not the artifact bytes", e)
		}
		pos += e.Length
		total += e.Length
	}
	if total != length {
		t.Fatalf("extents cover %d bytes of the %d asked: %+v", total, length, p.Offset.Image)
	}
	return len(p.Offset.Image)
}

func showClean(t *testing.T, dir, id string) provTestJSON {
	t.Helper()
	p, code, _, stderr := showProv(t, dir, id)
	if code != 0 || stderr != "" || len(p.Problems) != 0 {
		t.Fatalf("show %s: exit %d stderr %q problems %+v", id, code, stderr, p.Problems)
	}
	return p
}

func verifyClean(t *testing.T, dir string) {
	t.Helper()
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
}

func TestRecordsShowOffsetMatchesImageBytes(t *testing.T) {
	frag := fstest.Node{Path: "/frag.bin", Data: patternBytes(40 * 512), Fragments: 5}
	t.Run("fragmented raw", func(t *testing.T) {
		pi := newProvImg(t, 1, frag)
		art := extractedRecord(t, pi.e, "/frag.bin")
		if d := art.Source.Derived; len(d.Runs) < 5 || d.RunsArtifact != "" {
			t.Fatalf("derivation %+v: want >= 5 inline runs", d)
		}
		held := artifactFile(t, pi.e.c, art)
		if !bytes.Equal(held, frag.Data) {
			t.Fatal("the extracted artifact is not the file")
		}
		id := ingestNote(t, pi.e.c, art, &records.Range{Offset: 3000, Length: 9000})
		p := showClean(t, pi.e.c, id)
		if n := requireOracle(t, p, held, pi.raw, 3000, 9000); n < 3 {
			t.Errorf("a range over a 5-fragment file shows %d extents", n)
		}
		if p.Offset.Segments != 1 {
			t.Errorf("segments %d, want 1", p.Offset.Segments)
		}
		verifyClean(t, pi.e.c)
	})
	t.Run("split raw, 3 segments", func(t *testing.T) {
		pi := newProvImg(t, 3, frag)
		art := extractedRecord(t, pi.e, "/frag.bin")
		held := artifactFile(t, pi.e.c, art)
		id := ingestNote(t, pi.e.c, art, &records.Range{Offset: 100, Length: 15000})
		p := showClean(t, pi.e.c, id)
		requireOracle(t, p, held, pi.raw, 100, 15000) // pi.raw is the three files joined: logical offsets
		if p.Offset.Segments != 3 {
			t.Errorf("segments %d, want 3 (the offsets are logical across the segment files)", p.Offset.Segments)
		}
		verifyClean(t, pi.e.c)
	})
	t.Run("past MaxInlineRuns fragments (sidecar)", func(t *testing.T) {
		n := evidence.MaxInlineRuns + 1
		big := fstest.Node{Path: "/big.bin", Data: patternBytes(n * 512), Fragments: n}
		pi := newProvImg(t, 1, big)
		art := extractedRecord(t, pi.e, "/big.bin")
		if d := art.Source.Derived; d.RunsArtifact == "" || len(d.Runs) != 0 {
			t.Fatalf("derivation %+v: want a runs sidecar", d)
		}
		held := artifactFile(t, pi.e.c, art)
		id := ingestNote(t, pi.e.c, art, &records.Range{Offset: 70 * 512, Length: 40 * 512})
		p := showClean(t, pi.e.c, id)
		if n := requireOracle(t, p, held, pi.raw, 70*512, 40*512); n != 40 {
			t.Errorf("%d extents, want 40 (one per fragment)", n)
		}
		if len(p.Offset.Hops) != 1 || !strings.HasPrefix(p.Offset.Hops[0].Runs, "sidecar ") {
			t.Errorf("hops %+v", p.Offset.Hops)
		}
		verifyClean(t, pi.e.c)
	})
}

// newEWFProv builds a 3-segment E01 around the MTFS disk of nodes, imports it, and returns the case,
// the media the container was built from and its import records.
func newEWFProv(t *testing.T, nodes ...fstest.Node) (e *imgEnv, media []byte, recs []evidence.ManifestRecord) {
	t.Helper()
	media = imgDisk(append(defaultNodes(), nodes...)...)
	n := (len(media) + ewfChunkBytes - 1) / ewfChunkBytes
	n = (n + 2) / 3 * 3
	media = append(media, make([]byte, n*ewfChunkBytes-len(media))...)
	segs := ewftest.Build(ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: n / 3, Compress: ewftest.CompressMixed}, media)
	if len(segs) != 3 {
		t.Fatalf("built %d segments, want 3", len(segs))
	}
	dir := t.TempDir()
	var paths []string
	for i, s := range segs {
		p := filepath.Join(dir, "disk.E0"+string(rune('1'+i)))
		if err := os.WriteFile(p, s, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	e = &imgEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t)}
	code, out := run(t, e.d, append([]string{"image", "import", "--case", e.c, "--json"}, paths...)...)
	if err := json.Unmarshal(jsonPart(out), &recs); code != 0 || err != nil || len(recs) != 3 {
		t.Fatalf("image import: %d %s (%v)", code, out, err)
	}
	e.rec, e.ref = recs[0], recs[0].ID
	return e, media, recs
}

func TestRecordsShowOffsetMatchesE01Media(t *testing.T) {
	frag := fstest.Node{Path: "/frag.bin", Data: patternBytes(40 * 512), Fragments: 5}
	e, media, recs := newEWFProv(t, frag)
	// the decoded media, read back through the image package from the case's own .E01 files
	var paths []string
	for _, r := range recs {
		paths = append(paths, filepath.Join(e.c, filepath.FromSlash(r.Path)))
	}
	img, err := image.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	decoded := make([]byte, img.Size())
	if _, err := img.ReadAt(decoded, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, media) {
		t.Fatal("the image package does not decode the media the container was built from")
	}

	art := extractedRecord(t, e, "/frag.bin")
	held := artifactFile(t, e.c, art)
	id := ingestNote(t, e.c, art, &records.Range{Offset: 3000, Length: 9000})
	p := showClean(t, e.c, id)
	if n := requireOracle(t, p, held, decoded, 3000, 9000); n < 3 {
		t.Errorf("%d extents", n)
	}
	if p.Offset.Segments != 3 {
		t.Errorf("segments %d, want 3", p.Offset.Segments)
	}
	// the container file is not the oracle: the extents index the decoded media, not the .E01 bytes
	if raw, err := os.ReadFile(paths[0]); err == nil && len(p.Offset.Image) > 0 { //nolint:gosec // a test reading its own temp case
		e0 := p.Offset.Image[0]
		if e0.ImageOffset+e0.Length <= int64(len(raw)) && bytes.Equal(raw[e0.ImageOffset:e0.ImageOffset+e0.Length], held[e0.ArtifactOffset:e0.ArtifactOffset+e0.Length]) {
			t.Error("the container file matches at the media offset: the E01 oracle proves nothing")
		}
	}
	verifyClean(t, e.c)
}

// unallocBin runs the real `image unalloc` and returns the export and its decoded run map.
func unallocBin(t *testing.T, pi *provImg) (evidence.ManifestRecord, []evidence.UnallocRun) {
	t.Helper()
	if code, out := pi.e.image(t, "unalloc", pi.e.ref, "-p", "1"); code != 0 {
		t.Fatalf("image unalloc: %d\n%s", code, out)
	}
	bin := lastDerived(t, pi.e.c, "unallocated", "")
	recs, err := manifest(pi.e.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == bin.Source.Derived.RunsArtifact {
			m, err := evidence.ReadUnallocRunMap(bytes.NewReader(artifactFile(t, pi.e.c, r)))
			if err != nil {
				t.Fatal(err)
			}
			return bin, m
		}
	}
	t.Fatalf("the export names run map %q, which is not in the manifest", bin.Source.Derived.RunsArtifact)
	return bin, nil
}

func TestRecordsShowThroughRealUnallocatedExport(t *testing.T) {
	// the free blocks of the built disk are zero, which no oracle can tell apart: learn where the
	// export reads from, plant a pattern there in a second copy of the image, and export that one
	nodes := []fstest.Node{{Path: "/frag.bin", Data: patternBytes(8 * 512), Fragments: 2}}
	probe := newProvImg(t, 1, nodes...)
	_, m := unallocBin(t, probe)
	if len(m) == 0 {
		t.Fatal("the built image has no unallocated run")
	}
	disk := imgDisk(append(defaultNodes(), nodes...)...)
	for _, r := range m {
		copy(disk[r.ImageOffset:r.ImageOffset+r.Length], patternBytes(int(r.Length)))
	}
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, disk, 0o600); err != nil {
		t.Fatal(err)
	}
	e := &imgEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t)}
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", path)
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); code != 0 || err != nil || len(recs) != 1 {
		t.Fatalf("image import: %d %s (%v)", code, out, err)
	}
	e.ref = recs[0].ID
	raw, err := os.ReadFile(path) //nolint:gosec // a test reading the file it wrote
	if err != nil {
		t.Fatal(err)
	}
	export, m2 := unallocBin(t, &provImg{e: e, raw: raw})
	bin := artifactFile(t, e.c, export)
	if len(m2) == 0 || bytes.Contains(bin, []byte{0}) {
		t.Fatalf("the export is not the planted pattern (%d runs)", len(m2))
	}

	// the carve: bin[10:10+n), a piece of the export, then recorded on
	n := min(int64(len(bin))-10, 400)
	c, err := evidence.Open(e.c)
	if err != nil {
		t.Fatal(err)
	}
	carved := recordstest.AddDerivedWith(t, c, "carved.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: export.ID, ParentSHA256: export.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/carved", FSID: "nid:9",
		Runs: []evidence.Run{{Offset: 10, Length: n}},
	}}, bin[10:10+n])
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	id := ingestNote(t, e.c, carved, &records.Range{Offset: 5, Length: n - 10})
	p := showClean(t, e.c, id)
	requireOracle(t, p, artifactFile(t, e.c, carved), raw, 5, n-10)
	if len(p.Offset.Hops) != 2 || p.Offset.Hops[0].From != carved.ID || p.Offset.Hops[1].From != export.ID || p.Offset.Hops[1].To != e.ref {
		t.Errorf("hops %+v, want carved -> export -> image", p.Offset.Hops)
	}
	verifyClean(t, e.c)
}

// cancelFS is an MTFS filesystem whose opened files cancel the context after their first read, so an
// extract is cut off in the middle of a copy.
type cancelFS struct {
	filesys.FileSystem
	cancel func()
}

func (f *cancelFS) Open(e filesys.Entry) (filesys.File, error) {
	file, err := f.FileSystem.Open(e)
	if err != nil {
		return nil, err
	}
	return &cancelFile{File: file, cancel: f.cancel}, nil
}

type cancelFile struct {
	filesys.File
	cancel func()
	once   sync.Once
}

func (f *cancelFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.once.Do(f.cancel)
	return n, err
}

func TestRecordsShowCancelledExtractIsNoteNotProblem(t *testing.T) {
	big := fstest.Node{Path: "/big.bin", Data: patternBytes(300 * 512), Fragments: 3}
	pi := newProvImg(t, 1, big)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := Deps{FSDrivers: []detect.Driver{{Name: "mtfs", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
		fsys, err := fstest.Open(r, size)
		if err != nil {
			return nil, err
		}
		return &cancelFS{FileSystem: fsys, cancel: cancel}, nil
	}}}}
	runSplit(ctx, t, d, "image", "extract", "--case", pi.e.c, pi.e.ref, "/big.bin")
	art := lastDerived(t, pi.e.c, "extract", "/big.bin")
	held := artifactFile(t, pi.e.c, art)
	if !art.Incomplete || len(held) == 0 || len(held) >= len(big.Data) || art.Size != int64(len(held)) {
		t.Fatalf("the cancelled copy kept %d of %d bytes, incomplete %v", len(held), len(big.Data), art.Incomplete)
	}
	if len(art.Source.Derived.Runs) == 0 {
		t.Fatalf("the incomplete artifact lost its full run list: %+v", art.Source.Derived)
	}
	id := ingestNote(t, pi.e.c, art, &records.Range{Offset: 10, Length: int64(len(held)) - 20})
	p, code, _, stderr := showProv(t, pi.e.c, id)
	if code != 0 || stderr != "" || len(p.Problems) != 0 {
		t.Fatalf("exit %d stderr %q problems %+v: an interrupted copy is a note, not damage", code, stderr, p.Problems)
	}
	if len(p.Notes) == 0 {
		t.Error("no note says the artifact is incomplete")
	}
	requireOracle(t, p, held, pi.raw, 10, int64(len(held))-20)
}

func TestRecordsShowAPFSSnapshotProvenance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an APFS container")
	}
	img := apfstest.Build(apfstest.Options{Blocks: 2048, Xid: 20, Volumes: []apfstest.Volume{snapshotVolume("Data")}})
	path := imgFile(t, img)
	e := &imgEnv{d: Deps{}, c: newCLICase(t)}
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", path)
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); code != 0 || err != nil || len(recs) != 1 {
		t.Fatalf("image import: %d %s (%v)", code, out, err)
	}
	e.ref = recs[0].ID
	snapPath := "/Data/.snapshots/S/docs/a.txt"
	if code, out := e.image(t, "extract", e.ref, snapPath); code != 0 {
		t.Fatalf("extract: %d\n%s", code, out)
	}
	art := lastDerived(t, e.c, "extract", snapPath)
	sn := art.Source.Derived.Snapshot
	if sn == nil || sn.Name != "S" {
		t.Fatalf("derivation %+v: no snapshot", art.Source.Derived)
	}
	id := ingestNote(t, e.c, art, &records.Range{Offset: 0, Length: 5})
	p := showClean(t, e.c, id)
	got := p.Chain[0].Artifact.Source.Derived.Snapshot
	if got == nil || got.Name != "S" || got.Xid != sn.Xid {
		t.Errorf("shown snapshot %+v, manifest %+v", got, sn)
	}
	if p.Chain[0].Audit.State != "bound" {
		t.Errorf("audit %+v: the snapshot must come from the audited derivation", p.Chain[0].Audit)
	}
	// the audited derivation says the same
	var audited bool
	for _, a := range auditOf(t, e.c) {
		if a.Action == "artifact.create" && a.Details["id"] == art.ID {
			b, _ := json.Marshal(a.Details["source"])
			var s evidence.Source
			if err := json.Unmarshal(b, &s); err != nil || s.Derived == nil || s.Derived.Snapshot == nil || *s.Derived.Snapshot != *sn {
				t.Errorf("audit source %s does not carry the snapshot %+v", b, sn)
			}
			audited = true
		}
	}
	if !audited {
		t.Error("no artifact.create audit entry for the extract")
	}
	_, text := run(t, Deps{}, "records", "show", "--case", e.c, id)
	if want := `snapshot "S" (xid ` + strconv.FormatUint(sn.Xid, 10) + ")"; !strings.Contains(text, want) {
		t.Errorf("text output lacks %q:\n%s", want, text)
	}
	requireOracle(t, p, artifactFile(t, e.c, art), img, 0, 5)
}

func TestRecordsShowRecoveredArtifactEndToEnd(t *testing.T) {
	c := recordstest.NewCase(t)
	imgBytes := patternBytes(4096)
	img := recordstest.AddArtifact(t, c, "img.bin", imgBytes)
	conf := 60
	data := imgBytes[:8]
	rec := incompleteRecovered(t, c, img, data, &conf)
	recordstest.Ingest(t, c, recParser, []string{rec.ID}, []records.Record{
		{Type: "note", ArtifactID: rec.ID, Summary: "inside", Payload: map[string]any{}, Deleted: true, Recovery: "fat-contiguous", Confidence: rptr(50), Range: &records.Range{Offset: 2, Length: 4}},
		{Type: "note", ArtifactID: rec.ID, Summary: "past", Payload: map[string]any{}, Deleted: true, Recovery: "fat-contiguous", Confidence: rptr(50), Range: &records.Range{Offset: 0, Length: 1}},
	})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	verifyClean(t, dir)
	// 1: inside the captured prefix
	p := showClean(t, dir, "1")
	if p.Status != "recovered" || p.Recovery == nil || p.Recovery.Recovery.Class != evidence.ClassDeletedFile ||
		p.Recovery.Recovery.Method != "fat-contiguous" || p.Recovery.MinConfidence == nil || *p.Recovery.MinConfidence != 60 {
		t.Fatalf("status %q recovery %+v", p.Status, p.Recovery)
	}
	if a := p.Recovery.Recovery.Alloc; a.Free != 8 || a.Allocated != 0 || a.Unknown != 0 || a.ExcludedRuns != 1 || a.ExcludedBytes != 100 {
		t.Errorf("alloc %+v", a)
	}
	requireOracle(t, p, data, imgBytes, 2, 4)
	_, text := run(t, Deps{}, "records", "show", "--case", dir, "1")
	for _, want := range []string{
		"RECOVERED DATA (class deleted-file, method fat-contiguous, confidence 60; not live evidence)",
		"alloc 8/0/0, captured 8 bytes; 100 bytes in 1 runs named but not captured",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	// 2: a range past the captured prefix is refused, and the exit says integrity
	recordstest.SetRecordColumns(t, dir, 2, map[string]any{"src_offset": 6, "src_length": 20})
	p2, code, _, stderr := showProv(t, dir, "2")
	if code != 4 || p2.Offset.State != "unavailable" || len(p2.Offset.Image) != 0 || !strings.Contains(stderr, "case verify") {
		t.Fatalf("past the prefix: exit %d offset %+v stderr %q", code, p2.Offset, stderr)
	}
	if !hasProblem(p2, "range-outside") {
		t.Errorf("problems %+v lack range-outside", p2.Problems)
	}
	// 3: a LIVE record planted on the recovered artifact is a problem and is never shown as live
	recordstest.InjectRecord(t, dir, 99, 1, rec.ID, "planted live")
	p3, code, out, _ := showProv(t, dir, "99")
	if code != 4 || p3.Status != "recovered" {
		t.Fatalf("planted live record: exit %d status %q\n%s", code, p3.Status, out)
	}
	if !hasProblem(p3, "recovery-live-record") {
		t.Errorf("problems %+v lack recovery-live-record", p3.Problems)
	}
}

func hasProblem(p provTestJSON, kind string) bool {
	for _, pr := range p.Problems {
		if pr.Kind == kind {
			return true
		}
	}
	return false
}

// tamperFixture is img <- f (inline run 100+8, or the same run in a sidecar) with record 1 on f.
func tamperFixture(t *testing.T, sidecar bool) (dir string, img, f, side evidence.ManifestRecord) {
	t.Helper()
	c := recordstest.NewCase(t)
	img = recordstest.AddArtifact(t, c, "img.bin", make([]byte, 4096))
	d := &evidence.Derivation{ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/f", FSID: "nid:5"}
	runs := []evidence.Run{{Offset: 100, Length: 8}}
	if sidecar {
		side = recordstest.AddRunsSidecar(t, c, img, "f.runs.jsonl", runs)
		d.RunsArtifact = side.ID
	} else {
		d.Runs = runs
	}
	f = recordstest.AddDerivedWith(t, c, "f.bin", evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: d}, []byte("abcdefgh"))
	recordstest.Ingest(t, c, recParser, []string{f.ID}, []records.Record{{
		Type: "note", ArtifactID: f.ID, Summary: "tamper", Payload: map[string]any{}, Range: &records.Range{Offset: 2, Length: 4},
	}})
	dir = c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, img, f, side
}

// Wherever damage to the provenance is something `show` can check, `show` and `case verify` agree that
// there is damage; the one documented thing `show` does not check (the bytes of the artifact itself) is
// asserted by name.
func TestGetProvenanceMatchesVerifyOnTamper(t *testing.T) {
	cases := []struct {
		name    string
		sidecar bool
		tamper  func(t *testing.T, dir string, img, f, side evidence.ManifestRecord)
		want    string // the problem kind show must report
	}{
		{"manifest source rewritten", false, func(t *testing.T, dir string, _, f, _ evidence.ManifestRecord) {
			recordstest.SetManifestSource(t, dir, f.ID, func(s *evidence.Source) { s.Derived.FSPath = "/elsewhere" })
		}, "audit-differs"},
		{"parent hash edited in the manifest", false, func(t *testing.T, dir string, _, f, _ evidence.ManifestRecord) {
			recordstest.SetManifestSource(t, dir, f.ID, func(s *evidence.Source) { s.Derived.ParentSHA256 = strings.Repeat("b", 64) })
		}, "parent-hash"},
		{"runs sidecar bytes swapped", true, func(t *testing.T, dir string, _, _, side evidence.ManifestRecord) {
			old := artifactFile(t, dir, side)
			swapped := bytes.Replace(old, []byte("100"), []byte("200"), 1)
			if bytes.Equal(old, swapped) || len(old) != len(swapped) {
				t.Fatalf("sidecar %q cannot be swapped in place", old)
			}
			recordstest.SetArtifactFileBytes(t, dir, side.ID, swapped)
		}, "runs-sidecar"},
		{"parent removed from the manifest", false, func(t *testing.T, dir string, img, _, _ evidence.ManifestRecord) {
			recordstest.RemoveArtifactEverywhere(t, dir, img.ID)
		}, "parent-missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, img, f, side := tamperFixture(t, tc.sidecar)
			verifyClean(t, dir)
			showClean(t, dir, "1")
			tc.tamper(t, dir, img, f, side)
			if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != ExitIntegrity {
				t.Errorf("case verify: exit %d, want 4\n%s", code, out)
			}
			p, code, _, stderr := showProv(t, dir, "1")
			if code != ExitIntegrity || !strings.Contains(stderr, "case verify") || !hasProblem(p, tc.want) {
				t.Fatalf("show: exit %d problems %+v stderr %q, want a %s problem", code, p.Problems, stderr, tc.want)
			}
		})
	}

	// documented: show does not hash the artifact's own bytes; case verify does
	t.Run("artifact bytes changed, manifest untouched (show-clean, verify-problem)", func(t *testing.T) {
		dir, _, f, _ := tamperFixture(t, false)
		recordstest.SetArtifactFileBytes(t, dir, f.ID, []byte("ABCDEFGH"))
		showClean(t, dir, "1")
		if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != ExitIntegrity {
			t.Errorf("case verify: exit %d, want 4\n%s", code, out)
		}
	})
}

// caseFileHashes is the SHA-256 of every file of the case except audit.jsonl.
func caseFileHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Base(p) == "audit.jsonl" {
			return err
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // a test reading the files of its own temp case
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestShowDoesNotModifyCaseFiles(t *testing.T) {
	n := evidence.MaxInlineRuns + 1
	pi := newProvImg(t, 1, fstest.Node{Path: "/big.bin", Data: patternBytes(n * 512), Fragments: n})
	art := extractedRecord(t, pi.e, "/big.bin")
	if art.Source.Derived.RunsArtifact == "" {
		t.Fatal("want a derived artifact with a sidecar")
	}
	id := ingestNote(t, pi.e.c, art, &records.Range{Offset: 512, Length: 4096})
	before := caseFileHashes(t, pi.e.c)
	srcBefore := caseFileHashes(t, pi.src)
	auditBefore := len(auditOf(t, pi.e.c))
	for i, args := range [][]string{{"records", "show", id}, {"records", "show", id, "--json"}, {"records", "show", id, "--payload"}} {
		if code, out := run(t, Deps{}, append(args, "--case", pi.e.c)...); code != 0 {
			t.Fatalf("%v: exit %d\n%s", args, code, out)
		}
		if srcAfter := caseFileHashes(t, pi.src); len(srcBefore) == 0 || !mapsEqual(srcBefore, srcAfter) {
			t.Fatalf("%v changed an import source file", args)
		}
		if after := caseFileHashes(t, pi.e.c); !mapsEqual(before, after) {
			t.Fatalf("%v changed a case file other than audit.jsonl", args)
		}
		if es := auditOf(t, pi.e.c); len(es) != auditBefore+i+1 {
			t.Fatalf("%v: the audit log has %d entries, want %d", args, len(es), auditBefore+i+1)
		}
	}
	for _, a := range auditOf(t, pi.e.c)[auditBefore:] {
		if a.Action != "case.open" {
			t.Errorf("a show appended %q to the audit log", a.Action)
		}
	}
	verifyClean(t, pi.e.c)
}

// incompleteRecovered stores a deleted-file artifact derived from img whose copy was cut off: it holds
// data, the metadata named 100 more bytes that were not captured (so it is flagged incomplete).
func incompleteRecovered(t *testing.T, c *evidence.Case, img evidence.ManifestRecord, data []byte, conf *int) evidence.ManifestRecord {
	t.Helper()
	src := evidence.Source{Kind: evidence.KindRecover, DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: img.ID, ParentSHA256: img.SHA256, Partition: 1, FSType: "mtfs", FSPath: "/a", FSID: "dentry:1:1:1",
		Runs: []evidence.Run{{Offset: 0, Length: int64(len(data))}},
		Recovery: &evidence.Recovery{
			Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: conf, Basis: []string{"dentry:1:1:1"},
			Alloc:     evidence.AllocSummary{Free: int64(len(data)), ExcludedRuns: 1, ExcludedBytes: 100},
			Excluded:  []evidence.Run{{Offset: 3000, Length: 100}},
			Algorithm: evidence.AlgorithmRecover,
		},
	}}
	rec, err := c.Capture("dev1", "acq-derived", "recovered/p1-mtfs/000001-a.bin", src, func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return err
		}
		return errors.New("copy cut off")
	})
	if err == nil || !rec.Incomplete {
		t.Fatalf("record %+v, err %v: want a kept incomplete artifact", rec, err)
	}
	return rec
}
