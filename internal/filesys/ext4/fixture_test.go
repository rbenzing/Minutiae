package ext4_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
)

// oracleFile is one expected entry. The oracle (testdata/*.expect.json) is
// computed by tools/fixtures from the source tree handed to mke2fs and from
// dumpe2fs, never by Minutiae.
type oracleFile struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Mode       uint32 `json:"mode"`
	Mtime      int64  `json:"mtime"`
	LinkTarget string `json:"link_target"`
}

type oracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type          string       `json:"type"`
	Label         string       `json:"label"`
	UUID          string       `json:"uuid"`
	BlockSize     int          `json:"block_size"`
	Size          int64        `json:"size"`
	Features      []string     `json:"features"`
	Mke2fsCreated []string     `json:"mke2fs_created"`
	Files         []oracleFile `json:"files"`
	Deleted       []string     `json:"deleted"`
	// FreeBlocks are the free block ranges dumpe2fs reports, [first, last]
	// inclusive, sorted and merged.
	FreeBlocks [][2]int64 `json:"free_blocks"`
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
	if len(o.Files) == 0 || len(o.Deleted) == 0 {
		t.Fatal("oracle lists no files or no deleted entries")
	}
	if len(o.FreeBlocks) == 0 {
		t.Fatal("oracle lists no free blocks")
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

func TestExt4MatchesOracle(t *testing.T) {
	for _, path := range fixturePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		t.Run(name, func(t *testing.T) {
			want := loadOracle(t, path)
			img := gunzipFixture(t, path)
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s != oracle generator.image_sha256 %s: the oracle is stale, regenerate the fixtures", got, want.Generator.ImageSHA256)
			}
			if len(img) > 16<<20 {
				t.Errorf("fixture is %d bytes, over the 16 MiB budget", len(img))
			}

			fsys, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			checkInfo(t, fsys.Info(), want)

			live, deleted := walkAll(t, fsys)
			used := checkLive(t, fsys, want, live)
			checkDeleted(t, want, deleted)
			checkUnallocated(t, fsys, used, want)
		})
	}
}

func checkInfo(t *testing.T, info filesys.Info, want oracle) {
	t.Helper()
	if info.Type != want.Type || info.Label != want.Label || !strings.EqualFold(info.UUID, want.UUID) ||
		info.BlockSize != want.BlockSize || info.Size != want.Size {
		t.Errorf("Info = {%s %q %s block %d size %d}, want {%s %q %s block %d size %d}",
			info.Type, info.Label, info.UUID, info.BlockSize, info.Size,
			want.Type, want.Label, want.UUID, want.BlockSize, want.Size)
	}
	got, wantF := slices.Clone(info.Features), slices.Clone(want.Features)
	sort.Strings(got)
	sort.Strings(wantF)
	if !slices.Equal(got, wantF) {
		t.Errorf("features = %v, want (dumpe2fs) %v", got, wantF)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("warnings on a clean mke2fs image: %v", info.Warnings)
	}
}

