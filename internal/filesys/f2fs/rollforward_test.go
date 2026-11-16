package f2fs_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// Roll-forward node facts and the file summary (3H Task 5), on builder images.

// rfFix is a roll-forward image: a live file with a chain update, an inode that nothing
// replays, a small post-checkpoint file and a 1000-block one. The chain order is the live
// update, the unreplayed inode, a.bin's inode, b.bin's inode, b.bin's direct node.
type rfFix struct {
	img   []byte
	tree  *f2fstest.Tree
	o     f2fstest.Options
	live  uint32 // inode number of /live.bin
	ghost uint32 // inode number of the unreplayed inode
}

func rfStamp(blocks int) []byte {
	b := make([]byte, blocks*bs)
	for i := range b {
		b[i] = byte(i/bs + 1)
	}
	return b
}

func newRFFix(t *testing.T) *rfFix {
	t.Helper()
	o := f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery, ExtraAttr: true, InodeChksum: true}
	files := []f2fstest.File{
		{Path: "/rf", Dir: true},
		{Path: "/live.bin", Data: rfStamp(2)},
		{Path: "/rf/a.bin", Data: rfStamp(3), PostCheckpoint: true},
		{Path: "/rf/b.bin", Data: rfStamp(1000), PostCheckpoint: true},
	}
	_, probe := f2fstest.BuildTree(o, files)
	fx := &rfFix{o: o, live: probe.NID["/live.bin"], ghost: 90}
	o.Chain = []f2fstest.ChainNode{
		{Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: fx.live, Mode: 0o100644, Links: 1, Size: 9000, PIno: f2fstest.RootIno, Name: "live.bin", Extra: true}), Fsync: true},
		{Block: f2fstest.InodeBlock(o, f2fstest.Inode{NID: fx.ghost, Mode: 0o100644, Links: 1, Size: 5, PIno: f2fstest.RootIno, Name: "ghost", Extra: true}), Fsync: true},
	}
	fx.img, fx.tree = f2fstest.BuildTree(o, files)
	if len(fx.tree.ChainAddrs) != 5 {
		t.Fatalf("builder chain = %v, want 5 nodes", fx.tree.ChainAddrs)
	}
	return fx
}

