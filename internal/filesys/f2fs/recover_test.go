package f2fs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Recoverable (3D Task 6) on builder images: the dentry of a deleted file is
// re-resolved from disk, the inode is proven by structure among the stale
// copies of the footer index, and the map is walked through the stale nodes.
// Images use two node segments with summaries (the last two of 16 segments);
// the tests patch image bytes with the builder's helpers, never a reader seam.

// Byte offsets inside an inode block without an extra attribute header.
const (
	oMode    = 0
	oInline  = 3
	oUID     = 4
	oSize    = 16
	oFlags   = 80
	oPino    = 84
	oNameLen = 88
	oName    = 92
	oAddr    = 360
	oNID     = 4052
)

// stamp is the Go twin of lib.sh stamp: every 4 KiB block repeats the 32-byte
// record "<seed>:<logical block>", so the content tells which block it is.
func stamp(n int, seed string) []byte {
	out := make([]byte, n)
	for i := range out {
		rec := fmt.Sprintf("%s:%d", seed, i/bs)
		rec += strings.Repeat(" ", 32-len(rec))
		out[i] = rec[i%32]
	}
	return out
}

// readRuns is the concatenation of the image bytes at runs.
func readRuns(img []byte, runs []filesys.Run) []byte {
	var out []byte
	for _, r := range runs {
		out = append(out, img[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

func blk(img []byte, a uint32) []byte { return img[int(a)*bs : (int(a)+1)*bs] }

func recOpts(o f2fstest.Options) f2fstest.Options {
	if o.NodeSegs == 0 {
		o.NodeSegs = 2
	}
	o.SSA = true
	return o
}

// recImage builds the files, optionally patches the image, and opens it.
func recImage(t *testing.T, o f2fstest.Options, files []f2fstest.File, patch ...func(img []byte, tr *f2fstest.Tree)) (*f2fs.FS, []byte, *f2fstest.Tree) {
	t.Helper()
	o = recOpts(o)
	img, tr := f2fstest.BuildTree(o, files)
	for _, p := range patch {
		if p != nil {
			p(img, tr)
		}
	}
	return mustOpen(t, img), img, tr
}

// delID returns the ID ReadDir lists for the deleted name in directory dirPath.
func delID(t *testing.T, fsys *f2fs.FS, dirPath, name string) string {
	t.Helper()
	d, err := fsys.Lookup(dirPath)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", dirPath, err)
	}
	es, err := fsys.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dirPath, err)
	}
	for _, e := range es {
		if e.Deleted && e.Name == name {
			return e.ID
		}
	}
	t.Fatalf("no deleted entry %q in %s: %+v", name, dirPath, es)
	return ""
}

func goneFile(data []byte) []f2fstest.File {
	return []f2fstest.File{{Path: "/d/gone.bin", Data: data, Deleted: true, Unlinked: true}}
}

// patchInode applies mod to the stale inode block of path and re-seals its checksum.
func patchInode(o f2fstest.Options, path string, mod func(b []byte)) func([]byte, *f2fstest.Tree) {
	o = recOpts(o)
	return func(img []byte, tr *f2fstest.Tree) {
		a := tr.Addr[tr.NID[path]]
		mod(blk(img, a))
		f2fstest.ResealInode(o, img, a)
	}
}

func recoverGone(t *testing.T, fsys *f2fs.FS) ([]filesys.Candidate, error) {
	t.Helper()
	return fsys.Recoverable(filesys.Entry{ID: delID(t, fsys, "/d", "gone.bin")})
}

// freeNodeSlot is block j of the last node segment of a 16-segment image,
// where a planted stale copy lies (clear SIT bit, node-typed segment).
func freeNodeSlot(o f2fstest.Options, j uint32) uint32 {
	l := f2fstest.Geometry(recOpts(o))
	return l.Main + 15*f2fstest.BlocksPerSeg + j
}

// plantCopy writes a copy of the block at src to free node slot j of the last
// node segment with footer version ver, its summary naming nid.
func plantCopy(o f2fstest.Options, img []byte, src, j, nid uint32, ver uint64, mod func(b []byte)) uint32 {
	o = recOpts(o)
	a := freeNodeSlot(o, j)
	copy(blk(img, a), blk(img, src))
	if mod != nil {
		mod(blk(img, a))
	}
	f2fstest.SetFooter(img, a, f2fstest.Footer{NID: nid, Ino: nid, CPVer: ver})
	f2fstest.SetSSAEntry(img, o, a, nid)
	f2fstest.ResealInode(o, img, a)
	return a
}

func setAddrs(b []byte, at int, addrs ...uint32) {
	for i, a := range addrs {
		binary.LittleEndian.PutUint32(b[at+4*i:], a)
	}
}

func basisCount(c filesys.Candidate, prefix string) int {
	n := 0
	for _, b := range c.Basis {
		if strings.HasPrefix(b, prefix) {
			n++
		}
	}
	return n
}

func TestRecoverableUnlinkedFile(t *testing.T) {
	data := stamp(5*4096-100, "a")
	fsys, img, tr := recImage(t, f2fstest.Options{InodeChksum: true, ExtraAttr: true}, goneFile(data))
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d candidates", err, len(cs))
	}
	c := cs[0]
	if c.Method != f2fs.MethodNodeScan || c.Size != int64(len(data)) || !bytes.Equal(readRuns(img, c.Runs), data) {
		t.Fatalf("%+v", c)
	}
	nid := tr.NID["/d/gone.bin"]
	for _, want := range []string{
		fmt.Sprintf("node:nid=%d:blk=0x%x", nid, tr.Addr[nid]), "inode-name=match", "inode-parent=match",
		"inode-checksum=ok", "ssa=match", fmt.Sprintf("inode=%d", nid), "cp_ver=0", "checkpoint=2", "node-copies=1", "chain-nodes=0",
	} {
		if !slices.Contains(c.Basis, want) {
			t.Errorf("basis %v lacks %q", c.Basis, want)
		}
	}
	if !slices.Equal(c.Assumptions, []string{"newest-stale-copy-before-checkpoint"}) {
		t.Errorf("assumptions = %v, want exactly the version assumption", c.Assumptions)
	}
	if _, err := filesys.CheckCandidate(c, fsys.Info().Size); err != nil {
		t.Error(err)
	}
	if c.Encrypted {
		t.Error("Encrypted set for a plain file")
	}
}

