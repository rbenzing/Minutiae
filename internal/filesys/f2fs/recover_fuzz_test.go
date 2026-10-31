package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// recoverFuzzMaxEntries bounds the deleted entries examined per input.
const recoverFuzzMaxEntries = 50

// recFuzzOpts is the volume FuzzF2FSRecoverable edits: six segments, the last
// two node segments, summaries, inode checksums and extra attributes.
var recFuzzOpts = f2fstest.Options{Segments: 6, NodeSegs: 2, SSA: true, InodeChksum: true, ExtraAttr: true}

// recFuzzBase is the image the fuzzed edits are applied to. As in
// FuzzF2FSLookup the engine cannot mutate a whole F2FS image at speed (its
// metadata area is 16 MiB), so the input is a list of byte edits of the
// nonzero blocks of this image.
type recFuzzBase struct {
	img    []byte
	tree   *f2fstest.Tree
	blocks []int // the 4 KiB blocks of img with a nonzero byte, the only ones edits address
	layout f2fstest.Layout
}

var (
	recFuzzOnce sync.Once
	recFuzzVal  recFuzzBase
)

func fuzzRecoverBase() *recFuzzBase {
	recFuzzOnce.Do(func() {
		o := recFuzzOpts
		files := []f2fstest.File{
			{Path: "/live.txt", Data: stamp(2*bs+9, "l")},
			{Path: "/d/small.bin", Data: stamp(3000, "s"), Deleted: true, Unlinked: true},
			{Path: "/d/big.bin", Data: stamp(1000*bs, "b"), Deleted: true, Unlinked: true},
			{Path: "/d/sparse.bin", Data: stamp(5*bs, "p"), Deleted: true, Unlinked: true},
			{Path: "/d/twice.bin", Data: stamp(2*bs, "t"), Deleted: true, Unlinked: true},
			{Path: "/inl", Dir: true, Inline: true},
			{Path: "/inl/x.bin", Data: []byte("inline"), Inline: true, Deleted: true},
		}
		img, tr := f2fstest.BuildTree(o, files)
		l := f2fstest.Geometry(o)
		// sparse.bin: a hole at logical block 2 (i_addr starts after the 36-byte extra area)
		sp := tr.Addr[tr.NID["/d/sparse.bin"]]
		le.PutUint32(blk(img, sp)[360+36+4*2:], 0)
		f2fstest.ResealInode(o, img, sp)
		// twice.bin: a second stale copy of the same inode, one version newer, in the last node segment
		tw := tr.NID["/d/twice.bin"]
		a := l.Main + 5*f2fstest.BlocksPerSeg + 300
		copy(blk(img, a), blk(img, tr.Addr[tw]))
		f2fstest.SetFooter(img, a, f2fstest.Footer{NID: tw, Ino: tw, CPVer: 1})
		f2fstest.SetSSAEntry(img, o, a, tw)
		f2fstest.ResealInode(o, img, a)
		b := recFuzzBase{img: img, tree: tr, layout: l}
		for n := 0; n*bs < len(img); n++ {
			if bytes.ContainsFunc(img[n*bs:min(len(img), (n+1)*bs)], func(r rune) bool { return r != 0 }) {
				b.blocks = append(b.blocks, n)
			}
		}
		recFuzzVal = b
	})
	return &recFuzzVal
}

// edit encodes one byte edit of the base image (block index into the nonzero blocks, offset in the block, value).
func (b *recFuzzBase) edit(blockAddr, off int, val byte) []byte {
	idx := slices.Index(b.blocks, blockAddr)
	if idx < 0 {
		panic(fmt.Sprintf("block %d is not in the base image", blockAddr))
	}
	return []byte{byte(idx), byte(idx >> 8), byte(off), byte(off >> 8), val}
}

// apply returns a copy of the base image with the edits of in applied (a trailing partial edit is ignored).
func (b *recFuzzBase) apply(in []byte) []byte {
	img := bytes.Clone(b.img)
	for ; len(in) >= 5; in = in[5:] {
		n := b.blocks[(int(in[0])|int(in[1])<<8)%len(b.blocks)]
		img[n*bs+(int(in[2])|int(in[3])<<8)%bs] = in[4]
	}
	return img
}

