package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// importFiles writes datas to files called names in a temp dir and imports
// them, in the order given, as segments 1..N of one image.
func importFiles(t *testing.T, c *evidence.Case, names []string, datas [][]byte) (recs []evidence.ManifestRecord, paths []string) {
	t.Helper()
	dir := t.TempDir()
	for i, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, datas[i], 0o600); err != nil { //nolint:gosec // p is under t.TempDir()
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	recs, err := examine.Import(context.Background(), c, "img", paths, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return recs, paths
}

// e01Names are the usual names of an E01 set's segments.
func e01Names(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = "evidence.E0" + string(rune('1'+i))
	}
	return names
}

// e01Set wraps media in an E01 set of three segments with mixed compression
// (8 sectors per chunk).
func e01Set(t *testing.T, media []byte) [][]byte {
	t.Helper()
	segs := e01SetOf(media)
	if len(segs) != 3 {
		t.Fatalf("built %d segments, want 3", len(segs))
	}
	out := make([][]byte, len(segs))
	for i, s := range segs {
		out[i] = bytes.Clone(s) // tests corrupt their copy
	}
	return out
}

var (
	e01SetMu    sync.Mutex
	e01SetCache = map[*byte][][]byte{}
)

// e01SetOf builds (once per media slice) the three-segment set.
func e01SetOf(media []byte) [][]byte {
	e01SetMu.Lock()
	defer e01SetMu.Unlock()
	key := &media[0]
	if segs, ok := e01SetCache[key]; ok {
		return segs
	}
	chunks := (len(media) + e01Chunk - 1) / e01Chunk
	segs := ewftest.Build(ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: (chunks + 2) / 3, Compress: ewftest.CompressMixed}, media)
	e01SetCache[key] = segs
	return segs
}

// e01Content is what the builder disk holds.
type e01Content struct {
	raw  []byte
	ext4 map[string][]byte // path -> content
	fat  map[string][]byte
}

// e01BuilderDisk is a GPT disk with an ext4 partition (1) and a FAT16
// partition (2), each holding a small file and a file that spans many chunks.
// e01BuilderDisk returns the (cached, read-only) builder disk.
var e01BuilderDisk = sync.OnceValue(buildE01Disk)

func buildE01Disk() e01Content {
	e := e01Content{
		ext4: map[string][]byte{"/hello.txt": []byte("hello e01"), "/docs/big.bin": append([]byte("ext4 big file marker: "), pattern(60000, 7)...), "/docs/tail.txt": []byte("tail of the ext4 partition")},
		fat:  map[string][]byte{"/NOTE.TXT": []byte("a fat note"), "/BIG.BIN": append([]byte("fat big file marker: "), pattern(50000, 9)...)},
	}
	var xf []ext4test.File
	xf = append(xf, ext4test.File{Path: "/docs", Dir: true})
	for _, p := range []string{"/hello.txt", "/docs/big.bin", "/docs/tail.txt"} {
		xf = append(xf, ext4test.File{Path: p, Data: e.ext4[p]})
	}
	var ff []fattest.File
	for _, p := range []string{"/NOTE.TXT", "/BIG.BIN"} {
		ff = append(ff, fattest.File{Path: p, Data: e.fat[p]})
	}
	e.raw = disk(
		ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true, Label: "root"}, xf),
		fattest.Build(fattest.Options{Type: 16}, ff),
	)
	return e
}

// openE01 imports the three segments of media as an E01 set and opens the
// image with the default driver registry.
func openE01(t *testing.T, c *evidence.Case, media []byte) (*examine.Session, []evidence.ManifestRecord) {
	t.Helper()
	recs, _ := importFiles(t, c, e01Names(3), e01Set(t, media))
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, recs
}

