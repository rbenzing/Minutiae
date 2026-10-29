package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// recImageWith builds an image with BuildLayout, lets mutate edit it (every checksum kept valid by the layout
// helpers), and opens it.
func recImageWith(t *testing.T, o ext4test.Options, files []ext4test.File, mutate func(img []byte, l *ext4test.Layout)) (*ext4.FS, []byte, *ext4test.Layout) {
	t.Helper()
	img, l := ext4test.BuildLayout(o, files)
	if mutate != nil {
		mutate(img, l)
	}
	return openImg(t, img), img, l
}

// delEntry finds the listed deleted entry of path in its parent directory. A first-in-block record that has lost its
// name and inode is found as the parent's only deleted entry.
func delEntry(t *testing.T, fsys *ext4.FS, path string) filesys.Entry {
	t.Helper()
	i := strings.LastIndex(path, "/")
	parent, name := path[:i], path[i+1:]
	if parent == "" {
		parent = "/"
	}
	var only []filesys.Entry
	for _, e := range readDir(t, fsys, parent) {
		if !e.Deleted {
			continue
		}
		if e.Name == name {
			return e
		}
		only = append(only, e)
	}
	if len(only) == 1 {
		return only[0]
	}
	t.Fatalf("no deleted entry %q in %q", name, parent)
	return filesys.Entry{}
}

