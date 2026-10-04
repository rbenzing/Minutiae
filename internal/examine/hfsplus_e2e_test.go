package examine_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/hfsplus/hfsplustest"
)

// hfsOracle is the part of internal/filesys/hfsplus/testdata/*.expect.json used
// here. The oracle is a second parse of the image plus fsck.hfsplus and blkid,
// and for the populated image a comparison with the source tree the Linux
// hfsplus driver was fed (tools/fixtures/hfsplus_oracle.py): never Minutiae.
// Free space is in allocation blocks from the start of the VOLUME; a wrapped
// volume starts volume_offset bytes into the image.
type hfsOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type         string     `json:"type"`
	BlockSize    int64      `json:"block_size"`
	VolumeOffset int64      `json:"volume_offset"`
	FreeBlocks   [][2]int64 `json:"free_blocks"`
	Files        []struct {
		Path     string `json:"path"`
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
		Modified *int64 `json:"modified"`
	} `json:"files"`
}

func loadHFSFixture(t *testing.T, name string) ([]byte, hfsOracle) {
	t.Helper()
	dir := filepath.Join("..", "filesys", "hfsplus", "testdata")
	gz, err := os.ReadFile(filepath.Join(dir, name+".img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o hfsOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(img); got != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s != oracle %s: the oracle is stale, regenerate the fixtures", got, o.Generator.ImageSHA256)
	}
	return img, o
}

// hfsDisplayPath is the path the reader shows for an oracle path: a component
// holding a NUL (the hard-link private folder) is "~raw~" + base64url of its
// UTF-16 big-endian bytes.
func hfsDisplayPath(p string) string {
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if strings.ContainsRune(c, 0) {
			var b []byte
			for _, u := range utf16.Encode([]rune(c)) {
				b = append(b, byte(u>>8), byte(u))
			}
			parts[i] = "~raw~" + base64.RawURLEncoding.EncodeToString(b)
		}
	}
	return strings.Join(parts, "/")
}

