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

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// fatOracle is the part of internal/filesys/{fat,exfat}/testdata/*.expect.json
// used here. The oracle comes from the source tree the image was populated
// from, and from fsck.fat/mshowfat (FAT) or dump.exfat (exFAT), never from
// Minutiae (see tools/fixtures/fat.sh and exfat.sh).
type fatOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type      string `json:"type"`
	BlockSize int64  `json:"block_size"`
	DataStart int64  `json:"data_start"`
	Files     []struct {
		Path   string `json:"path"`
		Type   string `json:"type"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		Mtime  int64  `json:"mtime"`
	} `json:"files"`
	Deleted      []string   `json:"deleted"`
	FreeClusters [][2]int64 `json:"free_clusters"`
}

func loadFATFixture(t *testing.T, pkg, name string) ([]byte, fatOracle) {
	t.Helper()
	dir := filepath.Join("..", "filesys", pkg, "testdata")
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
	var o fatOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if got := sha256hex(img); got != o.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s != oracle %s: the oracle is stale, regenerate the fixtures", got, o.Generator.ImageSHA256)
	}
	return img, o
}

// extractFixture imports a real image as a whole-disk image (no partition
// table), extracts everything with the default driver registry and compares each
// artifact with the oracle; it then exports the unallocated space and checks it
// against the oracle's free clusters, and verifies the case. zoneKnown says
// whether the filesystem stores a UTC offset (exFAT) or not (FAT: the local time
// is shown as stored, without a zone suffix).
func extractFixture(t *testing.T, pkg, name, fsType string, zoneKnown bool) {
	t.Helper()
	img, want := loadFATFixture(t, pkg, name)
	c := newCase(t)
	imgSHA := sha256hex(img)
	recs := importImage(t, c, img, 1)
	s, err := examine.Open(c, recs[0].ID, examine.Options{}) // default driver registry
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if want.Type != fsType {
		t.Fatalf("oracle type %q, want %q", want.Type, fsType)
	}

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
		if d.FSType != fsType || !strings.Contains(rec.Path, "/p0-"+fsType+"/") {
			t.Errorf("%s: Derived.FSType = %q, path %q, want %s", d.FSPath, d.FSType, rec.Path, fsType)
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
			wantT := time.Unix(f.Mtime, 0).UTC().Format(time.RFC3339)
			if !zoneKnown {
				wantT = strings.TrimSuffix(wantT, "Z") // the stored local time, no zone claimed
			}
			if got := d.Times["modified"]; got != wantT {
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
		t.Errorf("artifacts = %d, Summary.Files = %d, want %d (every file of the oracle)", artifacts, sum.Files, wantFiles)
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
	if sum.FSWarnings != 0 {
		t.Errorf("FSWarnings = %d on a clean image", sum.FSWarnings)
	}

	// image unalloc -p: the export is exactly the free clusters of the oracle.
	var wantRuns []evidence.Run
	for _, r := range want.FreeClusters {
		wantRuns = append(wantRuns, evidence.Run{
			Offset: want.DataStart + (r[0]-2)*want.BlockSize,
			Length: (r[1] - r[0] + 1) * want.BlockSize,
		})
	}
	usum, err := s.ExportUnallocated(context.Background(), examine.UnallocOptions{Partition: -1})
	if err != nil {
		t.Fatalf("ExportUnallocated: %v", err)
	}
	if usum.FSWarnings != 0 || usum.Skipped != 0 {
		t.Errorf("unalloc summary = %+v", usum)
	}
	checkUnalloc(t, c, img, usum, wantRuns, "p0-"+fsType)
	verifyOK(t, c)
}

// TestExtractFromFAT32Fixture imports a real mkfs.fat/mtools image and
// extracts it: hashes, sizes and times equal the oracle computed from the
// source tree, deleted entries are skipped, the unallocated export is exactly
// the free clusters mshowfat leaves, and case verify is OK.
func TestExtractFromFAT32Fixture(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and extracts a 34 MiB image (about 140 artifacts)")
	}
	extractFixture(t, "fat", "fat32", "fat32", false)
}

// TestExtractFromExfatFixture does the same for the exFAT image populated
// through exfat-fuse and described by dump.exfat.
func TestExtractFromExfatFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and extracts a 16 MiB image (about 140 artifacts)")
	}
	extractFixture(t, "exfat", "exfat", "exfat", true)
}

// A zero-length FAT file has no clusters: it is extracted as an empty artifact
// with no recorded runs, and neither the file nor the reader raises a warning.
func TestExtractFATZeroLengthFile(t *testing.T) {
	img := fattest.Build(fattest.Options{Type: 16}, []fattest.File{
		{Path: "EMPTY.TXT"},
		{Path: "FULL.TXT", Data: []byte("full")},
	})
	s, _ := fatSession(t, img)
	c := s.Case
	sum := extractAll(t, s, examine.ExtractOptions{Partition: -1, Paths: []string{"/"}, Recursive: true})
	if sum.Files != 2 || sum.Skipped != 0 || sum.FSWarnings != 0 || len(sum.Warnings) != 0 || len(sum.Artifacts) != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	for _, rec := range sum.Artifacts {
		d := rec.Source.Derived
		switch {
		case strings.HasSuffix(rec.Source.RemotePath, "EMPTY.TXT"):
			if rec.Size != 0 || rec.Incomplete || d == nil || d.Runs != nil || d.RunsArtifact != "" {
				t.Errorf("empty file: size %d incomplete %v derived %+v, want an empty artifact with nil runs", rec.Size, rec.Incomplete, d)
			}
		case strings.HasSuffix(rec.Source.RemotePath, "FULL.TXT"):
			if string(readArtifact(t, c, rec)) != "full" || len(d.Runs) == 0 {
				t.Errorf("full file: %+v", rec)
			}
		default:
			t.Errorf("unexpected artifact %q", rec.Source.RemotePath)
		}
	}
	if w := auditByAction(t, c, "analysis.warning"); len(w) != 0 {
		t.Errorf("analysis.warning entries for a clean image: %v", w)
	}
	verifyOK(t, c)
}
