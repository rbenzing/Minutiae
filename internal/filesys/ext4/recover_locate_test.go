package ext4_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
)

// locReader counts the ReadAt calls made on the image.
type locReader struct {
	r     io.ReaderAt
	calls atomic.Int64
}

func (c *locReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return c.r.ReadAt(p, off)
}

func openLoc(t *testing.T, img []byte) (*ext4.FS, *locReader) {
	t.Helper()
	rr := &locReader{r: bytes.NewReader(img)}
	f, err := ext4.Open(rr, int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f, rr
}

func direntIDAt(blk, off int) string { return fmt.Sprintf("dirent:%d:%d", blk, off) }

// locFiles is a tree with deleted records in the root, a subdirectory, an htree directory and an inline directory.
func locFiles() []ext4test.File {
	files := []ext4test.File{
		{Path: "/keep", Data: []byte("keep")},
		{Path: "/gone", Data: []byte("gone"), Deleted: true},
		{Path: "/sub", Dir: true},
		{Path: "/sub/s1", Data: []byte("s1")},
		{Path: "/sub/sgone", Data: []byte("sg"), Deleted: true},
		{Path: "/big", Dir: true, HTree: true},
		{Path: "/idir", Dir: true, Inline: true},
		{Path: "/idir/n1", Data: []byte("n1")},
		{Path: "/idir/n2", Data: []byte("n2"), Deleted: true},
		{Path: "/idir/n3", Data: []byte("n3")},
	}
	for i := range 200 {
		files = append(files, ext4test.File{Path: fmt.Sprintf("/big/file-%03d", i), Data: []byte{byte(i)}})
	}
	return append(files, ext4test.File{Path: "/big/zgone", Data: []byte("zz"), Deleted: true})
}

var locOpts = ext4test.Options{Extents: true, InlineData: true, MetadataCsum: true, Groups: 4}

func TestResolveDeletedFindsListedEntries(t *testing.T) {
	img := ext4test.Build(locOpts, locFiles())
	f, _ := openLoc(t, img)
	total := 0
	for _, dir := range []string{"/", "/sub", "/big", "/idir"} {
		d, err := f.Lookup(dir)
		if err != nil {
			t.Fatal(err)
		}
		dn, err := strconv.Atoi(strings.TrimPrefix(d.ID, "inode:"))
		if err != nil {
			t.Fatalf("directory ID %q", d.ID)
		}
		n := 0
		for _, e := range readDir(t, f, dir) {
			if !e.Deleted {
				continue
			}
			n++
			gotDir, gotRec, lblk, err := f.ResolveDeleted(e.ID)
			if err != nil {
				t.Errorf("%s %s (%s): %v", dir, e.Name, e.ID, err)
				continue
			}
			want, _ := attr(e, "inode")
			if gotDir != uint32(dn) || strconv.Itoa(int(gotRec)) != want {
				t.Errorf("%s %s: dir %d rec %d, want dir %d rec %s", dir, e.Name, gotDir, gotRec, dn, want)
			}
			if (dir == "/idir") != (lblk == -1) || lblk < -1 {
				t.Errorf("%s %s: lblk %d", dir, e.Name, lblk)
			}
		}
		if n == 0 {
			t.Errorf("%s: no deleted entry listed, the test would pass vacuously", dir)
		}
		total += n
	}
	if total < 4 {
		t.Errorf("only %d deleted entries checked", total)
	}
}

func TestResolveDeletedRejectsNonCanonicalIDsWithoutReads(t *testing.T) {
	img := ext4test.Build(locOpts, locFiles())
	f, rr := openLoc(t, img)
	info := f.Info()
	blocks := info.Size / int64(info.BlockSize)
	ids := []string{
		"", "dirent:", "dirent:01:4", "dirent:+1:4", "dirent:1:-4", "dirent:1", "dirent:1:4:8",
		"dirent:5:3", fmt.Sprintf("dirent:5:%d", info.BlockSize), fmt.Sprintf("dirent:%d:0", blocks),
		"dirent:18446744073709551616:0", "dirent:inline:", "dirent:inline:0:4", "dirent:inline:12:02",
		" dirent:5:4", "dirent:5:4 ", "dirent:5:04", "dirent:0x5:4", "dirent:inline:4294967296:4",
		"dirent:inline:12:3", "dirent:inline:12:4156", "dirent:inline:12", "dirent:inline:12:4:8",
		"dirent:inline:+12:4", "dirent:inline:-1:4", fmt.Sprintf("dirent:inline:%d:4", 1<<31), "inode:7", "nid:7",
	}
	before := rr.calls.Load()
	for _, id := range ids {
		_, _, _, err := f.ResolveDeleted(id)
		if !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("ResolveDeleted(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if n := rr.calls.Load() - before; n != 0 {
		t.Errorf("non-canonical IDs made %d reads, want 0", n)
	}
}

func TestResolveDeletedLiveRecordIsNotDeleted(t *testing.T) {
	img, l := ext4test.BuildLayout(locOpts, locFiles())
	f, _ := openLoc(t, img)
	keep := l.File("/keep")
	_, _, _, err := f.ResolveDeleted(direntIDAt(keep.DirentBlock, keep.DirentOffset))
	if !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live record: %v, want ErrNotDeleted", err)
	}
	// A live record of an inline directory.
	n1 := l.File("/idir/n1")
	if _, _, _, err := f.ResolveDeleted(fmt.Sprintf("dirent:inline:%d:%d", l.File("/idir").Inode, n1.DirentOffset)); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live inline record: %v, want ErrNotDeleted", err)
	}
}