func TestRecoverableBasisChecksumNotRecorded(t *testing.T) {
	fsys, _, _ := recImage(t, f2fstest.Options{}, goneFile(stamp(bs, "a")))
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d", err, len(cs))
	}
	if !slices.Contains(cs[0].Basis, "inode-checksum=not-recorded") {
		t.Errorf("basis %v", cs[0].Basis)
	}
}

func TestRecoverableDirectNodeFile(t *testing.T) {
	data := stamp(1000*bs, "b")
	fsys, img, _ := recImage(t, f2fstest.Options{}, goneFile(data))
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d", err, len(cs))
	}
	if !bytes.Equal(readRuns(img, cs[0].Runs), data) || cs[0].Size != int64(len(data)) {
		t.Fatalf("content differs; %d runs", len(cs[0].Runs))
	}
	if !slices.Contains(cs[0].Basis, "chain-nodes=1") || len(cs[0].Assumptions) != 1 {
		t.Errorf("basis %v assumptions %v", cs[0].Basis, cs[0].Assumptions)
	}
}

func TestRecoverableInlineDentryDirectory(t *testing.T) {
	data := stamp(2*bs+7, "i")
	fsys, img, _ := recImage(t, f2fstest.Options{}, []f2fstest.File{
		{Path: "/d", Dir: true, Inline: true},
		{Path: "/d/gone.bin", Data: data, Deleted: true, Unlinked: true},
	})
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d", err, len(cs))
	}
	if !bytes.Equal(readRuns(img, cs[0].Runs), data) {
		t.Error("content differs")
	}
}

