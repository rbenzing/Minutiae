package fat_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

func direntIDOf(loc fattest.EntryLoc) string {
	return fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index)
}

func recImage(t *testing.T, o fattest.Options, files []fattest.File) (*fat.FS, []byte, *fattest.Built) {
	t.Helper()
	img, b := fattest.BuildLayout(o, files)
	fsys, err := fat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return fsys, img, b
}

// delEntry returns the listed Entry of the deleted file at path, found by the position BuildLayout reports.
func delEntry(t *testing.T, fsys *fat.FS, b *fattest.Built, path string) filesys.Entry {
	t.Helper()
	loc := b.Find(path, true)
	list, err := fsys.ReadDir(fsys.Root()) // the fixtures of this task keep deleted files in the root
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if e.ID == direntIDOf(loc) {
			return e
		}
	}
	t.Fatalf("deleted entry %q not listed", path)
	return filesys.Entry{}
}

// reopen opens a patched image again.
func reopen(t *testing.T, img []byte) *fat.FS {
	t.Helper()
	fsys, err := fat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func TestRecoverableContiguousFile(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 70) // 1120 bytes: 3 clusters of 512
	for _, typ := range []int{12, 16, 32} {
		o := fattest.Options{Type: typ}
		fsys, img, b := recImage(t, o, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
		loc := b.Find("/GONE.BIN", true)
		cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(loc)})
		if err != nil || len(cs) != 1 {
			t.Fatalf("FAT%d: %v, %d candidates", typ, err, len(cs))
		}
		c := cs[0]
		wantOff := b.Geometry.ClusterOffset(loc.FirstCluster)
		if c.Method != fat.MethodContiguous || c.Size != int64(len(data)) ||
			!reflect.DeepEqual(c.Runs, []filesys.Run{{Offset: wantOff, Length: int64(len(data))}}) {
			t.Errorf("FAT%d: %+v", typ, c)
		}
		if !slices.Contains(c.Assumptions, "contiguous-allocation") { // never claim more than the method knows
			t.Errorf("FAT%d: assumptions %v lack contiguous-allocation", typ, c.Assumptions)
		}
		if got := img[wantOff : wantOff+int64(len(data))]; !bytes.Equal(got, data) {
			t.Errorf("FAT%d: the map does not point at the data", typ)
		}
		if _, err := filesys.CheckCandidate(c, fsys.Info().Size); err != nil {
			t.Errorf("FAT%d: %v", typ, err)
		}
	}
}

func TestRecoverableCandidateStatesItsBasisAndTheEntryMetadata(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 700)
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	e := delEntry(t, fsys, b, "/GONE.BIN")
	cs, err := fsys.Recoverable(filesys.Entry{ID: e.ID})
	if err != nil || len(cs) != 1 {
		t.Fatal(err, len(cs))
	}
	c := cs[0]
	for _, want := range []string{fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index), fmt.Sprintf("first_cluster=%d", loc.FirstCluster)} {
		if !slices.Contains(c.Basis, want) {
			t.Errorf("basis %v lacks %q", c.Basis, want)
		}
	}
	if !slices.Contains(c.Assumptions, "size-from-directory-entry") {
		t.Errorf("assumptions %v lack size-from-directory-entry", c.Assumptions)
	}
	if c.Mode != e.Mode || !reflect.DeepEqual(c.Times, e.Times) || c.Encrypted {
		t.Errorf("mode %o times %+v encrypted %v; listing has %o %+v", c.Mode, c.Times, c.Encrypted, e.Mode, e.Times)
	}
}