// extractPartition extracts every file of one partition and returns the
// artifacts by file path (the runs sidecars are left out).
func extractPartition(t *testing.T, s *examine.Session, part int) (examine.Summary, map[string]evidence.ManifestRecord) {
	t.Helper()
	sum := extractAll(t, s, examine.ExtractOptions{Partition: part, Paths: []string{"/"}, Recursive: true})
	byPath := map[string]evidence.ManifestRecord{}
	for _, rec := range sum.Artifacts {
		if rec.Source.Kind != "extract" {
			continue
		}
		if _, dup := byPath[rec.Source.RemotePath]; dup {
			t.Fatalf("%s extracted twice", rec.Source.RemotePath)
		}
		byPath[rec.Source.RemotePath] = rec
	}
	return sum, byPath
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}

// sidecarOf reads the (image offset, length) runs of an unallocated export.
func unallocRuns(t *testing.T, c *evidence.Case, sum examine.Summary) []evidence.Run {
	t.Helper()
	var runs []evidence.Run
	for _, l := range strings.Split(strings.TrimSuffix(string(readArtifact(t, c, sum.Artifacts[0])), "\n"), "\n") {
		var u unallocLine
		if err := json.Unmarshal([]byte(l), &u); err != nil {
			t.Fatalf("sidecar line %q: %v", l, err)
		}
		runs = append(runs, evidence.Run{Offset: u.ImageOffset, Length: u.Length})
	}
	return runs
}

// A GPT disk with an ext4 and a FAT partition, wrapped in three E01 segments
// with mixed compression: everything the analysis commands do on the raw disk
// works on the E01, with provenance in logical media coordinates.
func TestExtractFromE01BuilderImage(t *testing.T) {
	e := e01BuilderDisk()
	c := newCase(t)
	s, recs := openE01(t, c, e.raw)

	info := s.Info()
	if info.Format != "ewf" || info.Size != int64(len(e.raw)) || len(info.Partitions) != 2 || len(info.Warnings) != 0 {
		t.Fatalf("Info: format %q size %d partitions %d warnings %q", info.Format, info.Size, len(info.Partitions), info.Warnings)
	}
	if info.Partitions[0].FSType != "ext4" || info.Partitions[1].FSType != "fat16" {
		t.Fatalf("filesystems %q, %q (errors %q, %q)", info.Partitions[0].FSType, info.Partitions[1].FSType, info.Partitions[0].Error, info.Partitions[1].Error)
	}

	wantSegs := make([]evidence.SegmentRef, len(recs))
	for i, r := range recs {
		wantSegs[i] = evidence.SegmentRef{ID: r.ID, SHA256: r.SHA256}
	}
	for part, want := range map[int]map[string][]byte{1: e.ext4, 2: e.fat} {
		fsType := info.Partitions[part-1].FSType
		sum, got := extractPartition(t, s, part)
		if sum.Skipped != 0 || sum.FSWarnings != 0 || sum.Files != len(want) || !slices.Equal(sortedKeys(got), sortedKeys(want)) {
			t.Fatalf("partition %d: summary %+v, extracted %v, want %v", part, sum, sortedKeys(got), sortedKeys(want))
		}
		for p, content := range want {
			rec := got[p]
			d := rec.Source.Derived
			if rec.Incomplete || rec.SHA256 != sha256hex(content) || rec.Size != int64(len(content)) || !bytes.Equal(readArtifact(t, c, rec), content) {
				t.Errorf("%s: sha256 %s size %d incomplete %v", p, rec.SHA256, rec.Size, rec.Incomplete)
			}
			if d == nil || d.ParentID != recs[0].ID || d.ParentSHA256 != recs[0].SHA256 || !slices.Equal(d.ParentSegments, wantSegs) ||
				d.FSType != fsType || d.Partition != part || d.ParentIncomplete {
				t.Errorf("%s: provenance %+v", p, d)
				continue
			}
			// The runs are in logical media coordinates (not E01 file offsets):
			// the raw disk at those runs is the content.
			if len(content) > 0 && !bytes.Equal(readImageRuns(e.raw, d.Runs), content) {
				t.Errorf("%s: the raw disk at the recorded runs is not the content", p)
			}
		}
	}

	// Unallocated space: the same runs and bytes as the same disk imported as a
	// plain file (the filesystem readers are checked against real images
	// elsewhere), each export being the concatenation of its runs.
	c2 := newCase(t)
	rawS, err := examine.Open(c2, importImage(t, c2, e.raw, 1)[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawS.Close() })
	for part := 1; part <= 2; part++ {
		want, err := rawS.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: part})
		if err != nil {
			t.Fatal(err)
		}
		wantRuns := unallocRuns(t, c2, want)
		if len(wantRuns) == 0 {
			t.Fatalf("partition %d: no free space in the builder image", part)
		}
		got, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: part})
		if err != nil {
			t.Fatal(err)
		}
		if got.FSWarnings != 0 || got.Skipped != 0 {
			t.Errorf("partition %d: unalloc summary %+v", part, got)
		}
		bin := checkUnalloc(t, c, e.raw, got, wantRuns, "p"+string(rune('0'+part))+"-"+info.Partitions[part-1].FSType)
		if bin.SHA256 != want.Artifacts[1].SHA256 {
			t.Errorf("partition %d: unallocated.bin differs from the raw image's export", part)
		}
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries on a clean image: %v", w)
	}
	verifyOK(t, c)
}

