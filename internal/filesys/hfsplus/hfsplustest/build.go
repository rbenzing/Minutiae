// Package hfsplustest synthesizes small, deterministic HFS+/HFSX images for
// unit and hostile-input tests of the hfsplus reader. It is an independent
// encoder of the on-disk format (it shares no code with the reader), so a
// reader/builder pair that agrees is evidence
// the format is understood, not that one side mirrors the other. Real
// mkfs.hfsplus images live under tools/fixtures.
//
// Everything is big-endian. Build lays out the volume in allocation blocks:
// the boot area and the volume header (byte 1024), the allocation file (an
// exact bitmap), the extents-overflow file (an empty B-tree), the catalog file
// (a B-tree holding the root folder record and its thread), optionally the
// journal info block and the journal, free space, and the block that holds the
// alternate volume header (the last 1024 bytes of the volume). Options and
// File only ever gain fields, so existing callers keep working.
//
// Layout choices that come from memory of Apple TN1150 and are to be checked
// against real mkfs.hfsplus images: the root folder is not counted in the
// header's folderCount; the allocation file's logical size is a whole number
// of blocks, its padding bits (past totalBlocks) are set; the alternate
// header's block is allocated; a plain HFS+ catalog has keyCompareType 0 (case
// folding) while a case-insensitive HFSX catalog has 0xCF.
package hfsplustest

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// WrapperBase is the byte offset of the embedded volume in an image built with
// Options.Wrapper (HFS allocation blocks start at sector 8, the embedded
// extent at HFS block 4 of 512 bytes). It is not a multiple of 4096.
const WrapperBase = 8*512 + 4*512

// hfsEpoch is the offset of the HFS+ epoch (1904-01-01) from the Unix epoch.
const hfsEpoch = 2082844800

// fixedDate is the creation/modification date of every object the builder
// writes (HFS+ seconds): 2023-11-14 22:13:20 UTC.
const fixedDate = hfsEpoch + 1700000000

// File describes one file or directory to place in the image. Task 1 builds
// only the root folder; later tasks give this type its fields' meaning.
type File struct {
	Path string // "/a/b.txt"
	Data []byte
	Dir  bool
}

// Options selects the features and geometry of the image.
type Options struct {
	HFSX            bool   // "HX" version 5; with CaseSensitive the catalog keyCompareType is 0xBC, else 0xCF
	CaseSensitive   bool   // only meaningful with HFSX
	BlockSize       uint32 // allocation block size, default 4096
	Blocks          uint32 // total allocation blocks, default 256
	NodeSize        uint16 // catalog/attributes node size, default 4096
	ExtentsNodeSize uint16 // extents-overflow node size, default 1024
	Label           string // volume name (the root folder's name)
	Wrapper         bool   // wrap the volume in a classic HFS master directory block (embedded at WrapperBase)

	Journaled      bool // set the journaled attribute and lay out a clean journal
	JournalPending bool // implies Journaled; the journal has an unreplayed transaction
	// JournalLittleEndian writes the journal header in little-endian order, as
	// a journal formatted on an Intel Mac is.
	JournalLittleEndian bool
	NotUnmounted        bool // clear the unmounted attribute bit
	PrimaryBad          bool // corrupt the primary volume header (the alternate must win)
	ImageExtra          int  // extra bytes after the volume end
}

// Layout reports where Build put things, so tests can patch or read them.
// Offsets named Image are from the start of the image; Volume offsets are from
// the start of the volume.
type Layout struct {
	Base        int64 // image offset of the volume (WrapperBase when wrapped)
	BlockSize   uint32
	Blocks      uint32
	VolumeBytes int64
	PrimaryVH   int64 // image offset of the primary volume header
	AltVH       int64 // image offset of the alternate volume header

	AllocBlock, AllocBlocks     uint32 // allocation file
	ExtentsBlock, ExtentsBlocks uint32 // extents-overflow file
	CatalogBlock, CatalogBlocks uint32 // catalog file
	NodeSize, ExtentsNodeSize   uint32

	JournalInfoBlock uint32 // 0 when not journaled
	JournalOffset    int64  // volume offset of the journal (the journal header)
	JournalBytes     int64
}