func TestRecoverableReturnsFreshSlices(t *testing.T) {
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{1}, 900), Deleted: true}})
	id := direntIDOf(b.Find("/GONE.BIN", true))
	a, err := fsys.Recoverable(filesys.Entry{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(a[0].Runs)
	a[0].Runs[0].Length = 1
	a[0].Assumptions[0] = "x"
	a[0].Basis[0] = "x"
	c, err := fsys.Recoverable(filesys.Entry{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c[0].Runs, want) || c[0].Assumptions[0] == "x" || c[0].Basis[0] == "x" {
		t.Fatalf("an edit by the caller reached the next call: %+v", c[0])
	}
}

func TestRecoverableZeroSizeFileIsAnEmptyCandidate(t *testing.T) {
	data := bytes.Repeat([]byte{9}, 600)
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	zero := uint32(0)
	fattest.PatchEntry(img, b.Geometry, loc, nil, &zero)
	cs, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)})
	if err != nil || len(cs) != 1 {
		t.Fatal(err, len(cs))
	}
	if cs[0].Method != fat.MethodContiguous || cs[0].Size != 0 || len(cs[0].Runs) != 0 {
		t.Fatalf("%+v", cs[0])
	}
	// size 0 with first cluster 0 is still empty: nothing is read from it.
	first := uint32(0)
	fattest.PatchEntry(img, b.Geometry, loc, &first, nil)
	if cs, err = reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)}); err != nil || len(cs) != 1 || len(cs[0].Runs) != 0 {
		t.Fatalf("%v %+v", err, cs)
	}
}

func TestRecoverableFAT32UsesBothClusterHalves(t *testing.T) {
	_, img, b := recImage(t, fattest.Options{Type: 32}, []fattest.File{{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{3}, 600), Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	first, size := uint32(0x0001_0005), uint32(100)
	fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
	cs, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)})
	if err != nil || len(cs) != 1 {
		t.Fatal(err, len(cs))
	}
	want := []filesys.Run{{Offset: b.Geometry.ClusterOffset(0x10005), Length: 100}}
	if !reflect.DeepEqual(cs[0].Runs, want) {
		t.Fatalf("runs %+v, want %+v", cs[0].Runs, want)
	}
}

// fatGuard fails the test on any read that touches a FAT copy once armed.
type fatGuard struct {
	t     *testing.T
	r     *bytes.Reader
	spans [][2]int64 // [start, end) of every FAT copy
	armed atomic.Bool
}

func newFATGuard(t *testing.T, img []byte, g fattest.Geometry) *fatGuard {
	fg := &fatGuard{t: t, r: bytes.NewReader(img)}
	for n := 0; n < g.NumFATs; n++ {
		lo := int64(g.FATStart(n)) * int64(g.SectorSize)
		fg.spans = append(fg.spans, [2]int64{lo, lo + int64(g.FATSectors)*int64(g.SectorSize)})
	}
	return fg
}

func (g *fatGuard) ReadAt(p []byte, off int64) (int, error) {
	if g.armed.Load() {
		for _, s := range g.spans {
			if off < s[1] && off+int64(len(p)) > s[0] {
				g.t.Errorf("read of %d bytes at %d touches the FAT copy [%d, %d)", len(p), off, s[0], s[1])
			}
		}
	}
	return g.r.ReadAt(p, off)
}

// guardedRecover opens img on a reader that fails the test on any FAT read
// after Open and ReadDir(Root), and recovers the entry at loc.
func guardedRecover(t *testing.T, img []byte, g fattest.Geometry, loc fattest.EntryLoc) []filesys.Candidate {
	t.Helper()
	fg := newFATGuard(t, img, g)
	fsys, err := fat.Open(fg, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadDir(fsys.Root()); err != nil { // teaches the reader its directories (FAT32: the root chain)
		t.Fatal(err)
	}
	fg.armed.Store(true)
	cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(loc)})
	if err != nil || len(cs) != 1 {
		t.Fatalf("FAT%d: %v, %d candidates", g.Type, err, len(cs))
	}
	return cs
}

