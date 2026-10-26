package f2fstest_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// The decoders below read the image straight from the kernel layout (offsets
// of include/linux/f2fs_fs.h); they use only f2fstest.Geometry for the area
// addresses and never the reader.

const (
	bs        = f2fstest.BlockSize
	segBlocks = f2fstest.BlocksPerSeg
)

var le = binary.LittleEndian

func blk(img []byte, addr uint32) []byte { return img[int(addr)*bs : (int(addr)+1)*bs] }

// natEntry returns the 9 NAT bytes of nid in copy 1 or 2.
func natEntry(img []byte, l f2fstest.Layout, nid uint32, second bool) []byte {
	b := blk(img, l.NATBlock(int(nid/f2fstest.NATPerBlock), second))
	off := int(nid%f2fstest.NATPerBlock) * 9
	return b[off : off+9]
}

// sitEntry returns vblocks and the valid map of main segment seg (copy 1: the
// tests keep the default all-zero SIT version bitmap).
func sitEntry(img []byte, l f2fstest.Layout, seg uint32) (uint16, []byte) {
	b := blk(img, l.SITBlock(int(seg/f2fstest.SITPerBlock), false))
	off := int(seg%f2fstest.SITPerBlock) * f2fstest.SITEntrySize
	return le.Uint16(b[off:]), b[off+2 : off+66]
}

func validBit(img []byte, l f2fstest.Layout, addr uint32) bool {
	_, m := sitEntry(img, l, (addr-l.Main)/segBlocks)
	i := int((addr - l.Main) % segBlocks)
	return m[i/8]&(0x80>>(i%8)) != 0
}

// footer returns nid, ino, flag, cp_ver and next_blkaddr of a node block.
func footer(b []byte) (nid, ino, flag uint32, ver uint64, next uint32) {
	return le.Uint32(b[4072:]), le.Uint32(b[4076:]), le.Uint32(b[4080:]), le.Uint64(b[4084:]), le.Uint32(b[4092:])
}

// ssaEntry returns the nid of summary entry i of main segment seg and the
// ofs_in_node of the entry; ssaType is the footer entry_type (byte 4091).
func ssaEntry(img []byte, l f2fstest.Layout, seg uint32, i int) (uint32, uint16) {
	b := blk(img, l.SSA+seg)
	return le.Uint32(b[7*i:]), le.Uint16(b[7*i+5:])
}

func ssaType(img []byte, l f2fstest.Layout, seg uint32) byte { return blk(img, l.SSA+seg)[4091] }

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i/bs)*7 + 1
	}
	return b
}