const (
	vhOffset    = 1024
	vhSize      = 512
	sectorSize  = 512
	jhdrSize    = 512 // journal_header size
	journalBlks = 8   // blocks reserved for the journal buffer
)

type forkSpec struct {
	logical    uint64
	clump      uint32
	totalBlks  uint32
	start, cnt uint32
}

// Build returns an image holding one HFS+/HFSX volume with only the root
// folder. files must be empty until a later task teaches the builder to place
// content; it panics rather than silently dropping a file.
func Build(o Options, files []File) []byte {
	img, _ := BuildLayout(o, files)
	return img
}

// BuildLayout is Build plus the Layout of the result.
func BuildLayout(o Options, files []File) ([]byte, *Layout) {
	if len(files) > 0 {
		panic("hfsplustest: files are not supported yet")
	}
	if o.BlockSize == 0 {
		o.BlockSize = 4096
	}
	if o.Blocks == 0 {
		o.Blocks = 256
	}
	if o.NodeSize == 0 {
		o.NodeSize = 4096
	}
	if o.ExtentsNodeSize == 0 {
		o.ExtentsNodeSize = 1024
	}
	if o.JournalPending {
		o.Journaled = true
	}
	bs, total := int(o.BlockSize), int(o.Blocks)
	ceil := func(n, d int) int { return (n + d - 1) / d }

	// Block layout.
	cur := ceil(vhOffset+vhSize, bs) // boot area and volume header
	allocBlocks := ceil(ceil(total, 8), bs)
	allocBlock := cur
	cur += allocBlocks
	extBytes := ceil(int(o.ExtentsNodeSize), bs) * bs
	extBlocks, extBlock := extBytes/bs, cur
	cur += extBlocks
	catBytes := ceil(2*int(o.NodeSize), bs) * bs
	catBlocks, catBlock := catBytes/bs, cur
	cur += catBlocks
	var jibBlock, jrnlBlock int
	if o.Journaled {
		jibBlock = cur
		jrnlBlock = cur + 1
		cur += 1 + journalBlks
	}
	altBlocks := ceil(1024, bs)
	altStart := total - altBlocks
	if cur > altStart {
		panic(fmt.Sprintf("hfsplustest: %d blocks of %d bytes are too few for the metadata", total, bs))
	}
	used := cur + altBlocks

	vol := make([]byte, total*bs)
	be := binary.BigEndian

	// Allocation file: an exact bitmap, MSB first; padding bits are set.
	bitmap := vol[allocBlock*bs : (allocBlock+allocBlocks)*bs]
	for n := 0; n < len(bitmap)*8; n++ {
		inUse := n >= total || n < cur || n >= altStart
		if inUse {
			bitmap[n/8] |= 0x80 >> (n % 8)
		}
	}

	// Extents-overflow file: a header node and nothing else.
	extNodes := extBytes / int(o.ExtentsNodeSize)
	copy(vol[extBlock*bs:], headerNode(int(o.ExtentsNodeSize), bthdr{
		maxKeyLength: 10, totalNodes: uint32(extNodes), freeNodes: uint32(extNodes - 1),
		clumpSize: uint32(extBytes), attrs: 2, // kBTBigKeysMask
	}, 1))

	// Catalog file: header node, one leaf holding the root folder and its thread.
	catNodes := catBytes / int(o.NodeSize)
	compare := byte(0)
	if o.HFSX {
		compare = 0xCF
		if o.CaseSensitive {
			compare = 0xBC
		}
	}
	cat := vol[catBlock*bs:]
	copy(cat, headerNode(int(o.NodeSize), bthdr{
		depth: 1, root: 1, leafRecords: 2, firstLeaf: 1, lastLeaf: 1,
		maxKeyLength: 516, totalNodes: uint32(catNodes), freeNodes: uint32(catNodes - 2),
		clumpSize: uint32(catBytes), keyCompare: compare, attrs: 2 | 4, // kBTBigKeysMask | kBTVariableIndexKeysMask
	}, 2))
	copy(cat[int(o.NodeSize):], rootLeaf(int(o.NodeSize), o.Label))

	// Journal.
	lay := &Layout{
		BlockSize: o.BlockSize, Blocks: o.Blocks, VolumeBytes: int64(total) * int64(bs),
		AllocBlock: uint32(allocBlock), AllocBlocks: uint32(allocBlocks),
		ExtentsBlock: uint32(extBlock), ExtentsBlocks: uint32(extBlocks),
		CatalogBlock: uint32(catBlock), CatalogBlocks: uint32(catBlocks),
		NodeSize: uint32(o.NodeSize), ExtentsNodeSize: uint32(o.ExtentsNodeSize),
	}
	if o.Journaled {
		jBytes := journalBlks * bs
		lay.JournalInfoBlock = uint32(jibBlock)
		lay.JournalOffset = int64(jrnlBlock) * int64(bs)
		lay.JournalBytes = int64(jBytes)
		jib := vol[jibBlock*bs:]
		be.PutUint32(jib[0:], 1) // kJIJournalInFSMask
		be.PutUint64(jib[36:], uint64(lay.JournalOffset))
		be.PutUint64(jib[44:], uint64(jBytes))
		var order binary.ByteOrder = binary.BigEndian
		if o.JournalLittleEndian {
			order = binary.LittleEndian
		}
		jh := vol[jrnlBlock*bs:]
		order.PutUint32(jh[0:], 0x4A4E4C78) // "JNLx"
		order.PutUint32(jh[4:], 0x12345678) // endian marker, in the writer's order
		start, end := uint64(jhdrSize), uint64(jhdrSize)
		if o.JournalPending {
			end += 4096
		}
		order.PutUint64(jh[8:], start)
		order.PutUint64(jh[16:], end)
		order.PutUint64(jh[24:], uint64(jBytes))
		order.PutUint32(jh[40:], jhdrSize)
	}

	// Volume header, primary and alternate.
	attrs := uint32(0)
	if !o.NotUnmounted {
		attrs |= 1 << 8
	}
	if o.Journaled {
		attrs |= 1 << 13
	}
	sig, ver := uint16(0x482B), uint16(4)
	if o.HFSX {
		sig, ver = 0x4858, 5
	}
	lastMounted := uint32(0x31302E30) // "10.0"
	if o.Journaled {
		lastMounted = 0x4846534A // "HFSJ"
	}
	vh := make([]byte, vhSize)
	be.PutUint16(vh[0:], sig)
	be.PutUint16(vh[2:], ver)
	be.PutUint32(vh[4:], attrs)
	be.PutUint32(vh[8:], lastMounted)
	be.PutUint32(vh[12:], uint32(jibBlock))
	for _, off := range []int{16, 20, 28} { // create, modify, checked
		be.PutUint32(vh[off:], fixedDate)
	}
	be.PutUint32(vh[32:], 0)                     // fileCount
	be.PutUint32(vh[36:], 0)                     // folderCount (the root folder is not counted)
	be.PutUint32(vh[40:], o.BlockSize)           // blockSize
	be.PutUint32(vh[44:], o.Blocks)              // totalBlocks
	be.PutUint32(vh[48:], uint32(total-used))    // freeBlocks
	be.PutUint32(vh[52:], uint32(cur))           // nextAllocation
	be.PutUint32(vh[56:], 65536)                 // rsrcClumpSize
	be.PutUint32(vh[60:], 65536)                 // dataClumpSize
	be.PutUint32(vh[64:], 16)                    // nextCatalogID
	be.PutUint32(vh[68:], 1)                     // writeCount
	be.PutUint64(vh[72:], 1)                     // encodingsBitmap: MacRoman
	be.PutUint64(vh[80+24:], 0x0123456789ABCDEF) // finderInfo words 6 and 7: the volume id
	putFork(vh[112:], forkSpec{uint64(allocBlocks * bs), uint32(allocBlocks * bs), uint32(allocBlocks), uint32(allocBlock), uint32(allocBlocks)})
	putFork(vh[192:], forkSpec{uint64(extBytes), uint32(extBytes), uint32(extBlocks), uint32(extBlock), uint32(extBlocks)})
	putFork(vh[272:], forkSpec{uint64(catBytes), uint32(catBytes), uint32(catBlocks), uint32(catBlock), uint32(catBlocks)})
	// attributes and startup files: empty forks.
	copy(vol[vhOffset:], vh)
	copy(vol[total*bs-1024:], vh)
	if o.PrimaryBad {
		vol[vhOffset], vol[vhOffset+1] = 0, 0 // destroy the signature
	}

	if !o.Wrapper {
		img := make([]byte, len(vol)+o.ImageExtra)
		copy(img, vol)
		lay.Base = 0
		lay.PrimaryVH, lay.AltVH = vhOffset, lay.VolumeBytes-1024
		return img, lay
	}
	return wrap(o, vol, lay)
}