// TestRecoverableFATDoesNotConsultTheFAT: a read of any FAT copy during
// Recoverable fails the test (a read whose result is discarded included), and
// the map is the same whatever the FAT holds.
func TestRecoverableFATDoesNotConsultTheFAT(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 70)
	for _, typ := range []int{12, 16, 32} {
		img, b := fattest.BuildLayout(fattest.Options{Type: typ}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
		loc := b.Find("/GONE.BIN", true)
		want := guardedRecover(t, img, b.Geometry, loc)
		for i, c := range loc.Clusters {
			v := uint32(0xFF7 - i) // bad, then arbitrary values
			if typ == 32 {
				v = 0x0FFFFFF7 - uint32(i)
			}
			fattest.SetFATEntry(img, b.Geometry, c, v)
		}
		if got := guardedRecover(t, img, b.Geometry, loc); !reflect.DeepEqual(got, want) {
			t.Fatalf("FAT%d: the map changed with the FAT:\n%+v\n%+v", typ, got, want)
		}
		// A first cluster far from the FAT sectors the open and the listing
		// already read, so that a cached sector cannot hide a FAT read.
		far, size := b.Geometry.Clusters+1, uint32(100)
		fattest.PatchEntry(img, b.Geometry, loc, &far, &size)
		guardedRecover(t, img, b.Geometry, loc)
	}
}

func TestRecoverableLiveIsNotDeleted(t *testing.T) {
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{
		{Path: "/LIVE.BIN", Data: []byte("live")},
		{Path: "/SUB", Dir: true},
		{Path: "/SUB/X.BIN", Data: []byte("x")},
	})
	if _, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(b.Find("/LIVE.BIN", false))}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live file: %v", err)
	}
	sub := b.Find("/SUB", false)
	if _, err := fsys.Recoverable(filesys.Entry{ID: fmt.Sprintf("dir:%d", sub.FirstCluster)}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live dir: %v", err)
	}
	if _, err := fsys.Recoverable(fsys.Root()); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("root: %v", err)
	}
	if _, err := fsys.Recoverable(filesys.Entry{ID: "dir:60000"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("no such dir: %v", err)
	}
}

func TestRecoverableDeletedDirectoryIsUnsupported(t *testing.T) {
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONEDIR", Dir: true, Deleted: true}})
	_, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(b.Find("/GONEDIR", true))})
	if !errors.Is(err, filesys.ErrUnsupported) || !strings.Contains(err.Error(), "deleted directory") {
		t.Fatalf("deleted directory: %v", err)
	}
}

func TestRecoverableVolumeLabelIsUnsupported(t *testing.T) {
	img, b := fattest.BuildLayout(fattest.Options{Type: 16, Label: "MYVOL"}, []fattest.File{{Path: "/A.BIN", Data: []byte("a"), Deleted: true}})
	loc := b.Find("/A.BIN", true)
	rootOff := loc.Offset - int64(loc.Index)*32
	if img[rootOff+11]&0x18 != 0x08 {
		t.Fatalf("entry 0 is not the label: attr %#x", img[rootOff+11])
	}
	img[rootOff] = 0xE5 // a deleted label
	if _, err := reopen(t, img).Recoverable(filesys.Entry{ID: "dirent:0:0"}); !errors.Is(err, filesys.ErrUnsupported) {
		t.Fatalf("deleted volume label: %v", err)
	}
}

func TestRecoverableIDOfFreeSlotIsNotFound(t *testing.T) {
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: []byte("x"), Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	for _, idx := range []int{loc.Index + 1, loc.Index + 7, 511} {
		if _, err := fsys.Recoverable(filesys.Entry{ID: fmt.Sprintf("dirent:0:%d", idx)}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("slot %d: %v", idx, err)
		}
	}
}

func TestRecoverableLongNameEntryIsNotFound(t *testing.T) {
	fsys, _, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/a long deleted name.txt", Data: []byte("x"), Deleted: true, LongName: true}})
	loc := b.Find("/a long deleted name.txt", true)
	if loc.Index < 1 {
		t.Fatalf("no LFN entry before index %d", loc.Index)
	}
	if _, err := fsys.Recoverable(filesys.Entry{ID: fmt.Sprintf("dirent:0:%d", loc.Index-1)}); !errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("LFN entry: %v", err)
	}
}

// countReader counts ReadAt calls so a test can see that an ID was refused before any read.
type countReader struct {
	r *bytes.Reader
	n atomic.Int64
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(1)
	return c.r.ReadAt(p, off)
}