// concatRuns is the bytes the runs name, in order.
func concatRuns(img []byte, runs []filesys.Run) []byte {
	var out []byte
	for _, r := range runs {
		out = append(out, img[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

// hasStop reports whether c carries a stale-chain-stop assumption.
func hasStop(c filesys.Candidate) bool { return stopOf(c) != "" }

func stopOf(c filesys.Candidate) string {
	for _, a := range c.Assumptions {
		if v, ok := strings.CutPrefix(a, "stale-chain-stop="); ok {
			return v
		}
	}
	return ""
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed ^ byte(i>>8)
	}
	return b
}

func coveredBytes(c filesys.Candidate) int64 {
	var n int64
	for _, r := range c.Runs {
		n += r.Length
	}
	return n
}

func oneCandidate(t *testing.T, fsys *ext4.FS, path string) filesys.Candidate {
	t.Helper()
	cs, err := fsys.Recoverable(delEntry(t, fsys, path))
	if err != nil || len(cs) != 1 {
		t.Fatalf("Recoverable(%s) = %d candidates, %v; want one", path, len(cs), err)
	}
	return cs[0]
}

var csumExt = ext4test.Options{Extents: true, MetadataCsum: true}

func TestRecoverableIntactExtentFile(t *testing.T) {
	data := pattern(5*1024+300, 1)
	o := ext4test.Options{Extents: true, MetadataCsum: true}
	fsys, img, _ := recImageWith(t, o, []ext4test.File{{Path: "/gone.bin", Data: data, Deleted: true}}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	cs, err := fsys.Recoverable(delEntry(t, fsys, "/gone.bin"))
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d candidates", err, len(cs))
	}
	c := cs[0]
	if c.Method != ext4.MethodInodeIntact || c.Size != int64(len(data)) ||
		!slices.Contains(c.Assumptions, "inode-not-reused-since-entry-deleted") || hasStop(c) {
		t.Fatalf("%+v", c)
	}
	if !slices.Contains(c.Assumptions, "size-from-inode") {
		t.Errorf("assumptions %q lack size-from-inode", c.Assumptions)
	}
	if got := concatRuns(img, c.Runs); !bytes.Equal(got, data) { // the map points at the data, nothing invented
		t.Fatal("runs do not reproduce the file")
	}
	if _, err := filesys.CheckCandidate(c, fsys.Info().Size); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverableIntactBlockMapWithIndirect(t *testing.T) {
	data := pattern(300*1024, 2) // 12 direct, 256 single indirect, 32 through the double indirect
	fsys, img, l := recImageWith(t, ext4test.Options{}, []ext4test.File{{Path: "/gone.bin", Data: data, Deleted: true}}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if c.Method != ext4.MethodInodeIntact || c.Size != int64(len(data)) || hasStop(c) {
		t.Fatalf("%+v", c)
	}
	if got := concatRuns(img, c.Runs); !bytes.Equal(got, data) {
		t.Fatal("runs do not reproduce the file")
	}
	if !slices.Contains(c.Basis, "map=blockmap") {
		t.Errorf("basis %q does not name the map kind", c.Basis)
	}
	tree := l.File("/gone.bin").Tree
	if len(tree) != 3 {
		t.Fatalf("the layout holds %d tree blocks, want 3", len(tree))
	}
	for _, blk := range tree {
		if want := fmt.Sprintf("tree-block=%d", blk); !slices.Contains(c.Basis, want) {
			t.Errorf("basis %q lacks %s", c.Basis, want)
		}
	}
	if !strings.HasPrefix(c.Basis[0], "dirent:") {
		t.Errorf("basis[0] = %q, want the entry id", c.Basis[0])
	}
}

// twoLeafFile is 6 blocks in two scattered pieces under a depth-1 tree with two leaves.
func twoLeafFile() (ext4test.File, []byte) {
	data := pattern(6*1024, 3)
	return ext4test.File{
		Path: "/gone.bin", Deleted: true, ExtentLeaves: 2, Scatter: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*1024]}, {Block: 3, Data: data[3*1024:]}},
	}, data
}

func TestRecoverableIntactDepthOneTree(t *testing.T) {
	f, data := twoLeafFile()
	fsys, img, l := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if hasStop(c) || c.Size != int64(len(data)) || len(c.Runs) != 2 {
		t.Fatalf("%+v", c)
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data) {
		t.Fatal("runs do not reproduce the file")
	}
	for _, blk := range l.File("/gone.bin").Tree {
		if want := fmt.Sprintf("tree-block=%d", blk); !slices.Contains(c.Basis, want) {
			t.Errorf("basis %q lacks %s", c.Basis, want)
		}
	}
}

func TestRecoverableStopsAtReusedMapBlock(t *testing.T) {
	f, data := twoLeafFile()
	fsys, img, l := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		l.SetBlockFree(img, l.File("/gone.bin").Tree[1], false) // the second leaf now belongs to someone else
	})
	_ = l
	c := oneCandidate(t, fsys, "/gone.bin")
	if got := stopOf(c); got != "map-block-not-free@3" {
		t.Fatalf("stop = %q, want map-block-not-free@3 (%+v)", got, c)
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data[:3*1024]) {
		t.Errorf("runs cover %d bytes, want exactly the first leaf's 3 blocks", coveredBytes(c))
	}
	if c.Size != int64(len(data)) || coveredBytes(c) >= c.Size {
		t.Errorf("size %d covered %d: the prefix must be shorter than the file", c.Size, coveredBytes(c))
	}
}

func TestRecoverableStopsAtHole(t *testing.T) {
	data := pattern(5*1024, 4)
	f := ext4test.File{
		Path: "/gone.bin", Deleted: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*1024]}, {Block: 6, Data: data[3*1024:]}},
	}
	for _, o := range []ext4test.Options{csumExt, {}} { // extents and a block map
		fsys, img, _ := recImageWith(t, o, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
			l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		})
		c := oneCandidate(t, fsys, "/gone.bin")
		if got := stopOf(c); got != "hole@3" {
			t.Fatalf("extents=%v: stop = %q, want hole@3", o.Extents, got)
		}
		if !bytes.Equal(concatRuns(img, c.Runs), data[:3*1024]) {
			t.Errorf("extents=%v: the runs are not exactly the blocks before the hole (nothing is filled)", o.Extents)
		}
		if c.Size != 8*1024 {
			t.Errorf("extents=%v: size = %d, want i_size 8192", o.Extents, c.Size)
		}
	}
}