// A chunk that cannot be decoded in the middle of one file's data: that file is
// an incomplete artifact (only bytes that were read before the chunk, never
// zeros, with the runs of those bytes as provenance) and the others extract
// normally. Done once in the ext4 partition and once in the FAT one.
func TestExtractFromCorruptE01ChunkIsIncomplete(t *testing.T) {
	e := e01BuilderDisk()
	for _, tc := range []struct {
		name  string
		part  int
		files map[string][]byte
		big   string
	}{{"ext4", 1, e.ext4, "/docs/big.bin"}, {"fat", 2, e.fat, "/BIG.BIN"}} {
		t.Run(tc.name, func(t *testing.T) {
			big := tc.files[tc.big]
			base := bytes.Index(e.raw, big[:64]) // the builders store the data contiguously
			if base < 0 {
				t.Fatal("cannot find the file's data in the disk")
			}
			corruptAt := base + len(big)/2
			idx := corruptAt / e01Chunk
			chunkStart := idx * e01Chunk
			if chunkStart <= base || chunkStart+e01Chunk >= base+len(big) {
				t.Fatalf("test setup: chunk %d..%d is not strictly inside the file %d+%d", chunkStart, chunkStart+e01Chunk, base, len(big))
			}
			segs := e01Set(t, e.raw)
			l := ewftest.ChunkLocs(segs)[idx]
			segs[l.Segment-1][l.Offset+l.Length/2] ^= 0x5A

			c := newCase(t)
			recs, _ := importFiles(t, c, e01Names(3), segs)
			s, err := examine.Open(c, recs[0].ID, examine.Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			for part, files := range map[int]map[string][]byte{1: e.ext4, 2: e.fat} {
				sum, got := extractPartition(t, s, part)
				for p, content := range files {
					rec, ok := got[p]
					if !ok {
						t.Fatalf("%s was not extracted: %+v", p, sum)
					}
					if part == tc.part && p == tc.big {
						continue
					}
					if rec.Incomplete || !bytes.Equal(readArtifact(t, c, rec), content) {
						t.Errorf("%s: the intact file was affected (incomplete %v)", p, rec.Incomplete)
					}
				}
				if part != tc.part {
					if sum.Skipped != 0 || sum.Files != len(files) {
						t.Errorf("partition %d, untouched by the damage: %+v", part, sum)
					}
					continue
				}
				rec := got[tc.big]
				data := readArtifact(t, c, rec)
				if !rec.Incomplete || !strings.Contains(rec.Error, "chunk "+strconv.Itoa(idx)) {
					t.Fatalf("%s: incomplete=%v error=%q", tc.big, rec.Incomplete, rec.Error)
				}
				// Exactly the bytes before the unreadable chunk: never zeros, never more,
				// although the filesystem readers drop the part of a failing read.
				if !bytes.Equal(data, big[:chunkStart-base]) {
					t.Fatalf("%s holds %d bytes; want the %d bytes before the chunk", tc.big, len(data), chunkStart-base)
				}
				t.Logf("%s: %d of %d bytes kept (the chunk starts %d bytes into the file)", tc.big, len(data), len(big), chunkStart-base)
				if sum.Files != len(files)-1 || sum.Skipped != 1 {
					t.Errorf("summary Files=%d Skipped=%d, want %d and 1", sum.Files, sum.Skipped, len(files)-1)
				}
				d := rec.Source.Derived
				var covered int64
				for _, r := range d.Runs {
					covered += r.Length
				}
				if d.RunsArtifact != "" || covered != rec.Size || !bytes.Equal(readImageRuns(e.raw, d.Runs), data) {
					t.Errorf("runs cover %d bytes (sidecar %q); want exactly the %d captured bytes", covered, d.RunsArtifact, rec.Size)
				}
				var reasons []string
				for _, w := range auditByAction(t, c, "analysis.warning") {
					r, _ := w.Details["reason"].(string)
					reasons = append(reasons, r)
				}
				if joined := strings.Join(reasons, "\n"); !strings.Contains(joined, "partial artifact kept, flagged incomplete") || !strings.Contains(joined, "chunk "+strconv.Itoa(idx)) {
					t.Errorf("warnings do not explain the partial extraction: %q", reasons)
				}
			}
			if len(auditByAction(t, c, "analysis.error")) != 0 || len(auditByAction(t, c, "analysis.end")) != 2 {
				t.Error("both analyses must end normally")
			}
			verifyOK(t, c)
		})
	}
}

// The chunk holding the GPT header is unreadable: Open fails with an error that
// names the chunk (a content problem: exit 1), never a panic.
func TestOpenE01CorruptPartitionTableChunkIsError(t *testing.T) {
	e := e01BuilderDisk()
	segs := e01Set(t, e.raw)
	l := ewftest.ChunkLocs(segs)[0] // LBA 0-7: the protective MBR and the GPT header
	segs[l.Segment-1][l.Offset+l.Length/2] ^= 0x5A
	c := newCase(t)
	recs, _ := importFiles(t, c, e01Names(3), segs)
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded on an image whose partition table is unreadable")
	}
	if !errors.Is(err, image.ErrChunkCorrupt) || !strings.Contains(err.Error(), "chunk 0") || !strings.Contains(err.Error(), "partition table") {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("a damaged container is not a case integrity failure: %v", err)
	}
}

