package exfat_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
)

// oracleFile is one expected entry. The oracle (testdata/*.expect.json) is
// computed by tools/fixtures from the source tree the image was populated from
// and from dump.exfat (exfatprogs), never by Minutiae.
type oracleFile struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mtime  int64  `json:"mtime"`
	// Clusters are the inclusive cluster ranges of the chain dump.exfat reports
	// (empty for a file without clusters).
	Clusters [][2]int64 `json:"clusters"`
}

type oracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type         string       `json:"type"`
	Label        string       `json:"label"`
	UUID         string       `json:"uuid"`
	BlockSize    int          `json:"block_size"` // bytes per cluster
	Size         int64        `json:"size"`
	DataStart    int64        `json:"data_start"` // byte offset of cluster 2
	ClusterCount int64        `json:"cluster_count"`
	Files        []oracleFile `json:"files"`
	Deleted      []string     `json:"deleted"`
	// FreeCount and FreeClusters are the clusters no live chain holds
	// (inclusive ranges, sorted), checked against dump.exfat's "Free Clusters".
	FreeCount    int64      `json:"free_count"`
	FreeClusters [][2]int64 `json:"free_clusters"`
}

func fixturePaths(t testing.TB) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.img.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in testdata (glob err %v)", err)
	}
	return paths
}

// gunzipFixture returns the decompressed image of a *.img.gz fixture.
func gunzipFixture(t testing.TB, path string) []byte {
	t.Helper()
	gz, err := os.ReadFile(path)
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
	return img
}

func loadOracle(t testing.TB, imgGz string) oracle {
	t.Helper()
	raw, err := os.ReadFile(strings.TrimSuffix(imgGz, ".img.gz") + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o oracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.Files) == 0 || len(o.Deleted) == 0 || len(o.FreeClusters) == 0 || o.DataStart == 0 {
		t.Fatal("oracle lists no files, deleted entries, free clusters or data start")
	}
	return o
}

func readFile(t *testing.T, f filesys.File) []byte {
	t.Helper()
	buf := make([]byte, f.Size())
	n, err := f.ReadAt(buf, 0)
	if n != len(buf) || (err != nil && err != io.EOF) {
		t.Fatalf("ReadAt = %d, %v; want %d bytes", n, err, len(buf))
	}
	return buf
}

func TestExfatMatchesOracle(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s != oracle generator.image_sha256 %s: the oracle is stale, regenerate the fixtures", got, want.Generator.ImageSHA256)
			}
			if len(img) > 64<<20 {
				t.Errorf("fixture is %d bytes, over the 64 MiB budget", len(img))
			}

			fsys, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			checkInfo(t, fsys.Info(), want)

			live, deleted := walkFixture(t, fsys)
			used := checkLive(t, fsys, want, live)
			checkDeleted(t, want, deleted)
			checkUnallocated(t, fsys, used, want)
			if w := fsys.Info().Warnings; len(w) != 0 {
				t.Errorf("warnings after reading a clean mkfs.exfat image: %v", w)
			}
		})
	}
}

func checkInfo(t *testing.T, info filesys.Info, want oracle) {
	t.Helper()
	if info.Type != want.Type || info.Label != want.Label || info.UUID != want.UUID ||
		info.BlockSize != want.BlockSize || info.Size != want.Size {
		t.Errorf("Info = {%s %q %s cluster %d size %d}, want {%s %q %s cluster %d size %d}",
			info.Type, info.Label, info.UUID, info.BlockSize, info.Size,
			want.Type, want.Label, want.UUID, want.BlockSize, want.Size)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("warnings on a clean mkfs.exfat image: %v", info.Warnings)
	}
	for _, f := range info.Features {
		if f == "volume dirty" || f == "media failure" {
			t.Errorf("feature %q on a cleanly unmounted image", f)
		}
	}
}