func TestResolveDeletedNoRecordAtOffsetIsNotFound(t *testing.T) {
	img, l := ext4test.BuildLayout(locOpts, locFiles())
	f, _ := openLoc(t, img)
	keep := l.File("/keep")
	// Four bytes into a live record: nothing starts there.
	_, _, _, err := f.ResolveDeleted(direntIDAt(keep.DirentBlock, keep.DirentOffset+4))
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("mid-record offset: %v, want ErrNotFound", err)
	}
	// The last 12 bytes of a block with a checksum tail are not a record.
	bs := l.BlockSize
	if _, _, _, err := f.ResolveDeleted(direntIDAt(keep.DirentBlock, bs-12)); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("checksum tail offset: %v, want ErrNotFound", err)
	}
}

// A file data block that parses as a directory block is not a directory block: no live directory maps it.
func TestResolveDeletedNonDirectoryBlockIsNotFound(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, []ext4test.File{
		{Path: "/keep", Data: []byte("keep")},
		{Path: "/victim", Data: bytes.Repeat([]byte{0xAA}, 1024)},
	})
	blk := l.File("/victim").Data[0]
	b := img[blk*1024 : (blk+1)*1024]
	clear(b)
	le := binary.LittleEndian
	// Record 1: inode 5, rec_len 1024, name "a"; the slack holds a deleted record at offset 12: inode 7, name "bb".
	le.PutUint32(b[0:], 5)
	le.PutUint16(b[4:], 1024)
	b[6], b[7] = 1, 1
	b[8] = 'a'
	le.PutUint32(b[12:], 7)
	le.PutUint16(b[16:], 12)
	b[18], b[19] = 2, 1
	copy(b[20:], "bb")
	f, _ := openLoc(t, img)
	_, _, _, err := f.ResolveDeleted(direntIDAt(blk, 12))
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("ID into a file block: %v, want ErrNotFound", err)
	}
}

func TestResolveDeletedCrossLinkedBlockIsCorrupt(t *testing.T) {
	files := []ext4test.File{
		{Path: "/a", Dir: true},
		{Path: "/a/a1", Data: []byte("a1")},
		{Path: "/b", Dir: true},
		{Path: "/b/b1", Data: []byte("b1")},
		{Path: "/b/bgone", Data: []byte("bg"), Deleted: true},
	}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	l.ShareDirBlock(img, "/a", "/b")
	f, _ := openLoc(t, img)
	g := l.File("/b/bgone")
	if g.DirentBlock != l.File("/b").Data[0] {
		t.Fatalf("test setup: the deleted record is in block %d, not b's first block %d", g.DirentBlock, l.File("/b").Data[0])
	}
	dn, rn, lb, err := f.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset))
	if !errors.Is(err, filesys.ErrCorrupt) {
		t.Fatalf("cross-linked block: (%d,%d,%d) %v, want ErrCorrupt", dn, rn, lb, err)
	}
	if errors.Is(err, filesys.ErrNotFound) || errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("cross-linked block: %v must be corruption alone", err)
	}
}

func capFiles() []ext4test.File {
	var files []ext4test.File
	for i := 1; i <= 5; i++ {
		d := fmt.Sprintf("/d%d", i)
		files = append(files,
			ext4test.File{Path: d, Dir: true},
			ext4test.File{Path: d + "/live", Data: []byte("l")},
			ext4test.File{Path: d + "/gone", Data: []byte("g"), Deleted: true})
	}
	return files
}