// The acquisition errors an E01 records (sectors that could not be read and are
// zero-filled) are a warning of Info, never a filesystem warning, and extraction
// from the image goes on.
func TestE01Error2WarningInInfo(t *testing.T) {
	e := e01BuilderDisk()
	c := newCase(t)
	chunks := (len(e.raw) + e01Chunk - 1) / e01Chunk
	segs := ewftest.Build(ewftest.Options{
		SectorsPerChunk: 8, ChunksPerSegment: (chunks + 2) / 3, Compress: ewftest.CompressMixed,
		ErrorRanges: [][2]uint32{{4000, 5}, {9000, 2}},
	}, e.raw)
	recs, _ := importFiles(t, c, e01Names(len(segs)), segs)
	s, err := examine.Open(c, recs[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var n int
	for _, w := range s.Info().Warnings {
		if strings.Contains(w, "acquisition error: 2 sector range(s) (7 sectors) were unreadable at acquisition") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("warnings %q", s.Info().Warnings)
	}
	sum, _ := extractPartition(t, s, 2)
	if sum.FSWarnings != 0 || sum.Skipped != 0 {
		t.Errorf("the container warning leaked into the analysis: %+v", sum)
	}
}

// The three E01 files are imported as segments 1..3 with their original paths
// and sizes; the E01 naming (.E01, .E02, ...) is kept in the artifact names.
func TestImportE01RecordsOriginalAndSegments(t *testing.T) {
	e := e01BuilderDisk()
	c := newCase(t)
	segs := e01Set(t, e.raw)
	recs, paths := importFiles(t, c, e01Names(3), segs)
	if len(recs) != 3 {
		t.Fatalf("%d records", len(recs))
	}
	dir := filepath.Dir(recs[0].Path)
	for i, r := range recs {
		if r.Source.Kind != "import" || r.Source.Segment != i+1 || r.Source.Segments != 3 || r.Source.OriginalPath != paths[i] ||
			r.Source.RemoteSize != int64(len(segs[i])) || r.Size != int64(len(segs[i])) || r.SHA256 != sha256hex(segs[i]) || r.Incomplete {
			t.Errorf("segment %d record: %+v", i+1, r)
		}
		if filepath.Dir(r.Path) != dir || filepath.Base(r.Path) != "evidence.E0"+string(rune('1'+i)) {
			t.Errorf("segment %d is stored as %q", i+1, r.Path)
		}
		// Every segment is reachable and opens as the same image.
		s, err := examine.Open(c, r.ID, examine.Options{})
		if err != nil {
			t.Fatalf("Open via segment %d: %v", i+1, err)
		}
		if s.Parent.ID != recs[0].ID || len(s.Segments) != 3 || s.Image.Format() != "ewf" {
			t.Errorf("segment %d opens %s with %d segments (%s)", i+1, s.Parent.ID, len(s.Segments), s.Image.Format())
		}
		_ = s.Close()
	}
	// A set with a segment missing from the case still opens, with a warning that it is incomplete.
	c2 := newCase(t)
	recs2, _ := importFiles(t, c2, e01Names(2), segs[:2])
	// The container opens (the segments present are readable) but says the set is incomplete.
	s2, err := examine.OpenContainer(c2, recs2[0].ID, examine.Options{})
	if err != nil {
		t.Fatalf("OpenContainer of a partial set: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if w := strings.Join(s2.Info().Warnings, "; "); !strings.Contains(w, "set incomplete") {
		t.Errorf("a two-segment import of a three-segment E01 set: warnings %q", w)
	}
}

// ---- real acquisition-tool output (fixtures) ----------------------------------

type ewfFixtureExpect struct {
	Raw        struct{ Size int64 } `json:"raw"`
	Partitions struct {
		Partitions []struct {
			Index         int    `json:"index"`
			Name          string `json:"name"`
			SourceFixture string `json:"source_fixture"`
		} `json:"partitions"`
	} `json:"partitions"`
}

func ewfTestdata(t *testing.T, name string) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join("..", "image", "ewf", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkAgainstOracle compares the extraction of one partition with the oracle of
// the filesystem image it was made from (computed from the source tree).
func checkAgainstOracle(t *testing.T, sum examine.Summary, got map[string]evidence.ManifestRecord, want fixtureOracle) {
	t.Helper()
	wantFiles := 0
	for _, f := range want.Files {
		if f.Type == "dir" {
			continue
		}
		wantFiles++
		rec, ok := got[f.Path]
		if !ok {
			t.Errorf("%s: not extracted", f.Path)
			continue
		}
		if rec.SHA256 != f.SHA256 || rec.Size != f.Size || rec.Incomplete {
			t.Errorf("%s: sha256/size %s/%d incomplete=%v, oracle %s/%d", f.Path, rec.SHA256, rec.Size, rec.Incomplete, f.SHA256, f.Size)
		}
	}
	if len(got) != wantFiles || sum.Files != wantFiles {
		t.Errorf("%d artifacts, Files=%d, want %d", len(got), sum.Files, wantFiles)
	}
	skipped := map[string]bool{}
	for _, w := range sum.Warnings {
		skipped[w.Path] = true
	}
	for _, d := range want.Deleted {
		if !skipped[d] {
			t.Errorf("deleted entry %s of the oracle was not skipped", d)
		}
	}
	if sum.FSWarnings != 0 {
		t.Errorf("FSWarnings = %d on a clean image", sum.FSWarnings)
	}
}

// The real E01 sets (one made with no compression in five segments, one with
// best compression in one) hold a disk whose two partitions are the committed
// ext4 and FAT12 fixtures: each file's hash equals the oracle of its source
// image, and the extraction equals that of the raw disk imported as a plain
// file.
func TestExtractFromE01Fixture(t *testing.T) {
	if testing.Short() {
		t.Skip("extracts about 150 files from two E01 sets")
	}
	var exp ewfFixtureExpect
	raw, err := os.ReadFile(filepath.Join("..", "image", "ewf", "testdata", "ewf-fixtures.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	disk := ewfTestdata(t, "ewf-disk.img.gz")
	if int64(len(disk)) != exp.Raw.Size || len(exp.Partitions.Partitions) != 2 {
		t.Fatalf("oracle: disk %d bytes, %d partitions", len(disk), len(exp.Partitions.Partitions))
	}
	oracles := map[int]fixtureOracle{}
	for _, p := range exp.Partitions.Partitions {
		pkg, base := filepath.Base(filepath.Dir(filepath.Dir(p.SourceFixture))), strings.TrimSuffix(filepath.Base(p.SourceFixture), ".img.gz")
		_, oracles[p.Index] = loadFixture(t, pkg, base)
	}

	// The plain extraction is the reference.
	c0 := newCase(t)
	plain, err := examine.Open(c0, importImage(t, c0, disk, 1)[0].ID, examine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	reference := map[int]map[string]string{}
	for part := 1; part <= 2; part++ {
		sum, got := extractPartition(t, plain, part)
		checkAgainstOracle(t, sum, got, oracles[part])
		reference[part] = map[string]string{}
		for p, r := range got {
			reference[part][p] = r.SHA256
		}
	}

	for _, name := range []string{"multi-none", "single-best"} {
		t.Run(name, func(t *testing.T) {
			var names []string
			var datas [][]byte
			for i := 1; i <= 5; i++ {
				f := "ewf-" + name + ".E0" + string(rune('0'+i))
				b, err := os.ReadFile(filepath.Join("..", "image", "ewf", "testdata", f+".gz"))
				if err != nil {
					if os.IsNotExist(err) {
						break
					}
					t.Fatal(err)
				}
				zr, err := gzip.NewReader(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(zr)
				if err != nil {
					t.Fatal(err)
				}
				names, datas = append(names, strings.TrimPrefix(f, "ewf-")), append(datas, data)
			}
			c := newCase(t)
			recs, _ := importFiles(t, c, names, datas)
			s, err := examine.Open(c, recs[0].ID, examine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			info := s.Info()
			if info.Format != "ewf" || len(info.Partitions) != 2 || len(info.Warnings) != 0 || info.Partitions[0].FSType != "ext4" || info.Partitions[1].FSType != "fat12" {
				t.Fatalf("Info: %+v", info)
			}
			for part := 1; part <= 2; part++ {
				sum, got := extractPartition(t, s, part)
				checkAgainstOracle(t, sum, got, oracles[part])
				for p, r := range got {
					if reference[part][p] != r.SHA256 {
						t.Errorf("partition %d %s: E01 artifact sha256 %s, raw disk %s", part, p, r.SHA256, reference[part][p])
					}
					if d := r.Source.Derived; d.ParentSHA256 != recs[0].SHA256 || len(d.ParentSegments) != len(recs) && len(recs) > 1 {
						t.Errorf("%s: provenance %+v", p, d)
					}
				}
				if len(got) != len(reference[part]) {
					t.Errorf("partition %d: %d artifacts from the E01, %d from the raw disk", part, len(got), len(reference[part]))
				}
			}
			verifyOK(t, c)

			// Container verification of the real set matches its stored hashes.
			cv, err := s.VerifyContainer(context.Background(), nil)
			if err != nil || cv.Result != "match" {
				t.Fatalf("VerifyContainer: %+v, %v", cv, err)
			}
			verifyOK(t, c)
		})
	}
}