// wrap embeds vol at WrapperBase in a classic HFS volume whose master
// directory block (sector 2) names the embedded extent in 512-byte HFS blocks.
func wrap(o Options, vol []byte, lay *Layout) ([]byte, *Layout) {
	const alBlkSiz = 512
	be := binary.BigEndian
	embedBlocks := len(vol) / alBlkSiz
	imgLen := WrapperBase + len(vol) + 1024 // the last two sectors hold the HFS alternate MDB
	hfsBlocks := (imgLen - 1024 - 8*sectorSize) / alBlkSiz
	if embedBlocks > 0xFFFF || hfsBlocks > 0xFFFF || len(vol)%alBlkSiz != 0 {
		panic("hfsplustest: volume too large for a wrapper with 512-byte HFS blocks")
	}
	img := make([]byte, imgLen+o.ImageExtra)
	copy(img[WrapperBase:], vol)
	mdb := make([]byte, sectorSize)
	be.PutUint16(mdb[0:], 0x4244)             // drSigWord "BD"
	be.PutUint32(mdb[20:], alBlkSiz)          // drAlBlkSiz
	be.PutUint16(mdb[18:], uint16(hfsBlocks)) // drNmAlBlks
	be.PutUint16(mdb[28:], 8)                 // drAlBlSt (sectors)
	mdb[36] = 7                               // drVN: Pascal string
	copy(mdb[37:], "Wrapper")
	be.PutUint16(mdb[124:], sigOf(o))            // drEmbedSigWord
	be.PutUint16(mdb[126:], 4)                   // drEmbedExtent.startBlock
	be.PutUint16(mdb[128:], uint16(embedBlocks)) // drEmbedExtent.blockCount
	copy(img[1024:], mdb)
	copy(img[imgLen-1024:], mdb)
	lay.Base = WrapperBase
	lay.PrimaryVH = WrapperBase + vhOffset
	lay.AltVH = WrapperBase + lay.VolumeBytes - 1024
	return img, lay
}