// walkFixture walks the whole tree and returns the live and the deleted entries by path.
func walkFixture(t *testing.T, fsys *exfat.FS) (live, deleted map[string]filesys.Entry) {
	t.Helper()
	live, deleted = map[string]filesys.Entry{}, map[string]filesys.Entry{}
	err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, err error) error {
		if err != nil {
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		if e.Deleted {
			deleted[p] = e
		} else {
			live[p] = e
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return live, deleted
}

// chainRuns converts inclusive cluster ranges into the byte runs of a file of
// size bytes: the chain's clusters in order, cut at the file size.
func chainRuns(w oracleFile, want oracle) []filesys.Run {
	var runs []filesys.Run
	left := w.Size
	for _, c := range w.Clusters {
		length := (c[1] - c[0] + 1) * int64(want.BlockSize)
		if length > left {
			length = left
		}
		if length <= 0 {
			break
		}
		runs = append(runs, filesys.Run{Offset: want.DataStart + (c[0]-2)*int64(want.BlockSize), Length: length})
		left -= length
	}
	return filesys.MergeRuns(runs)
}

// checkLive compares the live entries with the oracle and returns the on-disk
// runs of every live file.
func checkLive(t *testing.T, fsys *exfat.FS, want oracle, live map[string]filesys.Entry) (used []filesys.Run) {
	t.Helper()
	wantSet := map[string]oracleFile{}
	fragmented := false
	for _, f := range want.Files {
		wantSet[f.Path] = f
		if f.Type == "file" && len(f.Clusters) > 1 {
			fragmented = true
		}
	}
	if !fragmented {
		t.Error("the oracle has no fragmented file: the fixture lost its point")
	}
	for p := range wantSet {
		if _, ok := live[p]; !ok {
			t.Errorf("missing live entry %s", p)
		}
	}
	for p, e := range live {
		if _, ok := wantSet[p]; !ok {
			t.Errorf("unexpected live entry %s (%s)", p, e.Type)
		}
	}

	kinds := map[string]filesys.EntryType{"file": filesys.TypeFile, "dir": filesys.TypeDir}
	for _, w := range want.Files {
		e, ok := live[w.Path]
		if !ok {
			continue
		}
		if e.Type != kinds[w.Type] {
			t.Errorf("%s: type %s, want %s", w.Path, e.Type, w.Type)
			continue
		}
		// exFAT keeps 2-second times with a UTC offset (here 0, valid): the
		// source mtimes are even, so the instant is exact and the zone known.
		if got := e.Times.Modified.T.Unix(); got != w.Mtime {
			t.Errorf("%s: mtime %d, want %d", w.Path, got, w.Mtime)
		}
		if !e.Times.Modified.ZoneKnown {
			t.Errorf("%s: modified time has no known zone although the UTC offset is valid", w.Path)
		}
		if w.Type == "dir" {
			continue
		}
		if e.Size != w.Size {
			t.Errorf("%s: entry size %d, want %d", w.Path, e.Size, w.Size)
		}
		f, err := fsys.Open(e)
		if err != nil {
			t.Errorf("%s: Open: %v", w.Path, err)
			continue
		}
		if f.Size() != w.Size {
			t.Errorf("%s: file size %d, want %d", w.Path, f.Size(), w.Size)
			continue
		}
		runs := f.Runs()
		if w.Size == 0 {
			if len(runs) != 0 {
				t.Errorf("%s: runs %v for an empty file", w.Path, runs)
			}
		} else {
			if err := filesys.CheckRuns(runs, f.Size(), fsys.Info().Size); err != nil {
				t.Errorf("%s: runs: %v", w.Path, err)
			}
			// The runs are exactly the clusters dump.exfat reports for the chain.
			if wantRuns := chainRuns(w, want); !slices.Equal(filesys.MergeRuns(slices.Clone(runs)), wantRuns) {
				t.Errorf("%s: runs\n got  %v\n want %v (the dump.exfat chain %v)", w.Path, runs, wantRuns, w.Clusters)
			}
			used = append(used, runs...)
		}
		sum := sha256.Sum256(readFile(t, f))
		if got := hex.EncodeToString(sum[:]); got != w.SHA256 {
			t.Errorf("%s: sha256 %s, want %s", w.Path, got, w.SHA256)
		}
	}
	return used
}

func checkDeleted(t *testing.T, want oracle, deleted map[string]filesys.Entry) {
	t.Helper()
	for _, p := range want.Deleted {
		e, ok := deleted[p]
		if !ok {
			t.Errorf("deleted entry %s not reported (deleted entries: %v)", p, keys(deleted))
			continue
		}
		if !e.Deleted || e.Name != p[strings.LastIndex(p, "/")+1:] {
			t.Errorf("deleted entry %s = %+v", p, e)
		}
	}
	// Informational only: the oracle names the deletions the generator made.
	for p := range deleted {
		if !slices.Contains(want.Deleted, p) {
			t.Logf("extra deleted entry reported: %s", p)
		}
	}
}

func keys(m map[string]filesys.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkUnallocated checks that Unallocated is exactly the free clusters (the
// ones no live chain, bitmap, up-case table or root directory holds, with the
// count cross-checked against dump.exfat when the oracle was made) and that no
// free run overlaps live content.
func checkUnallocated(t *testing.T, fsys *exfat.FS, used []filesys.Run, want oracle) {
	t.Helper()
	runs, err := fsys.Unallocated()
	if err != nil {
		t.Fatalf("Unallocated: %v", err)
	}
	var total, prevEnd int64
	for _, r := range runs {
		if r.Length <= 0 || r.Offset < prevEnd || r.Offset+r.Length > fsys.Info().Size {
			t.Fatalf("bad unallocated run %+v after %d (fs size %d)", r, prevEnd, fsys.Info().Size)
		}
		prevEnd = r.Offset + r.Length
		total += r.Length
	}
	if total != want.FreeCount*int64(want.BlockSize) {
		t.Errorf("unallocated total %d bytes, want %d free clusters x %d", total, want.FreeCount, want.BlockSize)
	}

	var wantRuns []filesys.Run
	for _, c := range want.FreeClusters {
		wantRuns = append(wantRuns, filesys.Run{
			Offset: want.DataStart + (c[0]-2)*int64(want.BlockSize),
			Length: (c[1] - c[0] + 1) * int64(want.BlockSize),
		})
	}
	if !slices.Equal(runs, filesys.MergeRuns(wantRuns)) {
		t.Errorf("Unallocated differs from the free clusters of the oracle:\n got  %v\n want %v", runs, filesys.MergeRuns(wantRuns))
	}

	for _, r := range used {
		i := sort.Search(len(runs), func(i int) bool { return runs[i].Offset+runs[i].Length > r.Offset })
		if i < len(runs) && runs[i].Offset < r.Offset+r.Length {
			t.Errorf("free run %+v overlaps the content of a live file at %+v", runs[i], r)
		}
	}
}