// walkAll walks the whole tree and returns the live entries and the deleted
// entries by path.
func walkAll(t *testing.T, fsys *ext4.FS) (live, deleted map[string]filesys.Entry) {
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

// checkLive compares the live entries with the oracle and returns the on-disk
// runs (holes excluded) of every live file and symlink.
func checkLive(t *testing.T, fsys *ext4.FS, want oracle, live map[string]filesys.Entry) (used []filesys.Run) {
	t.Helper()
	wantSet := map[string]oracleFile{}
	for _, f := range want.Files {
		wantSet[f.Path] = f
	}
	// mke2fs creates these itself; they are not in the source tree.
	for _, p := range want.Mke2fsCreated {
		e, ok := live[p]
		if !ok || e.Type != filesys.TypeDir {
			t.Errorf("%s: want a directory created by mke2fs, got %+v (present %v)", p, e, ok)
		}
		delete(live, p)
		for q := range live {
			if strings.HasPrefix(q, p+"/") {
				t.Errorf("unexpected entry %s below %s", q, p)
				delete(live, q)
			}
		}
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

	kinds := map[string]filesys.EntryType{"file": filesys.TypeFile, "dir": filesys.TypeDir, "symlink": filesys.TypeSymlink}
	for _, w := range want.Files {
		e, ok := live[w.Path]
		if !ok {
			continue
		}
		if e.Type != kinds[w.Type] {
			t.Errorf("%s: type %s, want %s", w.Path, e.Type, w.Type)
			continue
		}
		if e.Mode&0o7777 != w.Mode {
			t.Errorf("%s: mode %04o, want %04o", w.Path, e.Mode&0o7777, w.Mode)
		}
		if got := e.Times.Modified.T.Unix(); got != w.Mtime {
			t.Errorf("%s: mtime %d, want %d", w.Path, got, w.Mtime)
		}
		if w.Type == "dir" {
			continue
		}
		if e.Size != w.Size {
			t.Errorf("%s: entry size %d, want %d", w.Path, e.Size, w.Size)
		}
		if w.Type == "symlink" && e.LinkTarget != w.LinkTarget {
			t.Errorf("%s: link target %q, want %q", w.Path, e.LinkTarget, w.LinkTarget)
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
		// Content stored in the inode (inline data, a fast symlink target) has no
		// on-disk run; everything else must account for exactly its size.
		runs := f.Runs()
		if len(runs) > 0 {
			for _, r := range runs {
				if r.Offset >= 0 {
					used = append(used, r)
				}
			}
			if err := filesys.CheckRuns(runs, f.Size(), fsys.Info().Size); err != nil {
				t.Errorf("%s: runs: %v", w.Path, err)
			}
		} else if w.Type == "file" && w.Size > 0 && !slices.Contains(want.Features, "inline_data") {
			t.Errorf("%s: no runs for a %d-byte file without inline data", w.Path, w.Size)
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
	// Informational only: the oracle names the deletions the generator made;
	// other stale records may legitimately remain in directory blocks.
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

// checkUnallocated checks the Unallocated contract and that no free run overlaps
// the content of a live file or a group bitmap (catches an inverted bitmap).
func checkUnallocated(t *testing.T, fsys *ext4.FS, used []filesys.Run, want oracle) {
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
	if total <= 0 {
		t.Error("no unallocated space reported")
	}

	// Exactly the blocks dumpe2fs reports free, no more and no fewer.
	var wantRuns []filesys.Run
	for _, r := range want.FreeBlocks {
		wantRuns = append(wantRuns, filesys.Run{Offset: r[0] * int64(want.BlockSize), Length: (r[1] - r[0] + 1) * int64(want.BlockSize)})
	}
	if !slices.Equal(runs, filesys.MergeRuns(wantRuns)) {
		t.Errorf("Unallocated differs from the dumpe2fs free blocks:\n got  %v\n want %v", runs, filesys.MergeRuns(wantRuns))
	}

	bs := int64(fsys.Info().BlockSize)
	overlaps := func(what string, off, length int64) {
		i := sort.Search(len(runs), func(i int) bool { return runs[i].Offset+runs[i].Length > off })
		if i < len(runs) && runs[i].Offset < off+length {
			t.Errorf("free run %+v overlaps %s at [%d, %d)", runs[i], what, off, off+length)
		}
	}
	for _, r := range used {
		overlaps("the content of a live file", r.Offset, r.Length)
	}
	for g := range fsys.GroupCount() {
		bb, ib, it, _, _ := fsys.Group(g)
		overlaps(fmt.Sprintf("the block bitmap of group %d", g), int64(bb)*bs, bs)
		overlaps(fmt.Sprintf("the inode bitmap of group %d", g), int64(ib)*bs, bs)
		overlaps(fmt.Sprintf("the first inode table block of group %d", g), int64(it)*bs, bs)
	}
}