func TestRecoverableStopsAtUninitExtent(t *testing.T) {
	data := pattern(5*1024, 5)
	f := ext4test.File{
		Path: "/gone.bin", Deleted: true,
		Pieces: []ext4test.Piece{{Block: 0, Data: data[:3*1024]}, {Block: 3, Data: data[3*1024:], Uninit: true}},
	}
	fsys, img, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if got := stopOf(c); got != "uninit@3" {
		t.Fatalf("stop = %q, want uninit@3", got)
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data[:3*1024]) {
		t.Error("the preallocated extent must not be read as data")
	}
}

func TestRecoverableLeafChecksumMismatchStops(t *testing.T) {
	f, data := twoLeafFile()
	fsys, img, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		tail := 12 + 12*((1024-12)/12) // the le32 right after the last possible entry
		img[l.File("/gone.bin").Tree[1]*1024+tail] ^= 0xFF
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if got := stopOf(c); got != "map-block-checksum@3" {
		t.Fatalf("stop = %q, want map-block-checksum@3", got)
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data[:3*1024]) {
		t.Error("the prefix before the damaged leaf is the first leaf's blocks")
	}
}

func TestRecoverableKernelUnlinkedSmallFileHasNoCandidate(t *testing.T) {
	data := pattern(2*1024, 6)
	kernel := ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true}
	cases := []struct {
		name string
		o    ext4test.Options
		e    ext4test.UnlinkEffects
	}{
		{"extents, size zeroed, root cleared", csumExt, kernel},
		{"extents, size kept, root cleared", csumExt, ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ClearExtentRoot: true}},
		{"block map, pointers zeroed", ext4test.Options{}, ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlockPointers: true}},
		{"block map, size kept, pointers zeroed", ext4test.Options{}, ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroBlockPointers: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys, _, _ := recImageWith(t, tc.o, []ext4test.File{{Path: "/gone.bin", Data: data, Deleted: true}}, func(img []byte, l *ext4test.Layout) {
				l.Unlink(img, "/gone.bin", tc.e)
			})
			prepare(t, fsys) // no leaf of this inode is in free space: the answer is then nothing, not unknown
			cs, err := fsys.Recoverable(delEntry(t, fsys, "/gone.bin"))
			if err != nil || cs != nil {
				t.Fatalf("Recoverable = %+v, %v; want nil, nil (the map is gone, nothing to claim)", cs, err)
			}
		})
	}
}

// The reader never checks the DATA runs against free space: that is the caller's cut (3A contract).
func TestRecoverableDataRunsAreNotCheckedAgainstFreeSpace(t *testing.T) {
	data := pattern(4*1024, 7)
	files := []ext4test.File{{Path: "/gone.bin", Data: data, Deleted: true}}
	base, _, _ := recImageWith(t, csumExt, files, func(img []byte, l *ext4test.Layout) { l.Unlink(img, "/gone.bin", ext4test.DebugfsRm) })
	want := oneCandidate(t, base, "/gone.bin")
	reused, _, _ := recImageWith(t, csumExt, files, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		l.SetBlockFree(img, l.File("/gone.bin").Data[1], false) // a data block allocated again
	})
	got := oneCandidate(t, reused, "/gone.bin")
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("a reused data block changed the candidate:\n got %+v\nwant %+v", got, want)
	}
}

func TestRecoverableLiveAndForged(t *testing.T) {
	files := []ext4test.File{{Path: "/live.txt", Data: []byte("live")}, {Path: "/gone.bin", Data: pattern(2048, 8), Deleted: true}}
	img, l := ext4test.BuildLayout(csumExt, files)
	l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	f, rr := openLoc(t, img)
	before := rr.calls.Load()
	if _, err := f.Recoverable(filesys.Entry{ID: "inode:12"}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("inode:12 = %v, want ErrNotDeleted", err)
	}
	for _, id := range []string{"dirent:01:4", "inode:07", "dirent:5:3", "", "nid:7", "inode:99999999"} {
		if _, err := f.Recoverable(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%q = %v, want ErrNotFound", id, err)
		}
	}
	if n := rr.calls.Load() - before; n != 0 {
		t.Errorf("live and forged ids made %d reads, want 0", n)
	}
	// Only the ID counts: a forged Deleted, Size or Name on the entry changes nothing.
	e := delEntry(t, f, "/gone.bin")
	e.Name, e.Size, e.Deleted = "other", 1, false
	if cs, err := f.Recoverable(e); err != nil || len(cs) != 1 {
		t.Errorf("forged fields: %d candidates, %v", len(cs), err)
	}
}