func TestResolveDeletedIndexCapIsLimitNotNotFound(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, capFiles())
	f, _ := openLoc(t, img)
	f.SetLocateCaps(2, 0) // the root and the first subdirectory
	in, out := l.File("/d1/gone"), l.File("/d5/gone")
	if _, _, _, err := f.ResolveDeleted(direntIDAt(in.DirentBlock, in.DirentOffset)); err != nil {
		t.Errorf("entry inside the indexed part: %v", err)
	}
	_, _, _, err := f.ResolveDeleted(direntIDAt(out.DirentBlock, out.DirentOffset))
	if !errors.Is(err, filesys.ErrLimit) || !errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("entry past the directory cap: %v, want ErrLimit and ErrIncomplete", err)
	}
	if errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("entry past the cap must never be not found: %v", err)
	}
}

func TestResolveDeletedRunCapIsLimitNotNotFound(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, capFiles())
	f, _ := openLoc(t, img)
	f.SetLocateCaps(0, 2) // the root's run and the first subdirectory's run
	out := l.File("/d5/gone")
	_, _, _, err := f.ResolveDeleted(direntIDAt(out.DirentBlock, out.DirentOffset))
	if !errors.Is(err, filesys.ErrLimit) || !errors.Is(err, filesys.ErrIncomplete) {
		t.Errorf("entry past the run cap: %v, want ErrLimit and ErrIncomplete", err)
	}
}

func TestResolveDeletedCutDirectoryMissIsIncomplete(t *testing.T) {
	files := []ext4test.File{
		{Path: "/ok", Dir: true},
		{Path: "/ok/live", Data: []byte("l")},
		{Path: "/ok/gone", Data: []byte("g"), Deleted: true},
		{Path: "/bad", Dir: true},
		{Path: "/bad/live", Data: []byte("l")},
		{Path: "/other", Data: bytes.Repeat([]byte{1}, 3000)},
	}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	l.PatchInode(img, l.File("/bad").Inode, func(raw []byte) { raw[0x28], raw[0x29] = 0, 0 }) // extent header magic
	f, _ := openLoc(t, img)
	ok := l.File("/ok/gone")
	if _, _, _, err := f.ResolveDeleted(direntIDAt(ok.DirentBlock, ok.DirentOffset)); err != nil {
		t.Errorf("a record in an undamaged directory: %v", err)
	}
	// A canonical ID of a block no indexed directory maps: the damaged directory might hold it.
	other := l.File("/other").Data[0]
	_, _, _, err := f.ResolveDeleted(direntIDAt(other, 0))
	if !errors.Is(err, filesys.ErrIncomplete) {
		t.Fatalf("miss with a cut directory: %v, want ErrIncomplete", err)
	}
	if errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("miss with a cut directory must never be not found: %v", err)
	}
}

func TestResolveDeletedCompleteIndexMissIsNotFound(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, capFiles())
	f, _ := openLoc(t, img)
	free := int(f.Info().Size/1024) - 1 // the last block of the filesystem: free in this image
	_, _, _, err := f.ResolveDeleted(direntIDAt(free, 0))
	if !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("block mapped by no directory: %v, want ErrNotFound", err)
	}
	_ = l
}

func TestResolveDeletedFirstRecordHasUnknownInode(t *testing.T) {
	files := []ext4test.File{
		{Path: "/live0", Data: []byte("0")},
		{Path: "/gone1", Data: []byte("1"), Deleted: true, NewBlock: true},
		{Path: "/gone2", Data: []byte("2"), Deleted: true},
	}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	f, _ := openLoc(t, img)
	var unknown, known int
	for _, e := range readDir(t, f, "/") {
		if !e.Deleted {
			continue
		}
		_, rec, _, err := f.ResolveDeleted(e.ID)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if v, _ := attr(e, "inode"); v == "unknown" {
			unknown++
			if rec != 0 {
				t.Errorf("%s: recInode %d, want 0", e.Name, rec)
			}
		} else {
			known++
			if strconv.Itoa(int(rec)) != v {
				t.Errorf("%s: recInode %d, want %s", e.Name, rec, v)
			}
		}
	}
	if unknown != 1 || known != 1 {
		t.Errorf("unknown %d known %d, want 1 and 1", unknown, known)
	}
	_ = l
}