// TestExtractFromHFSPlusFixture imports each real image (the five mkfs.hfsplus
// volumes, whose only files are the journal's, and the volume the Linux driver
// populated) as a whole-disk image and extracts everything through the default
// driver registry: hashes, sizes and modification times equal the oracle,
// symlinks are extracted as their target text, every hard link is a copy,
// Derived.ParentSHA256 is the imported image's, FSType is the volume's type,
// there are no filesystem warnings, the unallocated export is exactly the
// oracle's free blocks (offset by a wrapper), and case verify is OK.
func TestExtractFromHFSPlusFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and extracts a 16 MiB image (about 830 artifacts) and five 8 MiB ones")
	}
	for _, name := range []string{"hfsplus-journal", "hfsplus-populated", "hfsplus-empty", "hfsx-empty", "hfsplus-1k", "hfsplus-wrapped"} {
		t.Run(name, func(t *testing.T) {
			img, want := loadHFSFixture(t, name)
			c := newCase(t)
			imgSHA := sha256hex(img)
			recs := importImage(t, c, img, 1)
			s, err := examine.Open(c, recs[0].ID, examine.Options{}) // default driver registry
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if p := s.Info().Partitions; len(p) != 1 || p[0].FSType != want.Type || p[0].Error != "" {
				t.Fatalf("partitions = %+v, want one %s filesystem", p, want.Type)
			}
			sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			wantFiles := map[string]int{}
			for _, f := range want.Files {
				if f.Type != "dir" {
					wantFiles[hfsDisplayPath(f.Path)] = 1
				}
			}
			byPath := map[string]int{}
			for _, rec := range sum.Artifacts {
				if strings.HasSuffix(rec.Path, ".runs.jsonl") {
					continue // provenance sidecar of a heavily fragmented file
				}
				d := rec.Source.Derived
				if rec.Source.Kind != "extract" || d == nil {
					t.Fatalf("artifact %s has no extract provenance: %+v", rec.Path, rec.Source)
				}
				if d.ParentSHA256 != imgSHA {
					t.Errorf("%s: Derived.ParentSHA256 = %s, want the imported image's %s", d.FSPath, d.ParentSHA256, imgSHA)
				}
				if d.FSType != want.Type || !strings.Contains(rec.Path, "/p0-"+want.Type+"/") {
					t.Errorf("%s: Derived.FSType = %q, path %q, want %s", d.FSPath, d.FSType, rec.Path, want.Type)
				}
				if rec.Incomplete {
					t.Errorf("%s: flagged incomplete", d.FSPath)
				}
				byPath[d.FSPath]++
				var found bool
				for _, f := range want.Files {
					if hfsDisplayPath(f.Path) != d.FSPath {
						continue
					}
					found = true
					if f.Type == "dir" {
						t.Errorf("%s: a directory was extracted as a file", f.Path)
					}
					if rec.SHA256 != f.SHA256 || rec.Size != f.Size {
						t.Errorf("%s: artifact sha256/size %s/%d, want %s/%d", f.Path, rec.SHA256, rec.Size, f.SHA256, f.Size)
					}
					if f.Modified == nil {
						t.Errorf("%s: the oracle has no modification time", f.Path)
						break
					}
					if got, want := d.Times["modified"], time.Unix(*f.Modified, 0).UTC().Format(time.RFC3339); got != want {
						t.Errorf("%s: derived modified %q, want %q (HFS+ dates are UTC)", f.Path, got, want)
					}
				}
				if !found {
					t.Errorf("artifact for %s is not in the oracle", d.FSPath)
				}
			}
			for p := range wantFiles {
				if byPath[p] != 1 {
					t.Errorf("%s: %d artifacts, want 1", p, byPath[p])
				}
			}
			if sum.Files != len(wantFiles) || sum.Skipped != 0 || len(sum.Warnings) != 0 || sum.FSWarnings != 0 {
				t.Errorf("summary: files %d (want %d), skipped %d, warnings %v, FSWarnings %d", sum.Files, len(wantFiles), sum.Skipped, sum.Warnings, sum.FSWarnings)
			}

			// image unalloc -p: exactly the oracle's free blocks.
			var wantRuns []evidence.Run
			for _, r := range want.FreeBlocks {
				wantRuns = append(wantRuns, evidence.Run{Offset: want.VolumeOffset + r[0]*want.BlockSize, Length: (r[1] - r[0] + 1) * want.BlockSize})
			}
			if len(wantRuns) == 0 {
				t.Fatal("the oracle lists no free space")
			}
			usum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
			if err != nil {
				t.Fatalf("ExportUnallocated: %v", err)
			}
			if usum.FSWarnings != 0 || usum.Skipped != 0 {
				t.Errorf("unalloc summary = %+v", usum)
			}
			checkUnalloc(t, c, img, usum, wantRuns, "p0-"+want.Type)
			verifyOK(t, c)
		})
	}
}

// A volume holding a 255-unit name (the most HFS+ stores: ASCII, and 255 CJK
// characters, 765 bytes of UTF-8) extracts without aborting: the local path
// component is shortened (with a hash suffix) while the provenance keeps the
// original path.
func TestExtractHFSPlusLongNames(t *testing.T) {
	ascii := strings.Repeat("a", 251) + ".txt"
	cjk := strings.Repeat("日", 255)
	dir := strings.Repeat("d", 255)
	img := hfsplustest.Build(hfsplustest.Options{Label: "LONG"}, []hfsplustest.File{
		{Path: "/" + ascii, Data: []byte("one")},
		{Path: "/" + cjk, Data: pattern(5000, 1)},
		{Path: "/" + dir, Dir: true},
		{Path: "/" + dir + "/" + ascii, Data: []byte("four")},
	})
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 3 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Artifacts) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	want := map[string][]byte{
		"/" + ascii:             []byte("one"),
		"/" + cjk:               pattern(5000, 1),
		"/" + dir + "/" + ascii: []byte("four"),
	}
	locals := map[string]string{}
	for _, r := range sum.Artifacts {
		for _, comp := range strings.Split(r.Path, "/") {
			if len(comp) > 255 {
				t.Errorf("component of %d bytes in %q", len(comp), r.Path)
			}
		}
		w, ok := want[r.Source.RemotePath]
		if !ok || !bytes.Equal(readArtifact(t, c, r), w) {
			t.Errorf("artifact %q (remote %q) content wrong", r.Path, r.Source.RemotePath)
		}
		if r.Source.Derived == nil || r.Source.Derived.FSPath != r.Source.RemotePath || r.Source.Derived.FSType != "hfsplus" || !strings.Contains(r.Path, "/p1-hfsplus/") {
			t.Errorf("provenance lost the original path or type: %+v", r.Source)
		}
		if prev, dup := locals[r.Path]; dup {
			t.Errorf("artifacts %q and %q share the local path %q", prev, r.Source.RemotePath, r.Path)
		}
		locals[r.Path] = r.Source.RemotePath
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for a clean image: %v", w)
	}
	verifyOK(t, c)
}

