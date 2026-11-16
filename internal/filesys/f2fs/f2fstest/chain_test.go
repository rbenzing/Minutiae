package f2fstest_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// newerPack returns the header block of the newer checkpoint pack of an image
// built with the default Options.Version (pack 1).
func newerPack(img []byte) []byte {
	return blk(img, le.Uint32(img[1024+76:]))
}

// wantChainVersion decodes the footer cp_ver the kernel expects, from the
// header block alone.
func wantChainVersion(hdr []byte) uint64 {
	ver, flags := le.Uint64(hdr), le.Uint32(hdr[132:])
	if flags&f2fstest.CPCRCRecovery != 0 && flags&f2fstest.CPNoCRCRecovery == 0 {
		return ver | uint64(le.Uint32(hdr[le.Uint32(hdr[164:]):]))<<32
	}
	return ver
}

func chainData(blocks int) []byte {
	b := make([]byte, blocks*bs)
	for i := range b {
		b[i] = byte(i/bs + 1)
	}
	return b
}

func chainOpts() f2fstest.Options {
	return f2fstest.Options{NodeSegs: 2, CPFlags: f2fstest.CPCRCRecovery, Unclean: true}
}

func TestChainNodesFollowTheWarmCursor(t *testing.T) {
	img, tree := f2fstest.BuildTree(chainOpts(), []f2fstest.File{
		{Path: "/rf", Dir: true},
		{Path: "/rf/a.bin", Data: chainData(3), PostCheckpoint: true},
	})
	hdr := newerPack(img)
	l := f2fstest.Geometry(chainOpts())
	if got := le.Uint32(hdr[132:]); got&f2fstest.CPCRCRecovery == 0 {
		t.Fatalf("ckpt_flags = %#x, want CPCRCRecovery set", got)
	}
	seg, off := le.Uint32(hdr[36+4:]), uint32(le.Uint16(hdr[68+2:]))
	addr := l.Main + seg*segBlocks + off
	ino := tree.NID["/rf/a.bin"]
	nid, fino, flag, ver, next := footer(blk(img, addr))
	if nid != ino || fino != ino || flag != f2fstest.FlagFsync|f2fstest.FlagDent|f2fstest.FlagCold {
		t.Errorf("footer at the warm cursor = nid %d ino %d flag %#x, want inode %d flag 0x7", nid, fino, flag, ino)
	}
	if want := wantChainVersion(hdr); want>>32 == 0 || ver != want {
		t.Errorf("cp_ver = %#x, want ver|crc<<32 = %#x", ver, want)
	}
	if next != addr+1 {
		t.Errorf("next_blkaddr = %d, want %d", next, addr+1)
	}
	if !slices.Equal(tree.ChainAddrs, []uint32{addr}) || !slices.Equal(tree.ChainOf["/rf/a.bin"], []uint32{addr}) {
		t.Errorf("ChainAddrs = %v, ChainOf = %v, want [%d]", tree.ChainAddrs, tree.ChainOf, addr)
	}
	for _, second := range []bool{false, true} {
		if e := natEntry(img, l, ino, second); !bytes.Equal(e, make([]byte, 9)) {
			t.Errorf("NAT entry of the chain inode (copy2=%v) = %x, want none", second, e)
		}
	}
	if validBit(img, l, addr) {
		t.Error("SIT bit of the chain inode is set")
	}
	if len(tree.Data["/rf/a.bin"]) != 3 {
		t.Fatalf("Tree.Data has %d blocks, want 3", len(tree.Data["/rf/a.bin"]))
	}
	for i, d := range tree.Data["/rf/a.bin"] {
		if validBit(img, l, d) {
			t.Errorf("SIT bit of data block %d is set", i)
		}
		if !bytes.Equal(blk(img, d), chainData(3)[i*bs:(i+1)*bs]) {
			t.Errorf("data block %d was not written", i)
		}
	}
	if len(tree.DirBlocks["/rf"]) == 0 {
		t.Error("the parent directory has no dentry block to check")
	}
	for _, d := range tree.DirBlocks["/rf"] {
		if bytes.Contains(blk(img, d), []byte("a.bin")) {
			t.Error("the parent directory has a record for the post-checkpoint file")
		}
	}
}

func TestChainFooterVersionNoCRC(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags uint32
		high  bool
	}{
		{"none", 0, false},
		{"crc", f2fstest.CPCRCRecovery, true},
		{"nocrc", f2fstest.CPNoCRCRecovery | f2fstest.CPCRCRecovery, false},
	} {
		img := f2fstest.Build(f2fstest.Options{CPFlags: tc.flags}, nil)
		got := f2fstest.ChainFooterVersion(img)
		if (got>>32 != 0) != tc.high || uint32(got) != 2 {
			t.Errorf("%s: ChainFooterVersion = %#x, want version 2, high half set=%v", tc.name, got, tc.high)
		}
		if want := wantChainVersion(newerPack(img)); got != want {
			t.Errorf("%s: ChainFooterVersion = %#x, want %#x", tc.name, got, want)
		}
	}
}

