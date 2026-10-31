package f2fs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Hostile stale copies (3D Task 6). Each row patches the stale inode (or a
// node it points at) of an image the builder wrote, and asserts the typed
// error or the exact stop token. No candidate may cover more than its size,
// and a stop token is passed through verbatim from the walker.

func coveredBytes(c filesys.Candidate) int64 {
	var n int64
	for _, r := range c.Runs {
		n += r.Length
	}
	return n
}

func TestRecoverableHostileCopies(t *testing.T) {
	type row struct {
		name  string
		o     f2fstest.Options
		size  int
		patch func(img []byte, tr *f2fstest.Tree)
		err   error  // the typed error wanted ("" stop then)
		text  string // a substring of the error text
		stop  string // the exact stop token wanted, or "" for none
		block int64  // blocks covered by the prefix when stop != ""
	}
	inode := func(o f2fstest.Options, mod func(b []byte)) func([]byte, *f2fstest.Tree) {
		return patchInode(o, "/d/gone.bin", mod)
	}
	nodeOf := func(tr *f2fstest.Tree, k int) uint32 { return tr.Nodes["/d/gone.bin"][k] }
	ind := f2fstest.Options{}
	chk := f2fstest.Options{InodeChksum: true, ExtraAttr: true}
	rows := []row{
		{
			name: "stale size 1<<40", size: bs, err: filesys.ErrCorrupt,
			patch: inode(ind, func(b []byte) { binary.LittleEndian.PutUint64(b[oSize:], 1<<40) }),
		},
		{
			name: "stale size beyond the node tree", size: bs, err: filesys.ErrCorrupt, text: "node tree",
			patch: inode(ind, func(b []byte) { binary.LittleEndian.PutUint64(b[oSize:], 1<<43) }),
		},
		{
			name: "negative-looking size", size: bs, err: filesys.ErrCorrupt,
			patch: inode(ind, func(b []byte) { binary.LittleEndian.PutUint64(b[oSize:], 1<<63+5) }),
		},
		{
			name: "extra isize 3", o: chk, size: bs, err: filesys.ErrCorrupt,
			patch: inode(chk, func(b []byte) { binary.LittleEndian.PutUint16(b[oAddr:], 3) }),
		},
		{
			name: "footer offset not 0", size: bs, err: filesys.ErrNotFound,
			patch: func(img []byte, tr *f2fstest.Tree) {
				f2fstest.SetFooter(img, tr.Addr[tr.NID["/d/gone.bin"]], f2fstest.Footer{NID: tr.NID["/d/gone.bin"], Ino: tr.NID["/d/gone.bin"], Flag: 1 << 3})
			},
		},
		{
			name: "symlink against a regular dentry", size: bs, err: filesys.ErrNotFound,
			patch: inode(ind, func(b []byte) { binary.LittleEndian.PutUint16(b[oMode:], 0o120777) }),
		},
		{name: "checkpoint version with a high half", o: f2fstest.Options{Version: 1<<32 | 5}, size: bs, err: filesys.ErrUnsupported},
		{
			name: "dnode of another owner", size: 1000 * bs, stop: "node-owner@", block: 923,
			patch: func(img []byte, tr *f2fstest.Tree) {
				a := tr.Addr[nodeOf(tr, 1)]
				f2fstest.SetFooter(img, a, f2fstest.Footer{NID: nodeOf(tr, 1), Ino: 4000, Flag: 1 << 3})
			},
		},
		{
			name: "dnode missing", size: 1000 * bs, stop: "node-missing@", block: 923,
			patch: func(img []byte, tr *f2fstest.Tree) {
				f2fstest.SetFooter(img, tr.Addr[nodeOf(tr, 1)], f2fstest.Footer{})
			},
		},
		{
			name: "address in the SIT area", size: 3 * bs, stop: "address-outside-main@0", block: 0,
			patch: inode(ind, func(b []byte) { setAddrs(b, oAddr, f2fstest.Geometry(recOpts(ind)).SIT) }),
		},
		{
			name: "address below the main area after two blocks", size: 5 * bs, stop: "address-outside-main@2", block: 2,
			patch: inode(ind, func(b []byte) { setAddrs(b, oAddr+8, 1) }),
		},
		{
			name: "address beyond the image", size: 5 * bs, stop: "address-outside-main@3", block: 3,
			patch: inode(ind, func(b []byte) { setAddrs(b, oAddr+12, 0x7fffffff) }),
		},
		{
			name: "block repeated", size: 6 * bs, stop: "address-repeated@4", block: 4,
			patch: func(img []byte, tr *f2fstest.Tree) {
				d := tr.Data["/d/gone.bin"]
				patchInode(ind, "/d/gone.bin", func(b []byte) { setAddrs(b, oAddr+16, d[1]) })(img, tr)
			},
		},
		{
			name: "new address is a hole", size: 4 * bs, stop: "hole@1", block: 1,
			patch: inode(ind, func(b []byte) { setAddrs(b, oAddr+4, 0xFFFFFFFF) }),
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			o := r.o
			fsys, _, _ := recImage(t, o, goneFile(stamp(r.size, "h")), r.patch)
			cs, err := recoverGone(t, fsys)
			if r.err != nil {
				if !errors.Is(err, r.err) {
					t.Fatalf("err = %v, want %v (candidates %d)", err, r.err, len(cs))
				}
				if r.text != "" && !strings.Contains(err.Error(), r.text) {
					t.Errorf("err = %q, want it to contain %q", err, r.text)
				}
				if len(cs) != 0 {
					t.Errorf("an error came with %d candidates", len(cs))
				}
				return
			}
			if err != nil || len(cs) != 1 {
				t.Fatalf("err = %v, %d candidates", err, len(cs))
			}
			c := cs[0]
			var stop string
			for _, a := range c.Assumptions {
				if s, ok := strings.CutPrefix(a, "stale-chain-stop="); ok {
					stop = s
				}
			}
			if !strings.HasPrefix(stop, r.stop) || r.stop == "" {
				t.Errorf("stop = %q, want prefix %q (assumptions %v)", stop, r.stop, c.Assumptions)
			}
			if strings.HasSuffix(r.stop, "@") && strings.HasSuffix(stop, "@") {
				t.Errorf("stop %q names no number", stop)
			}
			if got := coveredBytes(c); got != r.block*bs || got > c.Size {
				t.Errorf("covered %d bytes of size %d, want %d blocks", got, c.Size, r.block)
			}
			if _, err := filesys.CheckCandidate(c, fsys.Info().Size); err != nil {
				t.Error(err)
			}
		})
	}
}