// A kernel that wipes a deleted dirent (inode, name and type, as the measured 6.1 driver does) leaves nothing to
// list; an ID forged at that place is not found, and nothing is invented. A kernel that keeps the inode and name of a
// merged record (5.10) leaves a record that resolves.
func TestResolveDeletedWipedRecordIsNotFound(t *testing.T) {
	files := []ext4test.File{
		{Path: "/live0", Data: []byte("0")},
		{Path: "/gone1", Data: []byte("1"), Deleted: true, NewBlock: true},
		{Path: "/gone2", Data: []byte("2"), Deleted: true},
		{Path: "/live3", Data: []byte("3")},
	}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	g1, g2 := l.File("/gone1"), l.File("/gone2")
	f0, _ := openLoc(t, img)
	for _, g := range []ext4test.FileLoc{g1, g2} {
		if _, _, _, err := f0.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset)); err != nil {
			t.Fatalf("unwiped %s: %v", g.Path, err)
		}
	}
	wipe := func(g ext4test.FileLoc, nameLen int) {
		o := g.DirentBlock*l.BlockSize + g.DirentOffset
		clear(img[o : o+4])
		clear(img[o+6 : o+8+nameLen])
	}
	wipe(g1, len("gone1"))
	wipe(g2, len("gone2"))
	f, _ := openLoc(t, img)
	for _, g := range []ext4test.FileLoc{g1, g2} {
		if dn, rn, _, err := f.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset)); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("wiped %s: (%d,%d) %v, want ErrNotFound", g.Path, dn, rn, err)
		}
	}
	// 5.10 on a first-in-block record: only the inode is zeroed, the name stays.
	img2, l2 := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	g := l2.File("/gone1")
	clear(img2[g.DirentBlock*l2.BlockSize+g.DirentOffset:][:4])
	f2, _ := openLoc(t, img2)
	_, rec, _, err := f2.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset))
	if err != nil || rec != 0 {
		t.Errorf("first-in-block record with its inode zeroed: rec %d, %v; want unknown inode (0) and no error", rec, err)
	}
}

func TestResolveDeletedWorkIsBounded(t *testing.T) {
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, capFiles())
	f, _ := openLoc(t, img)
	g := l.File("/d3/gone")
	id := direntIDAt(g.DirentBlock, g.DirentOffset)
	if _, _, _, err := f.ResolveDeleted(id); err != nil {
		t.Fatal(err)
	}
	first := f.LocateWork()
	for range 49 {
		if _, _, _, err := f.ResolveDeleted(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.LocateWork() - first; got != 49 {
		t.Errorf("49 more resolutions added %d work, want exactly 49 (one block read each, the index built once)", got)
	}
}

func TestResolveDeletedAddsNoFilesystemWarnings(t *testing.T) {
	files := append(capFiles(), ext4test.File{Path: "/bad", Dir: true}, ext4test.File{Path: "/bad/x", Data: []byte("x")})
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	l.PatchInode(img, l.File("/bad").Inode, func(raw []byte) { raw[0x28], raw[0x29] = 0, 0 })
	f, _ := openLoc(t, img)
	before := len(f.Info().Warnings)
	g := l.File("/d2/gone")
	_, _, _, _ = f.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset))
	_, _, _, _ = f.ResolveDeleted(direntIDAt(1, 0))
	if w := f.Info().Warnings; len(w) != before {
		t.Errorf("recovery added warnings: %v", w[before:])
	}
}

// A hostile image whose directories all map the same blocks cannot make the index read without bound.
func TestResolveDeletedIndexWorkIsBoundedOnCrossLinkedDirectories(t *testing.T) {
	var files []ext4test.File
	for i := range 40 {
		files = append(files, ext4test.File{Path: fmt.Sprintf("/d%02d", i), Dir: true}, ext4test.File{Path: fmt.Sprintf("/d%02d/x", i), Data: []byte("x")})
	}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	for i := 1; i < 40; i++ {
		l.ShareDirBlock(img, fmt.Sprintf("/d%02d", i), "/d00")
	}
	f, _ := openLoc(t, img)
	_, _, _, err := f.ResolveDeleted(direntIDAt(1, 0))
	if err == nil {
		t.Fatal("a block mapped by no directory resolved")
	}
	if w := f.LocateWork(); w > 100000 {
		t.Errorf("index work %d on a tiny image", w)
	}
}

// A block whose scan is cut before the offset (an invalid rec_len) cannot say the record is absent.
func TestResolveDeletedCutBlockMissIsIncomplete(t *testing.T) {
	files := []ext4test.File{
		{Path: "/keep", Data: []byte("k")},
		{Path: "/gone", Data: []byte("g"), Deleted: true},
	}
	img, l := ext4test.BuildLayout(ext4test.Options{Extents: true}, files)
	k, g := l.File("/keep"), l.File("/gone")
	binary.LittleEndian.PutUint16(img[k.DirentBlock*l.BlockSize+k.DirentOffset+4:], 2) // rec_len below the minimum
	f, _ := openLoc(t, img)
	_, _, _, err := f.ResolveDeleted(direntIDAt(g.DirentBlock, g.DirentOffset))
	if !errors.Is(err, filesys.ErrIncomplete) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("record behind a cut: %v, want ErrIncomplete and not ErrNotFound", err)
	}
}