// Empty files, and files whose content is not in the data fork (a small file
// compressed inline in its decmpfs attribute), have no allocation blocks: they
// are extracted whole as artifacts with nil runs, without any warning, while a
// file with data blocks records its runs.
func TestExtractHFSPlusEmptyFiles(t *testing.T) {
	inline := bytes.Repeat([]byte("compressible "), 100)
	img := hfsplustest.Build(hfsplustest.Options{Label: "EMPTY"}, []hfsplustest.File{
		{Path: "/EMPTY.TXT"},
		{Path: "/FULL.BIN", Data: pattern(6000, 2)},
		{Path: "/INLINE.TXT", OwnerFlags: 0x20, Attrs: []hfsplustest.Attr{{Name: "com.apple.decmpfs", Value: decmpfsZlib(inline)}}},
	})
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 3 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Warnings) != 0 || len(sum.Artifacts) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	seen := 0
	for _, rec := range sum.Artifacts {
		d := rec.Source.Derived
		if d == nil || rec.Incomplete {
			t.Errorf("%s: derived %+v incomplete %v", rec.Source.RemotePath, d, rec.Incomplete)
			continue
		}
		seen++
		got := readArtifact(t, c, rec)
		switch rec.Source.RemotePath {
		case "/EMPTY.TXT":
			if rec.Size != 0 || d.Runs != nil || d.RunsArtifact != "" {
				t.Errorf("empty file: size %d derived %+v, want an empty artifact with nil runs", rec.Size, d)
			}
		case "/INLINE.TXT":
			if !bytes.Equal(got, inline) || d.Runs != nil || d.RunsArtifact != "" {
				t.Errorf("inline compressed file: %d bytes, derived %+v, want its decompressed text and nil runs", len(got), d)
			}
		case "/FULL.BIN":
			if !bytes.Equal(got, pattern(6000, 2)) || len(d.Runs) == 0 {
				t.Errorf("file with data blocks: size %d runs %v", rec.Size, d.Runs)
			}
		default:
			seen--
			t.Errorf("unexpected artifact %q", rec.Source.RemotePath)
		}
	}
	if seen != 3 {
		t.Errorf("checked %d of 3 artifacts", seen)
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for a clean image: %v", w)
	}
	verifyOK(t, c)
}

