package examine_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/examine"
)

// ext4Oracle is the part of internal/filesys/ext4/testdata/*.expect.json used
// here. The oracle comes from the source tree given to mke2fs, never from
// Minutiae (see tools/fixtures/ext4.sh).
type ext4Oracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Files []struct {
		Path   string `json:"path"`
		Type   string `json:"type"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Mode   uint32 `json:"mode"`
		Mtime  int64  `json:"mtime"`
	} `json:"files"`
	Deleted []string `json:"deleted"`
}

func loadExt4Fixture(t *testing.T, name string) ([]byte, ext4Oracle) {
	t.Helper()
	dir := filepath.Join("..", "filesys", "ext4", "testdata")
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
	var o ext4Oracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(img); got != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s != oracle %s: the oracle is stale, regenerate the fixtures", got, o.Generator.ImageSHA256)
	}
	return img, o
}

// TestExtractFromExt4Fixture imports a real mke2fs image as a whole-disk
// image (no partition table), extracts everything and compares each artifact
// with the oracle computed from the source tree.
func TestExtractFromExt4Fixture(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and extracts a 16 MiB image (about 300 artifacts)")
	}
	img, want := loadExt4Fixture(t, "ext4-4k-csum")
	c := newCase(t)
	imgSHA := sha256hex(img)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // default driver registry
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sum, err := s.Extract(context.Background(), examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	byPath := map[string]int{}
	var artifacts int
	for _, rec := range sum.Artifacts {
		if strings.HasSuffix(rec.Path, ".runs.jsonl") {
			continue // provenance sidecar of a heavily fragmented file
		}
		artifacts++
		d := rec.Source.Derived
		if rec.Source.Kind != "extract" || d == nil {
			t.Fatalf("artifact %s has no extract provenance: %+v", rec.Path, rec.Source)
		}
		if d.ParentSHA256 != imgSHA {
			t.Errorf("%s: Derived.ParentSHA256 = %s, want the imported image's %s", d.FSPath, d.ParentSHA256, imgSHA)
		}
		if d.FSType != "ext4" {
			t.Errorf("%s: Derived.FSType = %q, want ext4", d.FSPath, d.FSType)
		}
		if rec.Incomplete {
			t.Errorf("%s: flagged incomplete", d.FSPath)
		}
		byPath[d.FSPath]++
		var found bool
		for _, f := range want.Files {
			if f.Path != d.FSPath {
				continue
			}
			found = true
			if f.Type == "dir" {
				t.Errorf("%s: a directory was extracted as a file", f.Path)
			}
			if rec.SHA256 != f.SHA256 || rec.Size != f.Size {
				t.Errorf("%s: artifact sha256/size %s/%d, want %s/%d", f.Path, rec.SHA256, rec.Size, f.SHA256, f.Size)
			}
			if d.Mode&0o7777 != f.Mode {
				t.Errorf("%s: derived mode %04o, want %04o", f.Path, d.Mode&0o7777, f.Mode)
			}
			if got, wantT := d.Times["modified"], time.Unix(f.Mtime, 0).UTC().Format(time.RFC3339); got != wantT {
				t.Errorf("%s: derived modified %q, want %q", f.Path, got, wantT)
			}
		}
		if !found {
			t.Errorf("artifact for %s is not in the oracle", d.FSPath)
		}
	}

	wantFiles := 0
	for _, f := range want.Files {
		if f.Type == "dir" {
			continue
		}
		wantFiles++
		if byPath[f.Path] != 1 {
			t.Errorf("%s: %d artifacts, want 1", f.Path, byPath[f.Path])
		}
	}
	if artifacts != wantFiles || sum.Files != wantFiles {
		t.Errorf("artifacts = %d, Summary.Files = %d, want %d (every file and symlink of the oracle)", artifacts, sum.Files, wantFiles)
	}

	// The deleted entries are skipped with a warning each, and nothing else is.
	var skipped []string
	for _, w := range sum.Warnings {
		skipped = append(skipped, w.Path)
	}
	wantSkipped := append([]string(nil), want.Deleted...)
	sort.Strings(skipped)
	sort.Strings(wantSkipped)
	if strings.Join(skipped, "\n") != strings.Join(wantSkipped, "\n") || sum.Skipped != len(want.Deleted) {
		t.Errorf("skipped = %v (count %d), want exactly the deleted entries %v", skipped, sum.Skipped, wantSkipped)
	}

	verifyOK(t, c)
}