func TestRecoverableForgedIDIsNotFoundBeforeAnyRead(t *testing.T) {
	img, _ := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: []byte("x"), Deleted: true}})
	cr := &countReader{r: bytes.NewReader(img)}
	fsys, err := fat.Open(cr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	before := cr.n.Load()
	forged := []string{
		"", "x", "dirent:", "dirent:0", "dirent:0:", "dirent:00:1", "dirent:0:01", "dirent:-1:2", "dirent:0:+3",
		"dirent: 0:1", "dirent:0:1 ", "dirent:4294967296:1", "dirent:0:2147483648", "dirent:0:1:2",
		"dir:", "dir:01", "dir:-1", "inode:5", "dirent:0:99999999",
	}
	for _, id := range forged {
		if _, err := fsys.Recoverable(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if after := cr.n.Load(); after != before {
		t.Fatalf("%d reads happened for forged IDs", after-before)
	}
}

func TestRecoverableMapPastTheVolumeIsCorruptNotClamped(t *testing.T) {
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{5}, 600), Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	last := b.Geometry.Clusters + 1 // the last valid cluster
	cases := []struct {
		name        string
		first, size uint32
	}{
		{"two clusters from the last", last, 2 * 512},
		{"starts past the volume", last + 1, 10},
		{"first cluster 0", 0, 10},
		{"first cluster 1", 1, 10},
		{"huge size", 2, 0xFFFFFFFF},
	}
	for _, tc := range cases {
		first, size := tc.first, tc.size
		fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
		cs, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)})
		var ce *filesys.CorruptError
		if !errors.As(err, &ce) || len(cs) != 0 {
			t.Errorf("%s: %v, %d candidates", tc.name, err, len(cs))
		}
	}
	// The last cluster alone still maps.
	first, size := last, uint32(512)
	fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
	if cs, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)}); err != nil || len(cs) != 1 {
		t.Errorf("last cluster: %v %d", err, len(cs))
	}
}

func TestRecoverableMapPastATruncatedImageIsCorrupt(t *testing.T) {
	_, img, b := recImage(t, fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{5}, 600), Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	first, size := uint32(10), uint32(3*512) // clusters 10..12
	fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
	short := img[:b.Geometry.ClusterOffset(12)] // cluster 12 is cut off
	fsys, err := fat.Open(bytes.NewReader(short), int64(len(short)))
	if err != nil {
		t.Fatalf("truncated image does not open: %v", err)
	}
	cs, err := fsys.Recoverable(filesys.Entry{ID: direntIDOf(loc)})
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || len(cs) != 0 {
		t.Fatalf("%v, %d candidates", err, len(cs))
	}
}

// TestRecoverableSizeNotAClusterMultipleEndingInTheLastCluster: a size whose
// last partial cluster is the volume's last cluster maps; one byte more needs a
// further cluster and is corrupt (the cluster count rounds up, never down).
func TestRecoverableSizeNotAClusterMultipleEndingInTheLastCluster(t *testing.T) {
	for _, typ := range []int{12, 16, 32} {
		_, img, b := recImage(t, fattest.Options{Type: typ}, []fattest.File{{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{5}, 600), Deleted: true}})
		loc := b.Find("/GONE.BIN", true)
		cs := uint32(b.Geometry.SecPerClus * b.Geometry.SectorSize)
		first := b.Geometry.Clusters // the second last cluster (the last is Clusters+1)
		for _, tc := range []struct {
			size    uint32
			corrupt bool
		}{
			{2*cs - 1, false}, {2 * cs, false}, {cs + 1, false}, {2*cs + 1, true},
		} {
			size := tc.size
			fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
			got, err := reopen(t, img).Recoverable(filesys.Entry{ID: direntIDOf(loc)})
			var ce *filesys.CorruptError
			switch {
			case tc.corrupt && (!errors.As(err, &ce) || len(got) != 0):
				t.Errorf("FAT%d size %d: %v, %d candidates; want a *CorruptError", typ, size, err, len(got))
			case !tc.corrupt && (err != nil || len(got) != 1 || got[0].Size != int64(size)):
				t.Errorf("FAT%d size %d: %v %+v", typ, size, err, got)
			}
		}
	}
}