// A file whose allocation ends inside the image (here the image is cut inside
// the file's fourth extent) is extracted as the trusted prefix flagged
// incomplete, with the runs of that prefix recorded: they reproduce exactly the
// bytes captured. The intact file next to it is not affected, and the
// filesystem's truncation warning is counted apart from the skip.
func TestExtractHFSPlusBrokenForkIsIncomplete(t *testing.T) {
	const bs = 4096
	content := pattern(12*bs, 7)
	img0, lay := hfsplustest.BuildLayout(hfsplustest.Options{Label: "BROKEN"}, []hfsplustest.File{
		{Path: "/fine.bin", Data: []byte("fine")},
		{Path: "/frag.bin", Data: content, Fragment: 2},
	})
	exts := lay.Files["/frag.bin"].Data
	if len(exts) != 6 {
		t.Fatalf("test setup: %d extents", len(exts))
	}
	img := img0[:(int64(exts[1].Start)+1)*bs+10] // extent 1 keeps its first block: 3 blocks of the file stay mapped
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
	if !strings.HasSuffix(frag.Source.RemotePath, "frag.bin") || !strings.Contains(frag.Path, "/p1-hfsplus/") {
		t.Fatalf("artifacts = %q / %q", frag.Source.RemotePath, fine.Source.RemotePath)
	}
	if !frag.Incomplete || frag.Size != 3*bs || !bytes.Equal(readArtifact(t, c, frag), content[:3*bs]) {
		t.Errorf("broken-fork artifact: incomplete=%v size=%d, want an incomplete 3-block prefix", frag.Incomplete, frag.Size)
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
		t.Errorf("Skipped = %d (want 1: the partial read), FSWarnings = %d (want the truncation warning)", sum.Skipped, sum.FSWarnings)
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

// A file compressed with a scheme the reader does not decode (LZVN) is skipped
// with an analysis.warning, as a compressed F2FS file is; its neighbours are
// extracted, the case verifies, and nothing is written for the skipped file.
func TestExtractHFSPlusCompressedIsSkippedWithWarning(t *testing.T) {
	img := hfsplustest.Build(hfsplustest.Options{Label: "CMP"}, []hfsplustest.File{
		{Path: "/lzvn.dat", OwnerFlags: 0x20, Attrs: []hfsplustest.Attr{{Name: "com.apple.decmpfs", Value: decmpfsValue(7, 1234, []byte("xxxx"))}}},
		{Path: "/plain.txt", Data: []byte("plain")},
	})
	s, _ := imageSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 1 || sum.Skipped != 1 || len(sum.Warnings) != 1 || sum.Warnings[0].Path != "/lzvn.dat" || len(sum.Artifacts) != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if r := sum.Warnings[0].Reason; !strings.Contains(r, "unsupported") && !strings.Contains(r, "compress") {
		t.Errorf("reason = %q", r)
	}
	if got := string(readArtifact(t, c, sum.Artifacts[0])); got != "plain" || sum.Artifacts[0].Source.RemotePath != "/plain.txt" {
		t.Errorf("artifact %q = %q", sum.Artifacts[0].Source.RemotePath, got)
	}
	var skipped bool
	for _, e := range auditByAction(t, c, "analysis.warning") {
		if strings.Contains(string(mustJSON(t, e)), "/lzvn.dat") {
			skipped = true
		}
	}
	if !skipped {
		t.Error("no analysis.warning names the skipped file")
	}
	verifyOK(t, c)
}

// TestInfoDetectsHFSPlusPartition runs the default driver registry over a GPT
// image whose partition holds an HFS+ filesystem (and an HFSX one).
func TestInfoDetectsHFSPlusPartition(t *testing.T) {
	for name, tc := range map[string]struct {
		o    hfsplustest.Options
		want string
	}{
		"hfsplus": {hfsplustest.Options{Label: "Macintosh HD"}, "hfsplus"},
		"hfsx":    {hfsplustest.Options{Label: "Macintosh HD", HFSX: true, CaseSensitive: true}, "hfsx"},
		"wrapped": {hfsplustest.Options{Label: "Macintosh HD", Wrapper: true}, "hfsplus"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCase(t)
			fsImg := hfsplustest.Build(tc.o, []hfsplustest.File{{Path: "/hello.txt", Data: []byte("hello")}})
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
			if p.FSType != tc.want || p.FSInfo == nil || p.FSInfo.Type != tc.want || p.FSInfo.Label != "Macintosh HD" || p.Error != "" {
				t.Errorf("partition = %+v (FSInfo %+v), want an %s filesystem labelled Macintosh HD", p, p.FSInfo, tc.want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decmpfsValue is a com.apple.decmpfs attribute value: magic "fpmc", the type
// and the uncompressed size (little-endian), then the payload.
func decmpfsValue(typ uint32, size uint64, payload []byte) []byte {
	v := make([]byte, 16, 16+len(payload))
	copy(v, "fpmc")
	binary.LittleEndian.PutUint32(v[4:], typ)
	binary.LittleEndian.PutUint64(v[8:], size)
	return append(v, payload...)
}

// decmpfsZlib is the attribute of a small file stored zlib-compressed inline.
func decmpfsZlib(content []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	_, _ = w.Write(content)
	_ = w.Close()
	return decmpfsValue(3, uint64(len(content)), z.Bytes())
}