func TestRecoverableTimesAndModeFromStaleInode(t *testing.T) {
	f := ext4test.File{
		Path: "/gone.bin", Data: pattern(2048, 9), Deleted: true, Mode: 0o640,
		Times: [4]int64{1600000001, 1600000002, 1600000003, 1600000004}, // atime, ctime, mtime, crtime
	}
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if c.Mode&0o7777 != 0o640 || c.Mode&0xF000 != 0x8000 {
		t.Errorf("mode = %#o, want a regular file 0640", c.Mode)
	}
	if got := c.Times.Modified.T.Unix(); got != 1600000003 {
		t.Errorf("mtime = %d", got)
	}
	if got := c.Times.Accessed.T.Unix(); got != 1600000001 {
		t.Errorf("atime = %d", got)
	}
	if got := c.Times.Changed.T.Unix(); got != 1600000002 {
		t.Errorf("ctime = %d", got)
	}
	if c.Times.Deleted.T.IsZero() {
		t.Error("the deletion time of the stale inode is lost")
	}
}

func TestRecoverableEncryptedIsCiphertextCandidate(t *testing.T) {
	o := ext4test.Options{Extents: true, MetadataCsum: true, Encrypt: true}
	data := pattern(3*1024, 10)
	fsys, img, _ := recImageWith(t, o, []ext4test.File{{Path: "/enc", Dir: true}, {Path: "/enc/x", Data: data, Deleted: true}}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/enc/x", ext4test.DebugfsRm)
	})
	c := oneCandidate(t, fsys, "/enc/x")
	if !c.Encrypted {
		t.Error("Encrypted is false for an inode with the encrypt flag")
	}
	if !bytes.Equal(concatRuns(img, c.Runs), data) {
		t.Error("the runs must name the on-disk (cipher) bytes")
	}
}

func TestRecoverableUncleanImageAssumesJournalNotReplayed(t *testing.T) {
	files := []ext4test.File{{Path: "/gone.bin", Data: pattern(3*1024, 11), Deleted: true}}
	clean, _, _ := recImageWith(t, csumExt, files, func(img []byte, l *ext4test.Layout) { l.Unlink(img, "/gone.bin", ext4test.DebugfsRm) })
	if c := oneCandidate(t, clean, "/gone.bin"); slices.Contains(c.Assumptions, "journal-not-replayed") {
		t.Fatalf("a clean image carries journal-not-replayed: %q", c.Assumptions)
	}
	fsys, _, _ := recImageWith(t, csumExt, files, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		l.PatchSuper(img, func(sb []byte) { putLE32(sb, 0x60, binary.LittleEndian.Uint32(sb[0x60:])|0x4) }) // s_feature_incompat gains needs_recovery
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if !slices.Contains(c.Assumptions, "journal-not-replayed") {
		t.Errorf("assumptions %q lack journal-not-replayed on an image that needs recovery", c.Assumptions)
	}
}

func TestRecoverableDtimeBeforeCtimeIsFlagged(t *testing.T) {
	f := ext4test.File{Path: "/gone.bin", Data: pattern(2048, 12), Deleted: true}
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		l.PatchInode(img, l.File("/gone.bin").Inode, func(raw []byte) {
			putLE32(raw, 0x0C, 1700000500) // ctime
			putLE32(raw, 0x14, 1700000490) // dtime: ten seconds before it
		})
	})
	c := oneCandidate(t, fsys, "/gone.bin") // still offered
	if !slices.Contains(c.Assumptions, "dtime-before-ctime") {
		t.Errorf("assumptions %q lack dtime-before-ctime", c.Assumptions)
	}
	if !slices.ContainsFunc(c.Warnings, func(w string) bool { return strings.Contains(w, "deletion time") }) {
		t.Errorf("warnings %q do not mention the deletion time", c.Warnings)
	}
	// The ordinary case carries neither.
	ok, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) { l.Unlink(img, "/gone.bin", ext4test.DebugfsRm) })
	prepare(t, ok)
	if c := oneCandidate(t, ok, "/gone.bin"); slices.Contains(c.Assumptions, "dtime-before-ctime") || len(c.Warnings) != 0 {
		t.Errorf("a consistent inode is flagged: %+v", c)
	}
}