func sigOf(o Options) uint16 {
	if o.HFSX {
		return 0x4858
	}
	return 0x482B
}

// putFork writes an HFSPlusForkData with one extent.
func putFork(b []byte, f forkSpec) {
	be := binary.BigEndian
	be.PutUint64(b[0:], f.logical)
	be.PutUint32(b[8:], f.clump)
	be.PutUint32(b[12:], f.totalBlks)
	be.PutUint32(b[16:], f.start)
	be.PutUint32(b[20:], f.cnt)
}

// bthdr is a B-tree header record.
type bthdr struct {
	depth                 uint16
	root, leafRecords     uint32
	firstLeaf, lastLeaf   uint32
	maxKeyLength          uint16
	totalNodes, freeNodes uint32
	clumpSize             uint32
	keyCompare            byte
	attrs                 uint32
}

// headerNode builds node 0: the descriptor, the 106-byte header record, the
// 128-byte user data record and the map record, with the first usedNodes
// nodes marked in use.
func headerNode(nodeSize int, h bthdr, usedNodes int) []byte {
	be := binary.BigEndian
	n := make([]byte, nodeSize)
	n[8] = 1 // kind: header node
	be.PutUint16(n[10:], 3)
	rec := n[14:]
	be.PutUint16(rec[0:], h.depth)
	be.PutUint32(rec[2:], h.root)
	be.PutUint32(rec[6:], h.leafRecords)
	be.PutUint32(rec[10:], h.firstLeaf)
	be.PutUint32(rec[14:], h.lastLeaf)
	be.PutUint16(rec[18:], uint16(nodeSize))
	be.PutUint16(rec[20:], h.maxKeyLength)
	be.PutUint32(rec[22:], h.totalNodes)
	be.PutUint32(rec[26:], h.freeNodes)
	be.PutUint32(rec[32:], h.clumpSize)
	rec[36] = 0 // btreeType: kHFSBTreeType
	rec[37] = h.keyCompare
	be.PutUint32(rec[38:], h.attrs)
	mapStart := 14 + 106 + 128
	mapLen := nodeSize - 8 - mapStart
	if int(h.totalNodes) > mapLen*8 {
		panic("hfsplustest: B-tree has more nodes than the header node's map covers")
	}
	for i := 0; i < usedNodes; i++ {
		n[mapStart+i/8] |= 0x80 >> (i % 8)
	}
	// Offsets: header record, user data, map, free space (the map fills the node).
	for i, off := range []int{14, 14 + 106, mapStart, nodeSize - 8} {
		be.PutUint16(n[nodeSize-2*(i+1):], uint16(off))
	}
	return n
}

