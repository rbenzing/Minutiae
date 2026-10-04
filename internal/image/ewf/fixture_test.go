package ewf_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
)

type fixtureExpect struct {
	Raw struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
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
			if len(segs) != v.Segments {
				t.Fatalf("segments %d, oracle %d", len(segs), v.Segments)
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
			if m["chunks"] == "" || m["segments"] == "" {
				t.Fatalf("metadata %v", m)
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