func TestRecoverableZeroSizeIsEmptyCandidate(t *testing.T) {
	fsys, _, _ := recImage(t, f2fstest.Options{}, goneFile(nil))
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d", err, len(cs))
	}
	if cs[0].Size != 0 || len(cs[0].Runs) != 0 {
		t.Errorf("%+v, want size 0 and no runs", cs[0])
	}
	if _, err := filesys.CheckCandidate(cs[0], fsys.Info().Size); err != nil {
		t.Error(err)
	}
}

func TestRecoverableSparseFileStopsAtHole(t *testing.T) {
	data := stamp(5*bs, "s")
	fsys, img, _ := recImage(t, f2fstest.Options{}, goneFile(data),
		patchInode(f2fstest.Options{}, "/d/gone.bin", func(b []byte) { setAddrs(b, oAddr+4*2, 0) }))
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v, %d", err, len(cs))
	}
	c := cs[0]
	if !slices.Contains(c.Assumptions, "stale-chain-stop=hole@2") {
		t.Errorf("assumptions = %v", c.Assumptions)
	}
	if got := readRuns(img, c.Runs); !bytes.Equal(got, data[:2*bs]) || c.Size != int64(len(data)) {
		t.Errorf("covered %d bytes of size %d, want the first 2 blocks", len(got), c.Size)
	}
}

func TestRecoverableLiveIsNotDeleted(t *testing.T) {
	fsys, _, tr := recImage(t, f2fstest.Options{}, []f2fstest.File{
		{Path: "/d/live.bin", Data: stamp(bs, "l")},
		{Path: "/d/gone.bin", Data: stamp(bs, "g"), Deleted: true, Unlinked: true},
	})
	if _, err := fsys.Recoverable(filesys.Entry{ID: fmt.Sprintf("nid:%d", tr.NID["/d/live.bin"])}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live nid: %v, want ErrNotDeleted", err)
	}
	// The slot of a live record: dentry blocks start with "." and "..", so slot 2 is the first child.
	live := fmt.Sprintf("dentry:%d:0:2", tr.NID["/d"])
	if _, err := fsys.Recoverable(filesys.Entry{ID: live}); !errors.Is(err, filesys.ErrNotDeleted) {
		t.Errorf("live slot %s: %v, want ErrNotDeleted", live, err)
	}
	if _, err := fsys.Recoverable(filesys.Entry{ID: "nid:999"}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("free nid: %v, want ErrNotFound", err)
	}
}

func TestRecoverableUnlistedSlotsAreNotFound(t *testing.T) {
	fsys, _, tr := recImage(t, f2fstest.Options{}, goneFile(stamp(bs, "g")))
	dir := tr.NID["/d"]
	for _, id := range []string{
		fmt.Sprintf("dentry:%d:0:200", dir),                 // an empty slot
		"dentry:77:0:0",                                     // no such directory
		fmt.Sprintf("dentry:%d:9:0", dir),                   // a block past the directory
		fmt.Sprintf("dentry:%d:0:0", tr.NID["/d/gone.bin"]), // the stale inode is not a directory
	} {
		if _, err := fsys.Recoverable(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", id, err)
		}
	}
}

func TestRecoverableInodeInUseIsReused(t *testing.T) {
	fsys, _, tr := recImage(t, f2fstest.Options{}, []f2fstest.File{{Path: "/d/gone.bin", Data: stamp(bs, "g"), Deleted: true}})
	_, err := recoverGone(t, fsys)
	nid := tr.NID["/d/gone.bin"]
	want := fmt.Sprintf("inode %d is live (NAT %d)", nid, tr.Addr[nid])
	if !errors.Is(err, filesys.ErrReused) || errors.Is(err, filesys.ErrNotFound) || !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("err = %v, want ErrReused ending %q", err, want)
	}
}