// utf16be encodes a name as a count of UTF-16 code units and the units.
func utf16be(name string) (units int, b []byte) {
	u := utf16.Encode([]rune(name))
	if len(u) > 255 {
		panic("hfsplustest: name longer than 255 UTF-16 units")
	}
	b = make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(b[2*i:], c)
	}
	return len(u), b
}

// catalogKey is keyLength, parentID, then the HFSUniStr255 name.
func catalogKey(parent uint32, name string) []byte {
	units, nb := utf16be(name)
	k := make([]byte, 2+6+len(nb))
	binary.BigEndian.PutUint16(k[0:], uint16(6+len(nb)))
	binary.BigEndian.PutUint32(k[2:], parent)
	binary.BigEndian.PutUint16(k[6:], uint16(units))
	copy(k[8:], nb)
	return k
}

// rootLeaf builds the catalog's only leaf node: the root folder record keyed
// (1, label) and its thread keyed (2, "").
func rootLeaf(nodeSize int, label string) []byte {
	be := binary.BigEndian
	folder := make([]byte, 88)
	be.PutUint16(folder[0:], 1)                 // recordType: folder
	be.PutUint16(folder[2:], 2)                 // flags: thread exists
	be.PutUint32(folder[4:], 0)                 // valence
	be.PutUint32(folder[8:], 2)                 // folderID
	for _, off := range []int{12, 16, 20, 24} { // create, contentMod, attributeMod, access
		be.PutUint32(folder[off:], fixedDate)
	}
	be.PutUint16(folder[42:], 0o40755) // BSD fileMode: directory rwxr-xr-x
	units, nb := utf16be(label)
	thread := make([]byte, 10+len(nb))
	be.PutUint16(thread[0:], 3) // recordType: folder thread
	be.PutUint32(thread[4:], 1) // parentID
	be.PutUint16(thread[8:], uint16(units))
	copy(thread[10:], nb)

	recs := [][]byte{
		append(catalogKey(1, label), folder...),
		append(catalogKey(2, ""), thread...),
	}
	n := make([]byte, nodeSize)
	n[8] = 0xFF // kind: leaf (-1)
	n[9] = 1    // height
	be.PutUint16(n[10:], uint16(len(recs)))
	off := 14
	for i, r := range recs {
		if len(r)%2 != 0 {
			panic("hfsplustest: odd record length")
		}
		be.PutUint16(n[nodeSize-2*(i+1):], uint16(off))
		copy(n[off:], r)
		off += len(r)
	}
	be.PutUint16(n[nodeSize-2*(len(recs)+1):], uint16(off))
	return n
}