func (fx *rfFix) open(t *testing.T) *f2fs.FS {
	t.Helper()
	f, err := f2fs.Open(bytes.NewReader(fx.img), int64(len(fx.img)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func rfWalk(t *testing.T, f *f2fs.FS) (filesys.RollForwardChain, []filesys.RollForwardNode) {
	t.Helper()
	var nodes []filesys.RollForwardNode
	c, err := f.RollForward(func(n filesys.RollForwardNode) bool { nodes = append(nodes, n); return true })
	if err != nil {
		t.Fatalf("RollForward: %v", err)
	}
	return c, nodes
}

func TestRollForwardReportsEveryNode(t *testing.T) {
	fx := newRFFix(t)
	f := fx.open(t)
	c, nodes := rfWalk(t, f)
	addrs := fx.tree.ChainAddrs
	if c.Nodes != 5 || len(nodes) != 5 || !c.Complete {
		t.Fatalf("chain = %+v with %d nodes visited, want 5 complete", c, len(nodes))
	}
	aIno, bIno := fx.tree.NID["/rf/a.bin"], fx.tree.NID["/rf/b.bin"]
	bDirect := fx.tree.Nodes["/rf/b.bin"][1]
	type want struct {
		nid, ino, ofs       uint32
		kind                string
		fsync, dentry, live bool
		replay              bool
	}
	wants := []want{
		{fx.live, fx.live, 0, "inode", true, false, true, true},
		{fx.ghost, fx.ghost, 0, "inode", true, false, false, false},
		{aIno, aIno, 0, "inode", true, true, false, true},
		{bIno, bIno, 0, "inode", true, true, false, true},
		{bDirect, bIno, 1, "direct", true, false, false, true},
	}
	for i, n := range nodes {
		w := wants[i]
		next := uint64(addrs[i]) + 1
		if i+1 < len(addrs) {
			next = uint64(addrs[i+1])
		}
		if n.Index != i || n.Block != uint64(addrs[i]) || n.Next != next || n.NID != w.nid || n.Ino != w.ino || n.Ofs != w.ofs ||
			n.Kind != w.kind || n.Fsync != w.fsync || n.Dentry != w.dentry || n.LiveIno != w.live || n.WouldReplay != w.replay ||
			n.CPVer != c.FooterVersion || n.Flag>>3 != w.ofs {
			t.Errorf("node %d = %+v, want %+v at block %d next %d", i, n, w, addrs[i], next)
		}
	}
	in := nodes[2].Inode
	if in == nil || in.Name != "a.bin" || in.Size != 3*bs || in.Pino != fx.tree.NID["/rf"] || in.Checksum != "ok" || in.Err != "" {
		t.Errorf("a.bin inode = %+v", in)
	}
	if nodes[4].Inode != nil || nodes[2].Addrs == nil || nodes[4].NIDs != nil {
		t.Errorf("inode/addrs/nids placement wrong: direct node inode %v, a.bin addrs %v, direct nids %v", nodes[4].Inode, nodes[2].Addrs, nodes[4].NIDs)
	}
	// The data addresses: the inode's slots, then the direct node's.
	ia := nodes[3].Addrs
	data := fx.tree.Data["/rf/b.bin"]
	if !slices.Equal(ia, data[:len(ia)]) || !slices.Equal(nodes[4].Addrs[:len(data)-len(ia)], data[len(ia):]) || len(nodes[4].Addrs) != 1018 {
		t.Errorf("b.bin addresses: inode %d slots, direct %d slots, do not match the builder's %d data blocks", len(ia), len(nodes[4].Addrs), len(data))
	}
	if !slices.Equal(nodes[2].Addrs[:3], fx.tree.Data["/rf/a.bin"]) || len(nodes[3].NIDs) != 5 {
		t.Errorf("a.bin addrs %v, b.bin nids %v", nodes[2].Addrs[:3], nodes[3].NIDs)
	}
}

func TestRollForwardFilesSummary(t *testing.T) {
	fx := newRFFix(t)
	c, _ := rfWalk(t, fx.open(t))
	a := fx.tree.ChainAddrs
	aIno, bIno := fx.tree.NID["/rf/a.bin"], fx.tree.NID["/rf/b.bin"]
	want := []filesys.RollForwardFile{
		{Ino: fx.live, Pino: f2fstest.RootIno, Kind: "live-update", Name: "live.bin", HasInode: true, InodeBlock: uint64(a[0]), LastFsyncBlock: uint64(a[0]), Size: 9000},
		{Ino: fx.ghost, Pino: f2fstest.RootIno, Kind: "not-replayed", Name: "ghost", HasInode: true, InodeBlock: uint64(a[1]), LastFsyncBlock: uint64(a[1]), Size: 5},
		{Ino: aIno, Pino: fx.tree.NID["/rf"], Kind: "post-checkpoint", ID: fmt.Sprintf("rollforward:%d", aIno), Name: "a.bin", HasInode: true, InodeBlock: uint64(a[2]), LastFsyncBlock: uint64(a[2]), Size: 3 * bs},
		{Ino: bIno, Pino: fx.tree.NID["/rf"], Kind: "post-checkpoint", ID: fmt.Sprintf("rollforward:%d", bIno), Name: "b.bin", HasInode: true, InodeBlock: uint64(a[3]), LastFsyncBlock: uint64(a[4]), Size: 1000 * bs},
	}
	if !slices.Equal(c.Files, want) {
		t.Errorf("Files =\n%+v\nwant\n%+v", c.Files, want)
	}
}

func TestRollForwardVisitStopsEarly(t *testing.T) {
	f := newRFFix(t).open(t)
	calls := 0
	c, err := f.RollForward(func(filesys.RollForwardNode) bool { calls++; return false })
	if err != nil || calls != 1 || c.Nodes != 5 || len(c.Files) != 4 {
		t.Errorf("calls = %d, chain nodes %d, files %d, err %v; want 1 call and the whole summary", calls, c.Nodes, len(c.Files), err)
	}
}

func TestRollForwardFreshSlices(t *testing.T) {
	f := newRFFix(t).open(t)
	_, first := rfWalk(t, f)
	for _, n := range first {
		for i := range n.Addrs {
			n.Addrs[i] = 0xdeadbeef
		}
		for i := range n.NIDs {
			n.NIDs[i] = 0xdeadbeef
		}
	}
	_, second := rfWalk(t, f)
	for i, n := range second {
		for _, v := range slices.Concat(n.Addrs, n.NIDs) {
			if v == 0xdeadbeef {
				t.Fatalf("node %d: a mutation of an earlier call shows in a later one", i)
			}
		}
	}
}

func TestRollForwardBadChecksumIsReported(t *testing.T) {
	fx := newRFFix(t)
	a := fx.tree.ChainAddrs[2]
	fx.img[int(a)*bs+4] ^= 0xff // i_uid of a.bin's inode: covered by the checksum
	c, nodes := rfWalk(t, fx.open(t))
	if c.Nodes != 5 || len(nodes) != 5 {
		t.Fatalf("the walk stopped at the bad checksum: %d nodes", len(nodes))
	}
	for i, n := range nodes {
		want := ""
		switch {
		case i == 2:
			want = "bad"
		case n.Kind == "inode":
			want = "ok"
		}
		got := ""
		if n.Inode != nil {
			got = n.Inode.Checksum
		}
		if got != want {
			t.Errorf("node %d checksum = %q, want %q", i, got, want)
		}
	}
}

func TestRollForwardNoRecordedChecksum(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery}
	img, _ := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/p.bin", Data: rfStamp(1), PostCheckpoint: true}})
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	_, nodes := rfWalk(t, f)
	if len(nodes) != 1 || nodes[0].Inode == nil || nodes[0].Inode.Checksum != "not-recorded" {
		t.Errorf("nodes = %+v, want one inode with checksum not-recorded", nodes)
	}
}

// rfInodeChain builds an image whose chain is the given inode nodes (nothing else).
func rfInodeChain(t *testing.T, nodes ...f2fstest.ChainNode) (*f2fs.FS, []byte, *f2fstest.Tree) {
	t.Helper()
	o := f2fstest.Options{NodeSegs: 2, Unclean: true, CPFlags: f2fstest.CPCRCRecovery, ExtraAttr: true, InodeChksum: true}
	o.Chain = nodes
	img, tree := f2fstest.BuildTree(o, nil)
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f, img, tree
}

func rfIno(in f2fstest.Inode, fsync, dent bool) f2fstest.ChainNode {
	o := f2fstest.Options{ExtraAttr: true, InodeChksum: true}
	in.Mode, in.Links, in.Extra = 0o100644, 1, true
	return f2fstest.ChainNode{Block: f2fstest.InodeBlock(o, in), Fsync: fsync, Dentry: dent}
}

func TestRollForwardOversizedExtraIsizeWarns(t *testing.T) {
	f, _, tree := rfInodeChain(t, rfIno(f2fstest.Inode{NID: 91, Size: 5, PIno: f2fstest.RootIno, Name: "x", ExtraIsize: 40}, true, true))
	before := f.Info().Warnings
	for _, stop := range []bool{true, false} { // an early stop still reports the whole chain
		c, err := f.RollForward(func(filesys.RollForwardNode) bool { return stop })
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, w := range c.Warnings {
			if strings.Contains(w, "i_extra_isize") && strings.Contains(w, fmt.Sprintf("block %d", tree.ChainAddrs[0])) {
				found = true
			}
		}
		if !found {
			t.Errorf("stop=%v: warnings %v lack the i_extra_isize warning naming the block", stop, c.Warnings)
		}
	}
	if after := f.Info().Warnings; !slices.Equal(before, after) {
		t.Errorf("RollForward changed the filesystem warnings: %v -> %v", before, after)
	}
}

func TestRollForwardFilesUseLastGoodInode(t *testing.T) {
	v1 := rfIno(f2fstest.Inode{NID: 91, Size: 100, PIno: f2fstest.RootIno, Name: "old"}, true, true)
	v2 := rfIno(f2fstest.Inode{NID: 91, Size: 200, PIno: f2fstest.RootIno, Name: "new"}, true, false)
	v3 := rfIno(f2fstest.Inode{NID: 91, Size: 300, PIno: f2fstest.RootIno, Name: "bad"}, true, false)
	_, img, tree := rfInodeChain(t, v1, v2, v3)
	f := rfCorrupt(t, img, tree.ChainAddrs[2])
	c, _ := rfWalk(t, f)
	if len(c.Files) != 1 {
		t.Fatalf("files = %+v", c.Files)
	}
	fl := c.Files[0]
	// Size from the last good inode node; name from the last good dentry-marked one (3H-R5).
	if !fl.HasInode || fl.Size != 200 || fl.Name != "old" || fl.InodeBlock != uint64(tree.ChainAddrs[1]) {
		t.Errorf("file = %+v, want size 200 from block %d and name old", fl, tree.ChainAddrs[1])
	}
}

func TestRollForwardFilesBadChecksumOnlyIsNoInode(t *testing.T) {
	_, img, tree := rfInodeChain(t, rfIno(f2fstest.Inode{NID: 91, Size: 300, PIno: f2fstest.RootIno, Name: "bad"}, true, true))
	f := rfCorrupt(t, img, tree.ChainAddrs[0])
	c, _ := rfWalk(t, f)
	fl := c.Files[0]
	if fl.HasInode || fl.Name != "" || fl.Size != 0 || fl.Pino != 0 || fl.InodeBlock != uint64(tree.ChainAddrs[0]) || fl.Caveat == "" {
		t.Errorf("file = %+v, want no usable inode, the block kept and a caveat", fl)
	}
}

func TestRollForwardFilesUnknownNameHasCaveat(t *testing.T) {
	in := f2fstest.Inode{NID: 91, Size: 7, PIno: f2fstest.RootIno, Name: "n", RawOverrides: map[int][]byte{88: {0x2c, 0x01, 0, 0}}}
	f, _, _ := rfInodeChain(t, rfIno(in, true, true))
	c, _ := rfWalk(t, f)
	fl := c.Files[0]
	if !fl.HasInode || fl.Size != 7 || fl.Name != "" || !strings.Contains(fl.Caveat, "name") {
		t.Errorf("file = %+v, want the size, no name and a name caveat", fl)
	}
}

// rfCorrupt flips a byte covered by the inode checksum of the chain inode at addr (no
// reseal) and reopens the image.
func rfCorrupt(t *testing.T, img []byte, addr uint32) *f2fs.FS {
	t.Helper()
	img[int(addr)*bs+4] ^= 0xff
	f, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A newer inode copy with a bad checksum makes the line show an older copy: the caveat says so, with the
// block of the bad copy, and a line whose last copy is good has none.
func TestRollForwardFilesNewerBadCopyHasCaveat(t *testing.T) {
	v1 := rfIno(f2fstest.Inode{NID: 91, Size: 100, PIno: f2fstest.RootIno, Name: "old"}, true, true)
	v2 := rfIno(f2fstest.Inode{NID: 91, Size: 200, PIno: f2fstest.RootIno, Name: "newer"}, true, false)
	_, img, tree := rfInodeChain(t, v1, v2)
	f := rfCorrupt(t, img, tree.ChainAddrs[1])
	c, _ := rfWalk(t, f)
	fl := c.Files[0]
	if !fl.HasInode || fl.Size != 100 || fl.Name != "old" || fl.InodeBlock != uint64(tree.ChainAddrs[0]) {
		t.Fatalf("file = %+v, want the older copy's size 100 and name old from block %d", fl, tree.ChainAddrs[0])
	}
	want := fmt.Sprintf("a newer inode copy (block %d) has a bad checksum", tree.ChainAddrs[1])
	if !strings.Contains(fl.Caveat, want) {
		t.Errorf("caveat %q lacks %q", fl.Caveat, want)
	}
	good, _, _ := rfInodeChain(t, v1, v2)
	if cg, _ := rfWalk(t, good); cg.Files[0].Caveat != "" {
		t.Errorf("clean chain caveat = %q, want none", cg.Files[0].Caveat)
	}
}