func TestUnlinkedFileLeavesStaleNodesAndData(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, SSA: true}
	data := pattern(3*bs, 0x40)
	files := []f2fstest.File{
		{Path: "/d/live.bin", Data: pattern(bs, 0x10)},
		{Path: "/d/gone.bin", Data: data, Deleted: true, Unlinked: true},
	}
	img, tree := f2fstest.BuildTree(o, files)
	l := f2fstest.Geometry(o)
	nid := tree.NID["/d/gone.bin"]

	for _, second := range []bool{false, true} {
		if e := natEntry(img, l, nid, second); !bytes.Equal(e, make([]byte, 9)) {
			t.Errorf("NAT entry of nid %d (second copy %v) = %x, want all zero", nid, second, e)
		}
	}
	live := tree.NID["/d/live.bin"]
	if e := natEntry(img, l, live, false); le.Uint32(e[5:]) != tree.Addr[live] {
		t.Errorf("live file's NAT address = %d, want %d", le.Uint32(e[5:]), tree.Addr[live])
	}

	addr := tree.Addr[nid]
	if addr < l.Main {
		t.Fatalf("inode address %d is outside the main area", addr)
	}
	if fn, fi, _, _, _ := footer(blk(img, addr)); fn != nid || fi != nid {
		t.Errorf("stale inode footer nid/ino = %d/%d, want %d/%d", fn, fi, nid, nid)
	}
	if validBit(img, l, addr) {
		t.Error("SIT bit of the stale inode block is set")
	}
	if !validBit(img, l, tree.Addr[live]) {
		t.Error("SIT bit of the live inode block is clear")
	}
	dataAddrs := tree.Data["/d/gone.bin"]
	if len(dataAddrs) != 3 {
		t.Fatalf("Tree.Data = %v, want 3 addresses", dataAddrs)
	}
	for i, a := range dataAddrs {
		if validBit(img, l, a) {
			t.Errorf("SIT bit of stale data block %d (addr %d) is set", i, a)
		}
		if !bytes.Equal(blk(img, a), data[i*bs:(i+1)*bs]) {
			t.Errorf("data block %d bytes are not at Tree.Data address %d", i, a)
		}
	}
	for _, a := range tree.Data["/d/live.bin"] {
		if !validBit(img, l, a) {
			t.Errorf("SIT bit of live data block %d is clear", a)
		}
	}
	// The vblocks of the segments agree with their valid maps.
	for seg := range l.MainSegs {
		vb, m := sitEntry(img, l, seg)
		n := 0
		for _, c := range m {
			for ; c != 0; c &= c - 1 {
				n++
			}
		}
		if int(vb&0x3ff) != n {
			t.Errorf("segment %d: vblocks %d but %d valid bits", seg, vb&0x3ff, n)
		}
	}

	// The dentry is deleted: bitmap bit clear, ino and name kept in the slot.
	db := blk(img, tree.DirBlocks["/d"][0])
	const slot = 3 // ".", "..", live.bin, gone.bin
	if db[slot/8]&(1<<(slot%8)) != 0 {
		t.Error("dentry bitmap bit of the deleted entry is set")
	}
	if ino := le.Uint32(db[30+11*slot+4:]); ino != nid {
		t.Errorf("deleted dentry ino = %d, want %d", ino, nid)
	}
	if name := db[30+214*11+8*slot:][:8]; string(name) != "gone.bin" {
		t.Errorf("deleted dentry name = %q", name)
	}
}

func TestNodeSegsTypesSegments(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, SSA: true}
	files := []f2fstest.File{
		{Path: "/a.bin", Data: pattern(2*bs, 0x20)},
		{Path: "/d/b.bin", Data: pattern(bs, 0x30)},
		{Path: "/d/gone.bin", Data: pattern(bs, 0x50), Deleted: true, Unlinked: true},
	}
	img, tree := f2fstest.BuildTree(o, files)
	l := f2fstest.Geometry(o)
	first := l.MainSegs - 2 // first node segment

	for seg := range l.MainSegs {
		vb, _ := sitEntry(img, l, seg)
		wantSIT, wantSSA := uint16(1), byte(0)
		if seg >= first {
			wantSIT, wantSSA = 4, 1
		}
		if vb>>10 != wantSIT {
			t.Errorf("segment %d: SIT type %d, want %d", seg, vb>>10, wantSIT)
		}
		if got := ssaType(img, l, seg); got != wantSSA {
			t.Errorf("segment %d: SSA footer type %d, want %d", seg, got, wantSSA)
		}
	}
	for path, nids := range tree.Nodes {
		for _, nid := range nids {
			addr := tree.Addr[nid]
			seg := (addr - l.Main) / segBlocks
			if seg < first {
				t.Errorf("%s: node %d at segment %d, want a node segment (>= %d)", path, nid, seg, first)
			}
			fn, _, _, _, _ := footer(blk(img, addr))
			if fn != nid {
				t.Errorf("%s: footer nid %d at addr %d, want %d", path, fn, addr, nid)
			}
			if got, _ := ssaEntry(img, l, seg, int((addr-l.Main)%segBlocks)); got != nid {
				t.Errorf("%s: SSA entry nid %d for node %d", path, got, nid)
			}
		}
	}
	for path, addrs := range tree.Data {
		inode := tree.NID[path]
		for i, a := range addrs {
			seg := (a - l.Main) / segBlocks
			if seg >= first {
				t.Errorf("%s: data block %d at node segment %d", path, i, seg)
			}
			if nid, ofs := ssaEntry(img, l, seg, int((a-l.Main)%segBlocks)); nid != inode || int(ofs) != i {
				t.Errorf("%s: SSA entry of data block %d = {%d, %d}, want {%d, %d}", path, i, nid, ofs, inode, i)
			}
		}
	}
	if len(tree.Nodes["/a.bin"]) != 1 || tree.Nodes["/a.bin"][0] != tree.NID["/a.bin"] {
		t.Errorf("Tree.Nodes[/a.bin] = %v, want just the inode %d", tree.Nodes["/a.bin"], tree.NID["/a.bin"])
	}
}