// FuzzF2FSRecoverable applies byte edits to a builder image with unlinked
// files (small, 1000 blocks, sparse, two stale copies of one inode), a live
// file and an inline directory with a deleted entry, then asks Recoverable for
// every deleted entry the walk lists (at most 50). It never panics, and it
// returns either a typed error or candidates that pass CheckCandidate, cover
// no more than their size, and are equal on a second call.
//
// Gate run: go test ./internal/filesys/f2fs -run '^$' -fuzz '^FuzzF2FSRecoverable$' -fuzztime 30s -fuzzminimizetime 0 -parallel 2
func FuzzF2FSRecoverable(f *testing.F) {
	base := fuzzRecoverBase()
	tr, l := base.tree, base.layout
	small := tr.Addr[tr.NID["/d/small.bin"]]
	f.Add([]byte{})
	f.Add(base.edit(int(small), 4072, 0x09))                       // footer nid
	f.Add(base.edit(int(small), 360+36+4, 0x55))                   // an i_addr slot
	f.Add(base.edit(int(l.SSA+4), 0, 0x33))                        // an SSA entry of a node segment
	f.Add(base.edit(int(l.SIT), 4*74, 0x01))                       // the vblocks word of a node segment
	f.Add(base.edit(int(tr.Addr[tr.NID["/d/big.bin"]]), 16, 0xff)) // the size of the 1000-block file
	f.Fuzz(func(t *testing.T, in []byte) {
		img := base.apply(in)
		fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			return
		}
		size := fsys.Info().Size
		var deleted []filesys.Entry
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err == nil && e.Deleted {
				if deleted = append(deleted, e); len(deleted) >= recoverFuzzMaxEntries {
					return errFuzzBudget
				}
			}
			return nil
		})
		for _, e := range deleted {
			cs, err := fsys.Recoverable(e)
			if err != nil {
				if cs != nil {
					t.Fatalf("%s: candidates and an error: %v", e.ID, err)
				}
				if !recoverTypedError(err) {
					t.Fatalf("%s: untyped error: %v", e.ID, err)
				}
			}
			for _, c := range cs {
				covered, cerr := filesys.CheckCandidate(c, size)
				if cerr != nil {
					t.Fatalf("%s: CheckCandidate: %v", e.ID, cerr)
				}
				if covered > c.Size {
					t.Fatalf("%s: covers %d bytes of a %d-byte file", e.ID, covered, c.Size)
				}
			}
			cs2, err2 := fsys.Recoverable(e)
			if (err == nil) != (err2 == nil) || err != nil && err.Error() != err2.Error() || !reflect.DeepEqual(cs, cs2) {
				t.Fatalf("%s: the second call differs: %v %v / %v %v", e.ID, cs, err, cs2, err2)
			}
		}
	})
}

// recoverTypedError reports whether err is one of the answers Recoverable may give on hostile bytes.
func recoverTypedError(err error) bool {
	for _, k := range []error{
		filesys.ErrNotFound, filesys.ErrNotDeleted, filesys.ErrCorrupt, filesys.ErrUnsupported,
		filesys.ErrUndecided, filesys.ErrLimit, filesys.ErrReused,
	} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}

// The unedited base recovers each of its unlinked files, so the fuzz mutates
// inputs that reach the whole map walk (and the inline entry is a live inode).
func TestFuzzRecoverBaseRecoversItsFiles(t *testing.T) {
	img := fuzzRecoverBase().img
	fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"small.bin": 3000, "big.bin": 1000 * bs, "sparse.bin": 5 * bs, "twice.bin": 2 * bs} {
		cs, err := fsys.Recoverable(filesys.Entry{ID: delID(t, fsys, "/d", name)})
		if err != nil || len(cs) != 1 || cs[0].Size != want || len(cs[0].Runs) == 0 {
			t.Errorf("%s: %v, %+v", name, err, cs)
		}
	}
	if _, err := fsys.Recoverable(filesys.Entry{ID: delID(t, fsys, "/inl", "x.bin")}); err == nil {
		t.Error("the inline directory's deleted entry names a live inode and must not be recovered")
	}
}
