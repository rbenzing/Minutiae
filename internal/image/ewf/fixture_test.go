package ewf_test

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"  //nolint:gosec // comparing stored hashes
	"crypto/sha1" //nolint:gosec // comparing stored hashes
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
)

type fixtureExpect struct {
	Raw struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		MD5    string `json:"md5"`
		SHA1   string `json:"sha1"`
	} `json:"raw"`
	Variants []fixtureVariant `json:"variants"`
}

type fixtureVariant struct {
	Name  string `json:"name"`
	Files []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
	Segments        int    `json:"segments"`
	MediaSize       int64  `json:"media_size"`
	BytesPerSector  int    `json:"bytes_per_sector"`
	SectorsPerChunk int    `json:"sectors_per_chunk"`
	Sectors         int64  `json:"sectors"`
	Chunks          int64  `json:"chunks"`
	MediaSHA256     string `json:"media_sha256"`
	StoredMD5       string `json:"stored_md5"`
	StoredSHA1      string `json:"stored_sha1"`
	Volume          struct {
		CompressionLevel int `json:"compression_level"`
	} `json:"volume"`
	Header2  map[string]string `json:"header2"`
	Sections []struct {
		Segment int    `json:"segment"`
		Type    string `json:"type"`
		Offset  int64  `json:"offset"`
		Next    int64  `json:"next"`
		Size    int64  `json:"size"`
	} `json:"sections"`
}