func TestPostCheckpointDirectNodeOrder(t *testing.T) {
	img, tree := f2fstest.BuildTree(chainOpts(), []f2fstest.File{{Path: "/big", Data: chainData(1000), PostCheckpoint: true}})
	c := tree.ChainOf["/big"]
	if len(c) != 2 || !slices.Equal(tree.ChainAddrs, c) || c[1] != c[0]+1 {
		t.Fatalf("ChainOf = %v ChainAddrs = %v, want inode then one direct node", c, tree.ChainAddrs)
	}
	_, _, f0, _, _ := footer(blk(img, c[0]))
	n1, i1, f1, _, _ := footer(blk(img, c[1]))
	if f0 != 0x7 || f1 != 1<<3|f2fstest.FlagFsync|f2fstest.FlagCold || i1 != tree.NID["/big"] || n1 == i1 {
		t.Errorf("flags = %#x, %#x (nid %d ino %d), want 0x7 and ofs 1 fsync|cold", f0, f1, n1, i1)
	}
}

func TestPostCheckpointIndirectStaysOutOfTheChain(t *testing.T) {
	for _, stale := range []bool{false, true} {
		img, tree := f2fstest.BuildTree(chainOpts(), []f2fstest.File{
			{Path: "/ind", Data: chainData(923 + 2*1018 + 5), PostCheckpoint: true, StaleIndirect: stale},
		})
		l := f2fstest.Geometry(chainOpts())
		ino := tree.NID["/ind"]
		if len(tree.ChainAddrs) == 0 {
			t.Fatalf("stale=%v: empty chain", stale)
		}
		indNID := le.Uint32(blk(img, tree.ChainAddrs[0])[4052+4*2:])
		if indNID == 0 {
			t.Fatal("the inode has no indirect node id")
		}
		// inode, direct ofs 1, direct ofs 2, direct ofs 4 (under the indirect node)
		var ofs []uint32
		for _, a := range tree.ChainAddrs {
			nid, _, flag, _, _ := footer(blk(img, a))
			if nid == indNID {
				t.Errorf("stale=%v: the indirect node is in the chain", stale)
			}
			ofs = append(ofs, flag>>3)
		}
		if fmt.Sprint(ofs) != "[0 1 2 4]" {
			t.Errorf("stale=%v: chain ofs = %v, want [0 1 2 4]", stale, ofs)
		}
		for _, second := range []bool{false, true} {
			if e := natEntry(img, l, indNID, second); !bytes.Equal(e, make([]byte, 9)) {
				t.Errorf("stale=%v: NAT entry of the indirect node = %x, want none", stale, e)
			}
		}
		var found []uint32
		for a := l.Main; a < l.BlockCount; a++ {
			if nid, fino, _, _, _ := footer(blk(img, a)); nid == indNID && fino == ino {
				found = append(found, a)
			}
		}
		switch {
		case !stale && len(found) != 0:
			t.Errorf("omitted by default, but the indirect node is written at %v", found)
		case stale && len(found) != 1:
			t.Errorf("StaleIndirect: indirect node found at %v, want one block", found)
		case stale:
			if validBit(img, l, found[0]) || slices.Contains(tree.ChainAddrs, found[0]) {
				t.Errorf("stale copy at %d is in the chain or has its SIT bit set", found[0])
			}
		}
	}
}