func TestRecoverableStaleSlotOfRenamedLiveFileIsReused(t *testing.T) {
	for _, tc := range []struct{ name, live string }{
		{"renamed in the same directory", "/d/new.bin"},
		{"hard link elsewhere", "/e/other.bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, _, tr := recImage(t, f2fstest.Options{},
				[]f2fstest.File{
					{Path: tc.live, Data: stamp(bs, "n")},
					{Path: "/d/old.bin", Deleted: true, NoInode: true},
				},
				func(img []byte, tr *f2fstest.Tree) {
					// Point the deleted slot "old.bin" at the live inode.
					b := blk(img, tr.DirBlocks["/d"][0])
					patched := false
					for slot := 0; slot < 214 && !patched; slot++ {
						isOld := binary.LittleEndian.Uint16(b[30+slot*11+8:]) == uint16(len("old.bin"))
						if isOld && b[slot/8]>>(slot%8)&1 == 0 {
							binary.LittleEndian.PutUint32(b[30+slot*11+4:], tr.NID[tc.live])
							patched = true
						}
					}
					if !patched {
						t.Fatal("deleted slot not found")
					}
				})
			_, err := fsys.Recoverable(filesys.Entry{ID: delID(t, fsys, "/d", "old.bin")})
			nid := tr.NID[tc.live]
			want := fmt.Sprintf("inode %d is live (NAT %d)", nid, tr.Addr[nid])
			if !errors.Is(err, filesys.ErrReused) || !strings.HasSuffix(err.Error(), want) {
				t.Fatalf("err = %v, want ErrReused ending %q", err, want)
			}
		})
	}
}

func TestRecoverableInlineAndCompressedAreUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(b []byte)
	}{
		{"inline data", func(b []byte) { b[oInline] |= 0x02 }},
		{"compressed", func(b []byte) { binary.LittleEndian.PutUint32(b[oFlags:], 0x4) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, _, _ := recImage(t, f2fstest.Options{}, goneFile(stamp(2*bs, "u")), patchInode(f2fstest.Options{}, "/d/gone.bin", tc.mod))
			if _, err := recoverGone(t, fsys); !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("err = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestRecoverableDeletedDirectoryIsUnsupported(t *testing.T) {
	fsys, _, _ := recImage(t, f2fstest.Options{}, []f2fstest.File{{Path: "/d/sub", Dir: true, Deleted: true, Unlinked: true}})
	_, err := fsys.Recoverable(filesys.Entry{ID: delID(t, fsys, "/d", "sub")})
	if !errors.Is(err, filesys.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestRecoverableEncryptedIsFlagged(t *testing.T) {
	files := []f2fstest.File{{Path: "/d/gone.bin", Data: stamp(2*bs, "e"), Encrypted: true, Deleted: true, Unlinked: true}}
	fsys, _, _ := recImage(t, f2fstest.Options{Encrypt: true}, files)
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 || !cs[0].Encrypted {
		t.Fatalf("%v %+v", err, cs)
	}
}

func TestRecoverableNATPrevCopyCorroborates(t *testing.T) {
	o := f2fstest.Options{}
	fsys, _, _ := recImage(t, o, goneFile(stamp(bs, "p")), func(img []byte, tr *f2fstest.Tree) {
		l := f2fstest.Geometry(recOpts(o))
		nid := tr.NID["/d/gone.bin"]
		i := nid / 455
		// The version bitmap selects the first copy of every block; the other copy lies 512 blocks on.
		prev := blk(img, l.NAT+(i/512)*1024+i%512+512)
		off := int(nid%455) * 9
		binary.LittleEndian.PutUint32(prev[off+1:], nid)
		binary.LittleEndian.PutUint32(prev[off+5:], tr.Addr[nid])
	})
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %d", err, len(cs))
	}
	if !slices.Contains(cs[0].Basis, "nat-prev-copy=match") {
		t.Errorf("basis %v lacks nat-prev-copy=match", cs[0].Basis)
	}
	// Without the old NAT entry the line is absent.
	plain, _, _ := recImage(t, o, goneFile(stamp(bs, "p")))
	cs, err = recoverGone(t, plain)
	if err != nil || len(cs) != 1 {
		t.Fatal(err)
	}
	if basisCount(cs[0], "nat-prev-copy") != 0 {
		t.Errorf("basis %v has nat-prev-copy without a matching old NAT entry", cs[0].Basis)
	}
}

type countReader struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(1)
	return c.r.ReadAt(p, off)
}

func TestRecoverableForgedIDsReadNothing(t *testing.T) {
	img, _ := f2fstest.BuildTree(recOpts(f2fstest.Options{}), goneFile(stamp(bs, "g")))
	cr := &countReader{r: bytes.NewReader(img)}
	fsys, err := f2fs.Open(cr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	before := cr.n.Load()
	for _, id := range []string{
		"dentry:03:0:1", "dentry:3:0", "dentry:3:0:1:2", "dentry:4294967296:0:0", "dentry:3:0:214",
		"dentry:1:0:0", "dentry:2:0:0", "dentry:0:0:0", "dentry:4294967295:0:0", "dentry:1000000000:0:0",
		"dentry:3:-1:0", "dentry:3:00:0", "dentry:3:0:+1", "nid:", "nid:0", "nid:1", "nid:2", "nid:007", "nid:4294967296", "",
	} {
		if _, err := fsys.Recoverable(filesys.Entry{ID: id}); !errors.Is(err, filesys.ErrNotFound) {
			t.Errorf("%q: %v, want ErrNotFound", id, err)
		}
	}
	if got := cr.n.Load(); got != before {
		t.Errorf("forged IDs made %d reads", got-before)
	}
}

// --- chooseInode: which stale copy is the deleted file's inode ---

func TestChooseInodePicksHighestVersionBelowCheckpoint(t *testing.T) {
	o := f2fstest.Options{}
	files := []f2fstest.File{
		{Path: "/d/other.bin", Data: stamp(3*bs, "o")},
		{Path: "/d/gone.bin", Data: stamp(3*bs-5, "g"), Deleted: true, Unlinked: true},
	}
	fsys, img, _ := recImage(t, o, files, func(img []byte, tr *f2fstest.Tree) {
		nid := tr.NID["/d/gone.bin"]
		plantCopy(o, img, tr.Addr[nid], 10, nid, 1, func(b []byte) { setAddrs(b, oAddr, tr.Data["/d/other.bin"]...) })
	})
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %d", err, len(cs))
	}
	if got, want := readRuns(img, cs[0].Runs), stamp(3*bs, "o")[:3*bs-5]; !bytes.Equal(got, want) {
		t.Errorf("version 1 did not win: content is %.20q", got)
	}
	if !slices.Contains(cs[0].Basis, "cp_ver=1") || !slices.Contains(cs[0].Basis, "node-copies=2") {
		t.Errorf("basis %v", cs[0].Basis)
	}
}

func TestChooseInodeNeverUsesPostCheckpointCopy(t *testing.T) {
	files := []f2fstest.File{{Path: "/d/gone.bin", Data: stamp(bs, "g"), Deleted: true, Unlinked: true, CPVer: 2}}
	fsys, _, _ := recImage(t, f2fstest.Options{}, files)
	_, err := recoverGone(t, fsys)
	if !errors.Is(err, filesys.ErrNotFound) || !strings.Contains(err.Error(), "post-checkpoint") {
		t.Errorf("err = %v, want ErrNotFound naming the post-checkpoint copy", err)
	}
	// A post-checkpoint copy next to an older one: the older one is used.
	o := f2fstest.Options{}
	data := stamp(bs, "g")
	fsys2, img, _ := recImage(t, o, goneFile(data), func(img []byte, tr *f2fstest.Tree) {
		nid := tr.NID["/d/gone.bin"]
		plantCopy(o, img, tr.Addr[nid], 10, nid, 2, func(b []byte) { setAddrs(b, oAddr, 0) })
	})
	cs, err := recoverGone(t, fsys2)
	if err != nil || len(cs) != 1 || !bytes.Equal(readRuns(img, cs[0].Runs), data) {
		t.Fatalf("%v %v", err, cs)
	}
}

func TestChooseInodeRejectsOtherNameOrParent(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(b []byte)
	}{
		{"other name", func(b []byte) { b[oName] ^= 0x20 }},
		{"shorter name", func(b []byte) { binary.LittleEndian.PutUint32(b[oNameLen:], 3) }},
		{"other parent", func(b []byte) { binary.LittleEndian.PutUint32(b[oPino:], 9999) }},
		{"other type", func(b []byte) { binary.LittleEndian.PutUint16(b[oMode:], 0o120777) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, _, _ := recImage(t, f2fstest.Options{}, goneFile(stamp(bs, "g")), patchInode(f2fstest.Options{}, "/d/gone.bin", tc.mod))
			if _, err := recoverGone(t, fsys); !errors.Is(err, filesys.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestChooseInodeTieWithDifferentMapsIsUndecided(t *testing.T) {
	o := f2fstest.Options{}
	files := []f2fstest.File{
		{Path: "/d/other.bin", Data: stamp(bs, "o")},
		{Path: "/d/gone.bin", Data: stamp(bs, "g"), Deleted: true, Unlinked: true},
	}
	var a1, a2 uint32
	fsys, _, _ := recImage(t, o, files, func(img []byte, tr *f2fstest.Tree) {
		nid := tr.NID["/d/gone.bin"]
		a1 = plantCopy(o, img, tr.Addr[nid], 10, nid, 1, nil)
		a2 = plantCopy(o, img, tr.Addr[nid], 11, nid, 1, func(b []byte) { setAddrs(b, oAddr, tr.Data["/d/other.bin"]...) })
	})
	_, err := recoverGone(t, fsys)
	if !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, filesys.ErrNotFound) {
		t.Fatalf("err = %v, want ErrUndecided", err)
	}
	for _, a := range []uint32{a1, a2} {
		if !strings.Contains(err.Error(), fmt.Sprintf("0x%x", a)) {
			t.Errorf("%q does not name block 0x%x", err, a)
		}
	}
}

func TestChooseInodeTieWithIdenticalMapsIsOneCandidate(t *testing.T) {
	o := f2fstest.Options{}
	data := stamp(2*bs, "g")
	var a1, a2 uint32
	fsys, img, _ := recImage(t, o, goneFile(data), func(img []byte, tr *f2fstest.Tree) {
		nid := tr.NID["/d/gone.bin"]
		a1 = plantCopy(o, img, tr.Addr[nid], 10, nid, 1, nil)
		a2 = plantCopy(o, img, tr.Addr[nid], 11, nid, 1, nil)
	})
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %d", err, len(cs))
	}
	if basisCount(cs[0], "node:nid=") != 2 {
		t.Errorf("basis %v, want two node: lines", cs[0].Basis)
	}
	for _, a := range []uint32{a1, a2} {
		found := false
		for _, b := range cs[0].Basis {
			found = found || strings.HasSuffix(b, fmt.Sprintf(":blk=0x%x", a))
		}
		if !found {
			t.Errorf("basis %v does not name block 0x%x", cs[0].Basis, a)
		}
	}
	if !bytes.Equal(readRuns(img, cs[0].Runs), data) {
		t.Error("content differs")
	}
}

func TestChooseInodeEvictedAndNothingQualifiesIsUndecided(t *testing.T) {
	o := f2fstest.Options{Version: 20}
	rename := func(b []byte) { b[oName] ^= 0x20 }
	for _, tc := range []struct {
		copies int
		want   error
	}{{9, filesys.ErrUndecided}, {3, filesys.ErrNotFound}} {
		t.Run(fmt.Sprintf("%d copies", tc.copies), func(t *testing.T) {
			fsys, _, _ := recImage(t, o, goneFile(stamp(bs, "g")),
				patchInode(o, "/d/gone.bin", rename),
				func(img []byte, tr *f2fstest.Tree) {
					nid := tr.NID["/d/gone.bin"]
					for i := 1; i < tc.copies; i++ {
						plantCopy(o, img, tr.Addr[nid], uint32(10+i), nid, uint64(i), nil)
					}
				})
			_, err := recoverGone(t, fsys)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if tc.want == filesys.ErrUndecided && errors.Is(err, filesys.ErrNotFound) {
				t.Error("undecided answer is also not-found")
			}
		})
	}
}

func TestChooseInodeBadChecksumOnlyIsCorrupt(t *testing.T) {
	o := f2fstest.Options{InodeChksum: true, ExtraAttr: true}
	fsys, _, _ := recImage(t, o, goneFile(stamp(bs, "g")), func(img []byte, tr *f2fstest.Tree) {
		blk(img, tr.Addr[tr.NID["/d/gone.bin"]])[oUID] ^= 0xff // not re-sealed
	})
	_, err := recoverGone(t, fsys)
	if !errors.Is(err, filesys.ErrCorrupt) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("err = %v, want a corrupt error, not not-found", err)
	}
}

// A bad newer copy does not hide a good older one: the good copy qualifies and
// wins, and the rejection is a warning.
func TestChooseInodeBadNewerCopyDoesNotHideGoodOlder(t *testing.T) {
	o := f2fstest.Options{InodeChksum: true, ExtraAttr: true}
	data := stamp(bs, "g")
	fsys, img, _ := recImage(t, o, goneFile(data), func(img []byte, tr *f2fstest.Tree) {
		nid := tr.NID["/d/gone.bin"]
		a := plantCopy(o, img, tr.Addr[nid], 10, nid, 1, nil)
		blk(img, a)[oUID] ^= 0xff
	})
	cs, err := recoverGone(t, fsys)
	if err != nil || len(cs) != 1 || !bytes.Equal(readRuns(img, cs[0].Runs), data) {
		t.Fatalf("%v %v", err, cs)
	}
	if len(cs[0].Warnings) == 0 {
		t.Error("a rejected copy left no warning")
	}
}

// A regular file whose content looks like a dentry block is not a directory:
// a slot read from it is never taken for a deleted record (the directory type
// check refuses it first; the parent check of chooseInode would reject it too).
func TestRecoverableSlotOfARegularFileIsNotFound(t *testing.T) {
	var slotID string
	fsys, _, _ := recImage(t, f2fstest.Options{},
		[]f2fstest.File{
			{Path: "/f.bin", Data: stamp(bs, "f")},
			{Path: "/d/gone.bin", Data: stamp(bs, "g"), Deleted: true, Unlinked: true},
		},
		func(img []byte, tr *f2fstest.Tree) {
			copy(blk(img, tr.Data["/f.bin"][0]), blk(img, tr.DirBlocks["/d"][0]))
			slotID = fmt.Sprintf("dentry:%d:0:", tr.NID["/f.bin"])
		})
	realID := delID(t, fsys, "/d", "gone.bin") // dentry:<d>:0:<slot>
	slot := realID[strings.LastIndex(realID, ":")+1:]
	if _, err := fsys.Recoverable(filesys.Entry{ID: slotID + slot}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("%s%s: %v, want ErrNotFound", slotID, slot, err)
	}
	if _, err := fsys.Recoverable(filesys.Entry{ID: realID}); err != nil {
		t.Errorf("the real slot %s: %v", realID, err)
	}
}