func gunzipFile(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadFixtureExpect(t *testing.T) fixtureExpect {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ewf-fixtures.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e fixtureExpect
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestOpenRealFixtureSections compares what Open reports for the real
// acquisition-tool output with the independent decoder's JSON.
func TestOpenRealFixtureSections(t *testing.T) {
	exp := loadFixtureExpect(t)
	if len(exp.Variants) == 0 {
		t.Fatal("no variants in the oracle")
	}
	for _, v := range exp.Variants {
		t.Run(v.Name, func(t *testing.T) {
			var segs []ewf.Segment
			for _, f := range v.Files {
				data := gunzipFile(t, f.Name+".gz")
				sum := sha256.Sum256(data)
				if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
					t.Fatalf("%s: fixture differs from the oracle", f.Name)
				}
				segs = append(segs, ewf.Segment{Name: f.Name, R: bytes.NewReader(data), Size: int64(len(data))})
			}
			r, err := ewf.Open(segs)
			if err != nil {
				t.Fatal(err)
			}
			if w := r.Warnings(); len(w) != 0 {
				t.Fatalf("warnings on a clean fixture: %q", w)
			}
			if r.Size() != v.MediaSize || r.SectorSize() != v.BytesPerSector ||
				r.ChunkSize() != v.BytesPerSector*v.SectorsPerChunk || r.Chunks() != v.Chunks {
				t.Fatalf("geometry: size %d sector %d chunk %d chunks %d; oracle %+v", r.Size(), r.SectorSize(), r.ChunkSize(), r.Chunks(), v)
			}
			if v.Sectors*int64(v.BytesPerSector) != v.MediaSize {
				t.Fatal("oracle inconsistent")
			}
			got := r.Sections()
			if len(got) != v.Segments {
				t.Fatalf("%d segments walked", len(got))
			}
			idx := make([]int, v.Segments)
			for _, s := range v.Sections {
				i := s.Segment - 1
				if idx[i] >= len(got[i]) {
					t.Fatalf("segment %d: oracle has more sections than Open", s.Segment)
				}
				g := got[i][idx[i]]
				idx[i]++
				if g.Type != s.Type || g.Offset != s.Offset || g.Next != s.Next || g.Size != s.Size {
					t.Fatalf("segment %d section %d: got %+v, oracle %+v", s.Segment, idx[i], g, s)
				}
			}
			for i := range got {
				if idx[i] != len(got[i]) {
					t.Fatalf("segment %d: Open found %d sections, oracle %d", i+1, len(got[i]), idx[i])
				}
			}
			m := meta(r)
			if m["md5"] != v.StoredMD5 || m["sha1"] != v.StoredSHA1 {
				t.Fatalf("stored hashes %q %q, oracle %q %q", m["md5"], m["sha1"], v.StoredMD5, v.StoredSHA1)
			}
			if m["segments"] != strconv.Itoa(v.Segments) || m["chunks"] != strconv.FormatInt(v.Chunks, 10) ||
				m["sector_count"] != strconv.FormatInt(v.Sectors, 10) {
				t.Fatalf("metadata segments %q chunks %q sector_count %q; oracle %d, %d, %d", m["segments"], m["chunks"], m["sector_count"], v.Segments, v.Chunks, v.Sectors)
			}
			// header2 wins over header.
			for field, key := range map[string]string{
				"c": "case_number", "n": "evidence_number", "a": "description",
				"e": "examiner", "t": "notes", "m": "acquired", "u": "system_date",
			} {
				if m[key] != v.Header2[field] {
					t.Fatalf("%s = %q, oracle header2 %s = %q", key, m[key], field, v.Header2[field])
				}
			}
		})
	}
}

// TestEWFReadAtMatchesRawFixture reads the whole media of every real
// acquisition-tool fixture and compares it, byte for byte, with the raw image
// the fixtures were made from (and with the oracle's hashes).
func TestEWFReadAtMatchesRawFixture(t *testing.T) {
	exp := loadFixtureExpect(t)
	raw := gunzipFile(t, "ewf-disk.img.gz")
	if int64(len(raw)) != exp.Raw.Size {
		t.Fatalf("raw image is %d bytes, oracle %d", len(raw), exp.Raw.Size)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != exp.Raw.SHA256 {
		t.Fatal("raw image differs from the oracle")
	}
	for _, v := range exp.Variants {
		t.Run(v.Name, func(t *testing.T) {
			var segs []ewf.Segment
			for _, f := range v.Files {
				data := gunzipFile(t, f.Name+".gz")
				segs = append(segs, ewf.Segment{Name: f.Name, R: bytes.NewReader(data), Size: int64(len(data))})
			}
			r, err := ewf.Open(segs)
			if err != nil {
				t.Fatal(err)
			}
			if r.Size() != v.MediaSize || v.MediaSize > int64(len(raw)) {
				t.Fatalf("size %d, oracle %d, raw %d", r.Size(), v.MediaSize, len(raw))
			}
			want := raw[:v.MediaSize] // seed-small holds a prefix of the raw image
			got := readAll(t, r)
			if !bytes.Equal(got, want) {
				t.Fatal("ReadAt over the whole media differs from the raw image")
			}
			if sum := sha256.Sum256(got); hex.EncodeToString(sum[:]) != v.MediaSHA256 {
				t.Fatal("media sha256 differs from the oracle")
			}
			md5s, sha1s := md5.Sum(got), sha1.Sum(got) //nolint:gosec // comparing stored hashes
			if hex.EncodeToString(md5s[:]) != v.StoredMD5 || hex.EncodeToString(sha1s[:]) != v.StoredSHA1 {
				t.Fatalf("md5/sha1 of the decoded media differ from the stored ones")
			}
			if v.MediaSize == exp.Raw.Size && (hex.EncodeToString(md5s[:]) != exp.Raw.MD5 || hex.EncodeToString(sha1s[:]) != exp.Raw.SHA1) {
				t.Fatal("md5/sha1 differ from the oracle's raw image hashes")
			}
			// Small unaligned reads across chunk (and segment) boundaries.
			rng := rand.New(rand.NewPCG(7, 1)) //nolint:gosec // deterministic test input, not security
			for range 300 {
				off := rng.Int64N(r.Size())
				p := make([]byte, 1+rng.IntN(3*r.ChunkSize()))
				n, err := r.ReadAt(p, off)
				if !bytes.Equal(p[:n], want[off:min(int64(len(want)), off+int64(n))]) || (n < len(p)) != (err == io.EOF) {
					t.Fatalf("read of %d bytes at %d: n=%d err=%v", len(p), off, n, err)
				}
			}
			if w := r.Warnings(); len(w) != 0 {
				t.Fatalf("warnings after reading a clean fixture: %q", w)
			}
		})
	}
}

// TestEWFTruncatedRealFixture cuts the last segment of the real multi-segment
// fixture mid-section: the set opens with a warning, the chunks whose table
// survived read back exactly, and the others are ErrChunkCorrupt reads.
func TestEWFTruncatedRealFixture(t *testing.T) {
	exp := loadFixtureExpect(t)
	raw := gunzipFile(t, "ewf-disk.img.gz")
	var v *fixtureVariant
	for i := range exp.Variants {
		if exp.Variants[i].Name == "multi-none" {
			v = &exp.Variants[i]
		}
	}
	if v == nil || v.Segments < 3 {
		t.Fatal("no multi-segment variant")
	}
	var files [][]byte
	for _, f := range v.Files {
		files = append(files, gunzipFile(t, f.Name+".gz"))
	}
	cs := int64(v.BytesPerSector * v.SectorsPerChunk)
	last := v.Segments
	// chunksIn[k] is how many (uncompressed) chunks segment k holds, from the
	// oracle's sectors sections: each chunk is cs bytes plus its Adler-32.
	chunksIn := make([]int64, last+1)
	cutPoint := map[string]int64{}
	for _, s := range v.Sections {
		if s.Type == "sectors" {
			chunksIn[s.Segment] += (s.Size - 76) / (cs + 4)
		}
		if s.Segment == last && (s.Type == "table2" || s.Type == "sectors") {
			cutPoint[s.Type] = s.Offset + 76 + (s.Size-76)/2
		}
	}
	var before int64
	for k := 1; k < last; k++ {
		before += chunksIn[k]
	}
	for cutIn, covered := range map[string]int64{"table2": before + chunksIn[last], "sectors": before} {
		t.Run("cut in "+cutIn, func(t *testing.T) {
			cut := append([][]byte(nil), files...)
			cut[last-1] = files[last-1][:cutPoint[cutIn]]
			r := mustOpen(t, cut)
			if !hasWarning(r, "truncated at offset") || !hasWarning(r, "E01 set incomplete") {
				t.Fatalf("warnings %q", r.Warnings())
			}
			p := make([]byte, covered*cs)
			if n, err := r.ReadAt(p, 0); n != len(p) || (err != nil && err != io.EOF) || !bytes.Equal(p, raw[:len(p)]) {
				t.Fatalf("covered chunks: %d bytes, %v", n, err)
			}
			if covered == v.Chunks {
				return // the table survived, so nothing is uncovered
			}
			q := bytes.Repeat([]byte{0xAA}, 64)
			n, err := r.ReadAt(q, covered*cs)
			if n != 0 || !errors.Is(err, ewf.ErrChunkCorrupt) || !bytes.Equal(q, bytes.Repeat([]byte{0xAA}, 64)) {
				t.Fatalf("first uncovered chunk: %d, %v", n, err)
			}
		})
	}
}