func TestChainOverrides(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, CPFlags: f2fstest.CPCRCRecovery}
	l := f2fstest.Geometry(o)
	node := func(nid uint32) []byte {
		return f2fstest.InodeBlock(o, f2fstest.Inode{NID: nid, Mode: 0o100644, Links: 1})
	}
	start := l.Main + (l.MainSegs-2)*segBlocks
	ver, next, far := uint64(0xabcdef), uint32(77), start+40
	o.Chain = []f2fstest.ChainNode{
		{Block: node(10), Fsync: true},
		{Block: node(11), Fsync: true, Dentry: true, CPVer: &ver, Next: &next},
		{Block: node(12), Addr: far},
	}
	img := f2fstest.Build(o, nil)
	hdr := newerPack(img)
	if seg, off := le.Uint32(hdr[36+4:]), uint32(le.Uint16(hdr[68+2:])); l.Main+seg*segBlocks+off != start {
		t.Errorf("default warm cursor = block %d, want the first node-segment block %d", l.Main+seg*segBlocks+off, start)
	}
	want := f2fstest.ChainFooterVersion(img)
	n0, _, f0, v0, x0 := footer(blk(img, start))
	n1, _, f1, v1, x1 := footer(blk(img, start+1))
	n2, _, f2, v2, x2 := footer(blk(img, far))
	if n0 != 10 || f0 != f2fstest.FlagFsync || v0 != want || x0 != start+1 {
		t.Errorf("node 10: nid %d flag %#x ver %#x next %d", n0, f0, v0, x0)
	}
	if n1 != 11 || f1 != f2fstest.FlagFsync|f2fstest.FlagDent || v1 != ver || x1 != next {
		t.Errorf("node 11: nid %d flag %#x ver %#x next %d, want overrides", n1, f1, v1, x1)
	}
	if n2 != 12 || f2 != 0 || v2 != want || x2 != far+1 {
		t.Errorf("node 12 at %d: nid %d flag %#x ver %#x next %d", far, n2, f2, v2, x2)
	}
	if got := blk(img, start+2); !bytes.Equal(got, make([]byte, bs)) {
		t.Error("the block after the second node was written although the third node has an Addr")
	}
}

func TestChainInodeChecksumResealed(t *testing.T) {
	o := chainOpts()
	o.ExtraAttr, o.InodeChksum = true, true
	o.UUID = [16]byte{9, 8, 7}
	img, tree := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/f", Data: chainData(2), PostCheckpoint: true}})
	if len(tree.ChainAddrs) == 0 {
		t.Fatal("empty chain")
	}
	b := blk(img, tree.ChainAddrs[0])
	if got, want := le.Uint32(b[368:]), f2fstest.InodeChecksum(o, b); got != want {
		t.Errorf("inode checksum = %#x, want %#x", got, want)
	}
}

func TestChainNeedsNodeSegs(t *testing.T) {
	panicked := func() (p bool) {
		defer func() { p = recover() != nil }()
		f2fstest.Build(f2fstest.Options{Chain: []f2fstest.ChainNode{{Block: make([]byte, bs)}}}, nil)
		return false
	}()
	if !panicked {
		t.Error("a chain without NodeSegs did not panic")
	}
}

func TestExistingImagesUnchangedByChainFields(t *testing.T) {
	// The files of the reader's TestReadDirFlagsDeletedEntries; the digest is
	// the constant TestExistingImagesUnchanged pins.
	longName := "a-long-deleted-name-0123456789-abcdefgh"
	files := []f2fstest.File{
		{Path: "/keep", Data: []byte("keep")},
		{Path: "/gone.txt", Data: []byte("gone"), Deleted: true},
		{Path: "/" + longName, Data: []byte("L"), Deleted: true},
		{Path: "/tail", Data: []byte("tail")},
		{Path: "/orphan", Deleted: true, NoInode: true},
		{Path: "/reused", Data: []byte("new"), Deleted: true},
		{Path: "/bad\x00name", Data: []byte("z"), Deleted: true},
		{Path: "/slash", RawName: []byte("a/b"), Deleted: true},
		{Path: "/last", Data: []byte("last")},
		{Path: "/sub", Dir: true},
		{Path: "/sub/deep", Data: []byte("deep"), Deleted: true},
		{Path: "/sub/deep2", Data: []byte("deep2"), Deleted: false},
	}
	const want = "fbc9337901ec6890eecad22c5d5f28e7cabc002ad087bf545fd55ff4ecec0f8f"
	if got := fmt.Sprintf("%x", sha256.Sum256(f2fstest.Build(f2fstest.Options{}, files))); got != want {
		t.Errorf("sha256 of the image = %s, want %s", got, want)
	}
}

func TestIsIndirectOfs(t *testing.T) {
	// direct nodes 1, 2; indirect 3 with direct 4..1021; indirect 1022 with direct 1023..2040;
	// double-indirect 2041; its indirect children 2042, 3061, ... each followed by 1018 direct nodes.
	for ofs, want := range map[uint32]bool{
		0: false, 1: false, 2: false, 3: true, 4: false, 1021: false, 1022: true, 1023: false, 2040: false,
		2041: true, 2042: true, 2043: false, 3060: false, 3061: true, 3062: false, 4080: true, 4081: false,
	} {
		if got := f2fstest.IsIndirectOfs(ofs); got != want {
			t.Errorf("isIndirectOfs(%d) = %v, want %v", ofs, got, want)
		}
	}
}