// Only a regular file or a symlink has a data map: a deleted directory's blocks are directory records, and its
// children are not enumerated (3C-R19), so the reader reports it and claims nothing.
func TestRecoverableDeletedDirectoryIsUnsupported(t *testing.T) {
	files := []ext4test.File{{Path: "/keep", Data: []byte("k")}, {Path: "/gonedir", Dir: true, Deleted: true}}
	fsys, _, _ := recImageWith(t, csumExt, files, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gonedir", ext4test.DebugfsRm)
	})
	_, err := fsys.Recoverable(delEntry(t, fsys, "/gonedir"))
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// Errors from the earlier stages pass through typed.
func TestRecoverableTypedAnswersPassThrough(t *testing.T) {
	files := []ext4test.File{{Path: "/gone.bin", Data: pattern(2048, 14), Deleted: true}}
	fsys, _, _ := recImageWith(t, csumExt, files, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		l.SetInodeFree(img, l.File("/gone.bin").Inode, false) // allocated again
	})
	if _, err := fsys.Recoverable(delEntry(t, fsys, "/gone.bin")); !errors.Is(err, filesys.ErrReused) {
		t.Errorf("err = %v, want ErrReused", err)
	}
}

// A map that ends before i_size (a sparse tail, or a map the kernel started to clear) stops there as a hole; the bytes
// past it are not invented, and the prefix is what the examiner caps.
func TestRecoverableMapEndingBeforeSizeStopsAsHole(t *testing.T) {
	data := pattern(3*1024, 15)
	for _, o := range []ext4test.Options{csumExt, {}} {
		f := ext4test.File{Path: "/gone.bin", Data: data, Size: 8 * 1024, Deleted: true}
		fsys, img, _ := recImageWith(t, o, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
			l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
		})
		c := oneCandidate(t, fsys, "/gone.bin")
		if got := stopOf(c); got != "hole@3" {
			t.Errorf("extents=%v: stop = %q, want hole@3", o.Extents, got)
		}
		if c.Size != 8*1024 || !bytes.Equal(concatRuns(img, c.Runs), data) {
			t.Errorf("extents=%v: size %d, covered %d", o.Extents, c.Size, coveredBytes(c))
		}
	}
}

// A hole or an uninitialized extent at block 0 leaves no run, but the reason is reported, not silence.
func TestRecoverableStopBeforeFirstRunIsReported(t *testing.T) {
	data := pattern(2*1024, 16)
	f := ext4test.File{Path: "/gone.bin", Deleted: true, Pieces: []ext4test.Piece{{Block: 2, Data: data}}}
	fsys, _, _ := recImageWith(t, csumExt, []ext4test.File{f}, func(img []byte, l *ext4test.Layout) {
		l.Unlink(img, "/gone.bin", ext4test.DebugfsRm)
	})
	c := oneCandidate(t, fsys, "/gone.bin")
	if len(c.Runs) != 0 || stopOf(c) != "hole@0" || c.Size != 4*1024 {
		t.Fatalf("%+v", c)
	}
}