func TestFileCPVerStampsEveryNodeBlock(t *testing.T) {
	// 925 blocks need one direct node beyond the 923 inode slots.
	files := []f2fstest.File{
		{Path: "/big", Data: pattern(925*bs, 1), CPVer: 0x1_0000_0007},
		{Path: "/plain", Data: pattern(bs, 2)},
	}
	img, tree := f2fstest.BuildTree(f2fstest.Options{}, files)
	if n := len(tree.Nodes["/big"]); n != 2 {
		t.Fatalf("Tree.Nodes[/big] has %d nodes, want inode + direct node", n)
	}
	for _, nid := range tree.Nodes["/big"] {
		if _, _, _, ver, _ := footer(blk(img, tree.Addr[nid])); ver != 0x1_0000_0007 {
			t.Errorf("node %d footer cp_ver = %#x, want 0x100000007", nid, ver)
		}
	}
	if _, _, _, ver, _ := footer(blk(img, tree.Addr[tree.NID["/plain"]])); ver != 0 {
		t.Errorf("default footer cp_ver = %#x, want 0", ver)
	}
	if got := len(tree.Data["/big"]); got != 925 {
		t.Errorf("Tree.Data[/big] has %d addresses, want 925", got)
	}
}

func TestExistingImagesUnchanged(t *testing.T) {
	// The files of the reader's TestReadDirFlagsDeletedEntries. The constant is
	// the digest the builder gave before this task (a deleted file keeps its NAT
	// entry and valid blocks; footers carry cp_ver 0).
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

func TestSetFooterAndReseal(t *testing.T) {
	o := f2fstest.Options{ExtraAttr: true, InodeChksum: true, UUID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}
	img, tree := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/f", Data: []byte("hello")}})
	addr := tree.Addr[tree.NID["/f"]]

	// recompute is the checksum an inode block of this volume must carry.
	recompute := func() uint32 {
		b := blk(img, addr)
		raw := func(seed uint32, p []byte) uint32 { return ^crc32.Update(^seed, crc32.IEEETable, p) }
		c := raw(raw(^uint32(0), o.UUID[:]), b[4076:4080])
		c = raw(c, b[68:72])
		c = raw(c, b[:368])
		c = raw(c, []byte{0, 0, 0, 0})
		return raw(c, b[372:])
	}
	if le.Uint32(blk(img, addr)[368:]) != recompute() {
		t.Fatal("builder's own checksum disagrees with the in-test recomputation")
	}

	want := f2fstest.Footer{NID: 77, Ino: 78, Flag: 3 << 7, CPVer: 0x1122334455667788, NextBlk: 0xAABBCCDD}
	f2fstest.SetFooter(img, addr, want)
	nid, ino, flag, ver, next := footer(blk(img, addr))
	if nid != 77 || ino != 78 || flag != 3<<7 || ver != 0x1122334455667788 || next != 0xAABBCCDD {
		t.Fatalf("footer = %d %d %#x %#x %#x", nid, ino, flag, ver, next)
	}
	if le.Uint32(blk(img, addr)[368:]) == recompute() {
		t.Fatal("the footer ino changed but the checksum still agrees: SetFooter must not reseal")
	}
	f2fstest.ResealInode(o, img, addr)
	if got, want := le.Uint32(blk(img, addr)[368:]), recompute(); got != want {
		t.Errorf("checksum after ResealInode = %#x, want %#x", got, want)
	}
	// Without the feature ResealInode leaves the block alone.
	before := slices.Clone(blk(img, addr))
	f2fstest.ResealInode(f2fstest.Options{}, img, addr)
	if !bytes.Equal(before, blk(img, addr)) {
		t.Error("ResealInode changed a block of a volume without inode_checksum")
	}
}

func TestCurNodeWrittenToBothPacks(t *testing.T) {
	cur := [3]f2fstest.CurSeg{{Seg: 11, Off: 5}, {Seg: 12, Off: 300}, {Seg: 13, Off: 511}}
	o := f2fstest.Options{CurNode: cur}
	img := f2fstest.Build(o, nil)
	l := f2fstest.Geometry(o)
	for _, base := range []uint32{l.CP, l.CP + segBlocks} {
		b := blk(img, base)
		for i, c := range cur {
			if got := le.Uint32(b[36+4*i:]); got != c.Seg {
				t.Errorf("pack at %d: cur_node_segno[%d] = %d, want %d", base, i, got, c.Seg)
			}
			if got := le.Uint16(b[68+2*i:]); got != c.Off {
				t.Errorf("pack at %d: cur_node_blkoff[%d] = %d, want %d", base, i, got, c.Off)
			}
		}
	}
	// The default leaves them zero.
	b := blk(f2fstest.Build(f2fstest.Options{}, nil), l.CP)
	if !bytes.Equal(b[36:84], make([]byte, 48)) {
		t.Errorf("default checkpoint has non-zero cursors: %x", b[36:84])
	}
}

func TestStaleNodesHaveNoNATAndClearSIT(t *testing.T) {
	base := f2fstest.Options{}
	l := f2fstest.Geometry(base)
	in := f2fstest.Inode{NID: 40, Mode: 0o100644, Links: 1, PIno: 3, Name: "old"}
	stale := f2fstest.Node{NID: 40, Addr: l.Main + 3*segBlocks + 5, Block: f2fstest.InodeBlock(base, in)}
	liveNode := f2fstest.Node{NID: 41, Addr: l.Main + 3*segBlocks + 6, Block: f2fstest.InodeBlock(base, f2fstest.Inode{NID: 41, Mode: 0o100644, Links: 1})}
	flagged := f2fstest.Node{NID: 42, Addr: l.Main + 3*segBlocks + 7, Block: f2fstest.InodeBlock(base, f2fstest.Inode{NID: 42, Mode: 0o100644, Links: 1}), Stale: true}

	o := base
	o.StaleNodes = []f2fstest.Node{stale}
	o.Nodes = []f2fstest.Node{liveNode, flagged}
	o.Data = []f2fstest.DataBlock{
		{Addr: l.Main + 5*segBlocks, Data: []byte("live data")},
		{Addr: l.Main + 5*segBlocks + 1, Data: []byte("stale data"), Stale: true},
	}
	img := f2fstest.Build(o, nil)

	for _, n := range []f2fstest.Node{stale, flagged} {
		if !bytes.Equal(blk(img, n.Addr), n.Block) {
			t.Errorf("node %d block is not written at %d", n.NID, n.Addr)
		}
		for _, second := range []bool{false, true} {
			if e := natEntry(img, l, n.NID, second); !bytes.Equal(e, make([]byte, 9)) {
				t.Errorf("node %d has NAT entry %x (second copy %v)", n.NID, e, second)
			}
		}
		if validBit(img, l, n.Addr) {
			t.Errorf("SIT bit of stale node %d is set", n.NID)
		}
	}
	if e := natEntry(img, l, 41, false); le.Uint32(e[5:]) != liveNode.Addr {
		t.Errorf("live node NAT address = %d, want %d", le.Uint32(e[5:]), liveNode.Addr)
	}
	if !validBit(img, l, liveNode.Addr) {
		t.Error("SIT bit of the live node is clear")
	}
	if !validBit(img, l, l.Main+5*segBlocks) || validBit(img, l, l.Main+5*segBlocks+1) {
		t.Error("data block SIT bits: want live set and stale clear")
	}
	if got := blk(img, l.Main+5*segBlocks+1)[:10]; string(got) != "stale data" {
		t.Errorf("stale data block not written: %q", got)
	}
	if vb, _ := sitEntry(img, l, 3); vb&0x3ff != 1 {
		t.Errorf("segment 3 vblocks = %d, want 1 (only the live node)", vb&0x3ff)
	}

	defer func() {
		if recover() == nil {
			t.Error("a StaleNodes entry without an address did not panic")
		}
	}()
	f2fstest.Build(f2fstest.Options{StaleNodes: []f2fstest.Node{{NID: 9, Block: make([]byte, bs)}}}, nil)
}

func TestSITAndSSAPatchHelpers(t *testing.T) {
	o := f2fstest.Options{}
	img, tree := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/f", Data: pattern(bs, 9)}})
	l := f2fstest.Geometry(o)
	addr := tree.Addr[tree.NID["/f"]]
	seg := (addr - l.Main) / segBlocks
	vb0, _ := sitEntry(img, l, seg)

	f2fstest.SetSITBit(img, o, addr, false)
	vb, _ := sitEntry(img, l, seg)
	if validBit(img, l, addr) || vb&0x3ff != vb0&0x3ff-1 {
		t.Errorf("after clearing: bit set %v, vblocks %d (was %d)", validBit(img, l, addr), vb&0x3ff, vb0&0x3ff)
	}
	f2fstest.SetSITBit(img, o, addr, false) // idempotent
	if vb2, _ := sitEntry(img, l, seg); vb2 != vb {
		t.Errorf("clearing twice changed vblocks: %d then %d", vb, vb2)
	}
	f2fstest.SetSITBit(img, o, addr, true)
	if vb3, _ := sitEntry(img, l, seg); !validBit(img, l, addr) || vb3 != vb0 {
		t.Errorf("after setting again: vblocks %#x, want %#x", vb3, vb0)
	}

	f2fstest.SetSITType(img, o, seg, 5)
	if vb4, _ := sitEntry(img, l, seg); vb4>>10 != 5 || vb4&0x3ff != vb0&0x3ff {
		t.Errorf("after SetSITType: vblocks %#x", vb4)
	}

	f2fstest.SetSSAEntry(img, o, addr, 1234)
	if nid, _ := ssaEntry(img, l, seg, int((addr-l.Main)%segBlocks)); nid != 1234 {
		t.Errorf("SSA entry nid = %d, want 1234", nid)
	}
	f2fstest.SetSSAType(img, o, seg, 1)
	if got := ssaType(img, l, seg); got != 1 {
		t.Errorf("SSA footer type = %d, want 1", got)
	}
}

func TestSSAWithoutNodeSegsLeavesFooterTypeZero(t *testing.T) {
	// An interleaved segment holds both kinds of block, so it is not marked as a
	// node segment; recovery tests set NodeSegs.
	o := f2fstest.Options{SSA: true}
	img, tree := f2fstest.BuildTree(o, []f2fstest.File{{Path: "/f", Data: pattern(bs, 3)}})
	l := f2fstest.Geometry(o)
	for seg := range l.MainSegs {
		if got := ssaType(img, l, seg); got != 0 {
			t.Errorf("segment %d: SSA footer type %d, want 0", seg, got)
		}
	}
	addr := tree.Addr[tree.NID["/f"]]
	if nid, _ := ssaEntry(img, l, (addr-l.Main)/segBlocks, int((addr-l.Main)%segBlocks)); nid != tree.NID["/f"] {
		t.Errorf("SSA entry nid = %d, want the inode %d", nid, tree.NID["/f"])
	}
}

// With NodeSegs the tree nodes record ofs_of_node as the kernel does (the
// footer flag word holds it above bit 7): 1 and 2 for the direct nodes of
// i_nid[0..1], 3 for the indirect node of i_nid[2] and 4, 5, ... for its
// direct children. Without NodeSegs the old images keep a zero flag.
func TestNodeSegsRecordNodeOffsets(t *testing.T) {
	files := []f2fstest.File{{Path: "/big", Data: pattern((923+2*1018+1018+5)*bs, 3)}}
	img, tree := f2fstest.BuildTree(f2fstest.Options{NodeSegs: 2}, files)
	inode := blk(img, tree.Addr[tree.NID["/big"]])
	ofs := func(nid uint32) uint32 { _, _, flag, _, _ := footer(blk(img, tree.Addr[nid])); return flag >> 7 }
	nid := func(b []byte, i int) uint32 { return le.Uint32(b[4052+4*i:]) }
	for slot, want := range []uint32{1, 2, 3} {
		if got := ofs(nid(inode, slot)); got != want {
			t.Errorf("i_nid[%d] has ofs %d, want %d", slot, got, want)
		}
	}
	ind := blk(img, tree.Addr[nid(inode, 2)])
	for k, want := range []uint32{4, 5} {
		if got := ofs(le.Uint32(ind[4*k:])); got != want {
			t.Errorf("child %d of the indirect node has ofs %d, want %d", k, got, want)
		}
	}
	plainImg, plainTree := f2fstest.BuildTree(f2fstest.Options{}, files[:1])
	n := nid(blk(plainImg, plainTree.Addr[plainTree.NID["/big"]]), 0)
	if _, _, flag, _, _ := footer(blk(plainImg, plainTree.Addr[n])); flag != 0 {
		t.Errorf("an image without NodeSegs has footer flag %#x, want 0 (existing images stay byte-identical)", flag)
	}
}

// The deep parts of the node tree record their ofs_of_node too (T6 carry-over):
// a sparse file with one block under i_nid[3] and one under the double
// indirect node i_nid[4]. i_nid[3] is node 1022, its first direct child 1023;
// the double indirect node is 2041, its first indirect child 2042 and that
// one's first direct child 2043 (from memory of the kernel numbering,
// confirmed by the 3D probe).
func TestNodeSegsRecordNodeOffsetsDeepInTheTree(t *testing.T) {
	const a = f2fstest.AddrsPerBlock
	o := f2fstest.Options{NodeSegs: 2}
	al := f2fstest.NewAlloc(o, 101)
	ind2 := int64(923 + 2*a + a*a)   // first logical block under i_nid[3]
	dind := int64(923 + 2*a + 2*a*a) // first logical block under i_nid[4]
	nodes, _ := al.File(o, f2fstest.Inode{NID: 100, Mode: 0o100644, Size: uint64(dind+a*a+1) * bs},
		f2fstest.FileData{Blocks: map[int64][]byte{ind2: pattern(bs, 1), dind: pattern(bs, 2), dind + a*a: pattern(bs, 3)}})
	byNID := map[uint32][]byte{}
	for _, n := range nodes {
		byNID[n.NID] = n.Block
	}
	ofs := func(nid uint32) uint32 {
		b, ok := byNID[nid]
		if !ok {
			t.Fatalf("node %d is not among the %d built nodes", nid, len(nodes))
		}
		_, _, flag, _, _ := footer(b)
		return flag >> 7
	}
	child := func(b []byte, i int) uint32 { return le.Uint32(b[4*i:]) }
	inode := byNID[100]
	n3, n4 := le.Uint32(inode[4052+4*3:]), le.Uint32(inode[4052+4*4:])
	if got := ofs(n3); got != 1022 {
		t.Errorf("i_nid[3] has ofs %d, want 1022", got)
	}
	if got := ofs(child(byNID[n3], 0)); got != 1023 {
		t.Errorf("the first child of i_nid[3] has ofs %d, want 1023", got)
	}
	if got := ofs(n4); got != 2041 {
		t.Errorf("i_nid[4] has ofs %d, want 2041", got)
	}
	ind := child(byNID[n4], 0)
	if got := ofs(ind); got != 2042 {
		t.Errorf("the first child of the double indirect node has ofs %d, want 2042", got)
	}
	ind1 := child(byNID[n4], 1)
	if got := ofs(ind1); got != 3061 {
		t.Errorf("the second child of the double indirect node has ofs %d, want 3061", got)
	}
	if got := ofs(child(byNID[ind1], 0)); got != 3062 {
		t.Errorf("its first direct child has ofs %d, want 3062", got)
	}
	if got := ofs(child(byNID[ind], 0)); got != 2043 {
		t.Errorf("its first direct child has ofs %d, want 2043", got)
	}
}