func TestPostCheckpointDirectNodesHaveNoNATAndClearSIT(t *testing.T) {
	img, tree := f2fstest.BuildTree(chainOpts(), []f2fstest.File{{Path: "/big", Data: chainData(1000), PostCheckpoint: true}})
	l := f2fstest.Geometry(chainOpts())
	if len(tree.Nodes["/big"]) != 2 || len(tree.ChainOf["/big"]) != 2 {
		t.Fatalf("Nodes = %v, ChainOf = %v, want an inode and one direct node", tree.Nodes["/big"], tree.ChainOf["/big"])
	}
	for i, nid := range tree.Nodes["/big"] {
		for _, second := range []bool{false, true} {
			if e := natEntry(img, l, nid, second); !bytes.Equal(e, make([]byte, 9)) {
				t.Errorf("node %d (nid %d) has NAT entry %x (copy2=%v), want none", i, nid, e, second)
			}
		}
		if validBit(img, l, tree.ChainOf["/big"][i]) {
			t.Errorf("SIT bit of chain node %d is set", i)
		}
	}
}

func TestChainWarmCursorInBothPacks(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    f2fstest.Options
		want func(l f2fstest.Layout) uint32
	}{
		{"default cursor", chainOpts(), nil},
		{"explicit cursor", func() f2fstest.Options {
			o := chainOpts()
			o.CurNode[1] = f2fstest.CurSeg{Seg: 5, Off: 3}
			return o
		}(), func(l f2fstest.Layout) uint32 { return l.Main + 5*segBlocks + 3 }},
	} {
		img, tree := f2fstest.BuildTree(tc.o, []f2fstest.File{{Path: "/a", Data: chainData(1), PostCheckpoint: true}})
		l := f2fstest.Geometry(tc.o)
		want := tree.ChainAddrs[0]
		if tc.want != nil {
			if want = tc.want(l); tree.ChainAddrs[0] != want {
				t.Fatalf("%s: chain starts at %d, want the cursor %d", tc.name, tree.ChainAddrs[0], want)
			}
		}
		base := le.Uint32(img[1024+76:])
		for pack := range 2 {
			hdr := blk(img, base+uint32(pack)*segBlocks)
			got := l.Main + le.Uint32(hdr[36+4:])*segBlocks + uint32(le.Uint16(hdr[68+2:]))
			if got != want {
				t.Errorf("%s: pack %d warm cursor = block %d, want %d", tc.name, pack, got, want)
			}
		}
	}
}

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	f()
}

func TestPostCheckpointWithCPVerPanics(t *testing.T) {
	mustPanic(t, "PostCheckpoint with CPVer", func() {
		f2fstest.BuildTree(chainOpts(), []f2fstest.File{{Path: "/a", Data: chainData(1), PostCheckpoint: true, CPVer: 5}})
	})
}

func TestChainAddrCollisionPanics(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, CPFlags: f2fstest.CPCRCRecovery}
	l := f2fstest.Geometry(o)
	start := l.Main + (l.MainSegs-2)*segBlocks
	node := func(nid uint32) []byte {
		return f2fstest.InodeBlock(o, f2fstest.Inode{NID: nid, Mode: 0o100644, Links: 1})
	}
	existing := f2fstest.Node{NID: 20, Addr: start + 9, Block: node(20)}
	mustPanic(t, "a chain Addr on an existing node", func() {
		c := o
		c.Nodes = []f2fstest.Node{existing}
		c.Chain = []f2fstest.ChainNode{{Block: node(10)}, {Block: node(11), Addr: existing.Addr}}
		f2fstest.Build(c, nil)
	})
	mustPanic(t, "a chain Addr on a stale node", func() {
		c := o
		c.StaleNodes = []f2fstest.Node{existing}
		c.Chain = []f2fstest.ChainNode{{Block: node(10), Addr: existing.Addr}}
		f2fstest.Build(c, nil)
	})
	mustPanic(t, "a chain Addr on a data block", func() {
		c := o
		c.Data = []f2fstest.DataBlock{{Addr: start + 20, Data: []byte("x")}}
		c.Chain = []f2fstest.ChainNode{{Block: node(10), Addr: start + 20}}
		f2fstest.Build(c, nil)
	})
	mustPanic(t, "two chain elements at one address", func() {
		c := o
		c.Chain = []f2fstest.ChainNode{{Block: node(10)}, {Block: node(11), Addr: start}}
		f2fstest.Build(c, nil)
	})
	// distinct addresses are fine
	c := o
	c.Nodes = []f2fstest.Node{existing}
	c.Chain = []f2fstest.ChainNode{{Block: node(10), Addr: start + 30}, {Block: node(11), Addr: start + 31}}
	f2fstest.Build(c, nil)
}