// A cycle through an indirect node, and a node tree read through the indirect
// slot, end the map with their own tokens.
func TestRecoverableIndirectStops(t *testing.T) {
	const size = 13 << 20 // needs i_nid[2]
	indirectNID := func(img []byte, tr *f2fstest.Tree) uint32 {
		b := blk(img, tr.Addr[tr.NID["/d/gone.bin"]])
		return binary.LittleEndian.Uint32(b[oNID+8:])
	}
	for _, tc := range []struct {
		name  string
		patch func(img []byte, tr *f2fstest.Tree)
		stop  func(img []byte, tr *f2fstest.Tree) string
	}{
		{"cycle", func(img []byte, tr *f2fstest.Tree) {
			n := indirectNID(img, tr)
			binary.LittleEndian.PutUint32(blk(img, tr.Addr[n]), n)
		}, func(img []byte, tr *f2fstest.Tree) string { return fmt.Sprintf("node-cycle@%d", indirectNID(img, tr)) }},
		{"child of another owner", func(img []byte, tr *f2fstest.Tree) {
			n := indirectNID(img, tr)
			child := binary.LittleEndian.Uint32(blk(img, tr.Addr[n]))
			f2fstest.SetFooter(img, tr.Addr[child], f2fstest.Footer{NID: child, Ino: 4000, Flag: 4 << 3})
		}, func(img []byte, tr *f2fstest.Tree) string {
			n := indirectNID(img, tr)
			return fmt.Sprintf("node-owner@%d", binary.LittleEndian.Uint32(blk(img, tr.Addr[n])))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys, img, tr := recImage(t, f2fstest.Options{}, goneFile(stamp(size, "x")), tc.patch)
			cs, err := recoverGone(t, fsys)
			if err != nil || len(cs) != 1 {
				t.Fatalf("%v %d", err, len(cs))
			}
			want := "stale-chain-stop=" + tc.stop(img, tr)
			if !slices.Contains(cs[0].Assumptions, want) {
				t.Errorf("assumptions %v lack %q", cs[0].Assumptions, want)
			}
			if got := coveredBytes(cs[0]); got >= cs[0].Size || got%bs != 0 {
				t.Errorf("covered %d of %d", got, cs[0].Size)
			}
		})
	}
}

// failing returns a reader that fails reads touching [lo, hi) while armed.
type failRange struct {
	r      io.ReaderAt
	lo, hi int64
	armed  bool
	err    error
}

func (f *failRange) ReadAt(p []byte, off int64) (int, error) {
	if f.armed && off < f.hi && off+int64(len(p)) > f.lo {
		return 0, f.err
	}
	return f.r.ReadAt(p, off)
}

// A node block that cannot be read after the index was built is an error of
// the candidate, never a prefix that looks like a finding.
func TestRecoverableWalkerReadErrorIsErrorNotPrefix(t *testing.T) {
	o := recOpts(f2fstest.Options{})
	img, tr := f2fstest.BuildTree(o, goneFile(stamp(1000*bs, "r")))
	dn := tr.Addr[tr.Nodes["/d/gone.bin"][1]]
	boom := errors.New("device unplugged")
	fr := &failRange{r: bytes.NewReader(img), lo: int64(dn) * bs, hi: int64(dn+1) * bs, err: boom}
	fsys, err := f2fs.Open(fr, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	id := delID(t, fsys, "/d", "gone.bin")
	if err := fsys.BuildNodeIndex(); err != nil {
		t.Fatal(err)
	}
	fr.armed = true
	cs, err := fsys.Recoverable(filesys.Entry{ID: id})
	if !errors.Is(err, boom) || errors.Is(err, filesys.ErrCorrupt) || len(cs) != 0 {
		t.Fatalf("err = %v, %d candidates, want the read error alone", err, len(cs))
	}
}

// A capped index answers with its typed error, never not-found.
func TestRecoverableIndexLimitPassesThrough(t *testing.T) {
	fsys, _, _ := recImage(t, f2fstest.Options{}, goneFile(stamp(1000*bs, "c")))
	id := delID(t, fsys, "/d", "gone.bin")
	fsys.SetNodeIndexCap(1) // the inode and the dnode are two stale nodes
	_, err := fsys.Recoverable(filesys.Entry{ID: id})
	if !errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("err = %v, want ErrLimit", err)
	}
}
