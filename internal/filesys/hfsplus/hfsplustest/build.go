// Package hfsplustest synthesizes small, deterministic HFS+/HFSX images for
// unit and hostile-input tests of the hfsplus reader. It is an independent
// encoder of the on-disk format (it shares no code with the reader), so a
// reader/builder pair that agrees is evidence
// the format is understood, not that one side mirrors the other. Real
// mkfs.hfsplus images live under tools/fixtures.
//
// Everything is big-endian. Build lays out the volume in allocation blocks:
// the boot area and the volume header (byte 1024), the allocation file (an
// exact bitmap), the extents-overflow file (a B-tree, empty unless a fragmented
// catalog or forged overflow records need it), the catalog file (a B-tree of
// the root folder, the given files and folders, and their threads), optionally
// the attributes file (when a file has attributes), the file forks (Data and
// Rsrc, optionally fragmented, with extents-overflow records past the eighth
// extent), hard links (a private metadata folder with iNode files), optionally
// the journal info block and the journal, free space, and the block that holds
// the alternate volume header (the last 1024 bytes of the volume). Options and
// File only ever gain fields, so existing callers keep working.
//
// Layout choices that were checked against real mkfs.hfsplus images (see
// tools/fixtures): the root folder is not counted in the header's folderCount;
// every folder and file has a thread; the catalog keyCompareType is 0xCF for
// H+ and case-insensitive HFSX, 0xBC for case-sensitive HFSX. Layout choices
// from memory of Apple TN1150: the allocation file's logical size is a whole
// number of blocks; the alternate header's block is allocated. Padding bits (past
// totalBlocks) are CLEAR: real mkfs.hfsplus images leave them clear and fsck
// reports orphaned blocks when they are set.
package hfsplustest

import (
	"encoding/binary"
	"fmt"
	"path"
	"sort"
	"strings"
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

// HFSEpoch is the offset of the HFS+ epoch (1904-01-01) from the Unix epoch, in
// seconds: an HFS+ date is a Unix time plus HFSEpoch.
const HFSEpoch = hfsEpoch

// FixedDate is the HFS+ date (seconds since 1904) of every object that does
// not set Times.
const FixedDate = fixedDate

// Times are raw catalog dates, in HFS+ seconds since 1904-01-01 UTC (0 =
// absent).
type Times struct{ Create, ContentMod, AttrMod, Access, Backup uint32 }

// File describes one file or directory to place in the image. Parents must
// precede their children; CNIDs are assigned from 16 in order (the hard-link
// private folder and its iNode files come after every listed file).
type File struct {
	Path string // "/a/b.txt"
	Data []byte
	Dir  bool

	Mode     uint16 // BSD fileMode with type bits; 0 = the default (0o40755 folder, 0o100644 file)
	ZeroMode bool   // write fileMode 0 (a volume written by classic Mac OS has no BSD info)
	UID, GID uint32
	Times    *Times // nil = FixedDate for the four dates, no backup date
	// FileType and FileCreator are the Finder 4CCs of a file (4 bytes, or empty).
	FileType, FileCreator string
	// DataLogical is the logical size recorded in the data fork of a file. The
	// fork has no extents until a later task gives files content.
	DataLogical uint64
	// RsrcLogical and RsrcBlocks are recorded in the resource fork of a file
	// (no extents).
	RsrcLogical uint64
	RsrcBlocks  uint32
	// NameUnits, when not nil, is the name stored on disk, as UTF-16 code units
	// (any units: '/', NUL, a lone surrogate, 255 of them). The last component
	// of Path then only has to be unique: it names the object inside the builder.
	NameUnits []uint16
	NoThread  bool // write no thread record for this object (a legacy volume)

	// Rsrc is the content of the resource fork. Data and Rsrc are placed in
	// allocation blocks after the metadata files; a symlink is a file with
	// mode 0o120777 whose Data is the target.
	Rsrc []byte
	// Fragment, when non-zero, splits the data fork into extents of this many
	// blocks with one free block between them; extents past the eighth live in
	// the extents-overflow tree.
	Fragment uint32
	// OwnerFlags is the BSD ownerFlags byte (0x20 = UF_COMPRESSED); Special is
	// the BSD special field (link count of an iNode, inode number of a link).
	OwnerFlags uint8
	Special    uint32
	// Attrs are inline (or, with ForkData, fork-data) records of the attributes tree.
	Attrs []Attr
	// HardLink names a group of files that share one iNode in the private
	// metadata folder: the first member donates its content and metadata to
	// iNode<N>, and every member becomes a link record ('hlnk'/'hfs+').
	HardLink string
	// LinkInode makes this file a link record to inode number N without
	// creating an iNode (a dangling link unless the volume holds iNode<N>).
	LinkInode uint32
}

// Attr is one extended attribute of a file: a record of the attributes tree.
type Attr struct {
	Name  string
	Value []byte
	// ForkData writes a fork-data record (type 0x20) whose logical size is
	// len(Value) and which holds no extents, instead of an inline one.
	ForkData bool
}

// RawRecord is a catalog leaf record placed under the key (Parent, Name) with
// Data as its data bytes, verbatim: a way to plant junk or forged records.
type RawRecord struct {
	Parent uint32
	Name   []uint16
	Data   []byte
}

// Extent is a run of allocation blocks.
type Extent struct{ Start, Count uint32 }

// OverflowRecord is a record written verbatim into the extents-overflow tree,
// so tests can forge overflow structures (zero-block records, repeats,
// overshoots). The extents are zero-padded to eight.
type OverflowRecord struct {
	FileID     uint32
	Resource   bool
	StartBlock uint32
	Extents    []Extent
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

	// CatalogFragment, when non-zero, splits the catalog file into extents of
	// this many blocks with one free block between them; extents past the
	// eighth are described by records of the extents-overflow tree.
	CatalogFragment uint32
	// StaleSlack leaves a fully valid-looking catalog record in the free space
	// of every leaf node, past its record count (bytes of a removed record).
	StaleSlack bool
	// OverflowRecords are added to the extents-overflow tree as given.
	OverflowRecords []OverflowRecord
	// RawRecords are added to the catalog as given (see RawRecord).
	RawRecords []RawRecord
	// IndexKeys selects the catalog's index record layout: 0 variable-length keys
	// (kBTVariableIndexKeysMask set, as on real volumes), 1 fixed maxKeyLength
	// keys with the key length field set to maxKeyLength, 2 fixed keys whose
	// key length field holds the real key length.
	IndexKeys int
	// PrivatePrefix is the code unit repeated four times at the start of the
	// hard-link private folder's name: 0 (the default, as Apple writes it), 0x2400 or 0x200B.
	PrivatePrefix uint16
	// RawFoldOrder orders a case-folding catalog by the plain lower-cased units,
	// without skipping any ignorable one (U+0000 sorts first; a tree the reader's
	// own descent cannot search for such names: only its linear fallback scan finds
	// them). NulIgnorable skips U+0000 like the other ignorable units (an earlier
	// guess). By default U+0000 sorts after every other unit (it folds to 0xFFFF:
	// the reader's assumption, confirmed on a real image).
	RawFoldOrder bool
	NulIgnorable bool
}

func (o Options) foldMode() foldMode {
	switch {
	case o.RawFoldOrder:
		return foldRaw
	case o.NulIgnorable:
		return foldNulIgnored
	}
	return foldNulLast
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
	CatalogBlock, CatalogBlocks uint32 // catalog file (CatalogBlock is its first extent's start)
	NodeSize, ExtentsNodeSize   uint32

	CatalogExtents []Extent // every extent of the catalog file, in order
	// Node counts and tree shape. Levels[0] holds the leaf node numbers, the
	// last level the root.
	CatalogNodes, ExtentsNodes uint32
	CatalogDepth               int
	CatalogRoot                uint32
	CatalogLevels              [][]uint32
	ExtentsDepth               int
	ExtentsRoot                uint32
	ExtentsLevels              [][]uint32
	CNIDs                      map[string]uint32 // path -> catalog node id

	// Attributes file (zero when no file has attributes).
	AttrBlock, AttrBlocks uint32
	AttrNodes             uint32
	AttrLevels            [][]uint32
	// Files maps a path (including the private folder's children) to the
	// extents of its forks, in fork order.
	Files map[string]FileLayout
	// PrivateFolder is the CNID of the hard-link private folder (0 when the
	// volume has no hard links) and Inodes maps a HardLink group to its inode
	// number (the CNID of iNode<N>).
	PrivateFolder uint32
	Inodes        map[string]uint32

	JournalInfoBlock uint32 // 0 when not journaled
	JournalOffset    int64  // volume offset of the journal (the journal header)
	JournalBytes     int64
}

// CatalogOffset maps a byte offset in the catalog file to an image offset,
// following the catalog's extents.
func (l *Layout) CatalogOffset(forkOff int64) int64 {
	return forkOffset(l.CatalogExtents, int64(l.BlockSize), l.Base, forkOff)
}

// FileLayout is where a file's forks were placed.
type FileLayout struct {
	CNID       uint32
	Data, Rsrc []Extent // every extent of the fork, in order
}

// AttrOffset maps a byte offset in the attributes file to an image offset.
func (l *Layout) AttrOffset(forkOff int64) int64 {
	return forkOffset([]Extent{{l.AttrBlock, l.AttrBlocks}}, int64(l.BlockSize), l.Base, forkOff)
}

// ExtentsOffset maps a byte offset in the extents-overflow file to an image offset.
func (l *Layout) ExtentsOffset(forkOff int64) int64 {
	return forkOffset([]Extent{{l.ExtentsBlock, l.ExtentsBlocks}}, int64(l.BlockSize), l.Base, forkOff)
}

func forkOffset(exts []Extent, bs, base, off int64) int64 {
	for _, e := range exts {
		n := int64(e.Count) * bs
		if off < n {
			return base + int64(e.Start)*bs + off
		}
		off -= n
	}
	panic("hfsplustest: fork offset beyond the fork")
}

const (
	vhOffset    = 1024
	vhSize      = 512
	sectorSize  = 512
	jhdrSize    = 512 // journal_header size
	journalBlks = 8   // blocks reserved for the journal buffer
)

// Build returns an image holding one HFS+/HFSX volume with the given files.
func Build(o Options, files []File) []byte {
	img, _ := BuildLayout(o, files)
	return img
}

// extentsRecords converts the catalog's extents past the eighth, and the
// forged records, to extents-overflow records, sorted by key.
func extentsRecords(catExts []Extent, forged []OverflowRecord) []rec {
	type item struct {
		fileID uint32
		fork   byte
		start  uint32
		exts   []Extent
	}
	var items []item
	var covered uint32
	for _, e := range catExts[:min(8, len(catExts))] {
		covered += e.Count
	}
	if len(catExts) > 8 {
		for rest := catExts[8:]; len(rest) > 0; {
			n := min(8, len(rest))
			items = append(items, item{4, 0, covered, rest[:n]})
			for _, e := range rest[:n] {
				covered += e.Count
			}
			rest = rest[n:]
		}
	}
	for _, f := range forged {
		fk := byte(0)
		if f.Resource {
			fk = 0xFF
		}
		items = append(items, item{f.FileID, fk, f.StartBlock, f.Extents})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.fileID != b.fileID {
			return a.fileID < b.fileID
		}
		if a.fork != b.fork {
			return a.fork < b.fork
		}
		return a.start < b.start
	})
	be := binary.BigEndian
	out := make([]rec, len(items))
	for i, it := range items {
		key := make([]byte, 12)
		be.PutUint16(key[0:], 10)
		key[2] = it.fork
		be.PutUint32(key[4:], it.fileID)
		be.PutUint32(key[8:], it.start)
		data := make([]byte, 64)
		for k, e := range it.exts {
			be.PutUint32(data[8*k:], e.Start)
			be.PutUint32(data[8*k+4:], e.Count)
		}
		out[i] = rec{key: key, data: data}
	}
	return out
}

// BuildLayout is Build plus the Layout of the result.
func BuildLayout(o Options, files []File) ([]byte, *Layout) {
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

	compare := byte(0xCF)
	if o.HFSX && o.CaseSensitive {
		compare = 0xBC
	}

	// Trees. The catalog first: its size fixes the number of its extents,
	// which fixes the extents-overflow records.
	files, privCNID, inodes := expandLinks(files, o.PrivatePrefix)
	// Pass 1 places the forks at block 0 only to count extents (the sizes of the
	// trees do not depend on where the forks lie); pass 2, below, places them.
	forks, _ := allocForks(files, bs, 0)
	recs, counts, cnids := catalogRecords(o, files, forks)
	sortCatalog(recs, compare == 0xBC, o.foldMode())
	var stale []byte
	if o.StaleSlack {
		stale = staleRecord()
	}
	catAttrs := uint32(2 | 4) // kBTBigKeysMask | kBTVariableIndexKeysMask
	if o.IndexKeys != 0 {
		catAttrs = 2
	}
	cat := buildTree(treeSpec{
		nodeSize: int(o.NodeSize), blockSize: bs, recs: recs,
		maxKey: 516, keyCompare: compare, attrs: catAttrs,
		fixedIndex: o.IndexKeys, stale: stale,
	})
	catBytes := len(cat.data)
	catBlocks := catBytes / bs
	nFrags := 1
	if o.CatalogFragment > 0 {
		nFrags = ceil(catBlocks, int(o.CatalogFragment))
	}
	var attrTree *builtTree
	if ar := attrRecords(files); len(ar) > 0 {
		attrTree = buildTree(treeSpec{nodeSize: int(o.NodeSize), blockSize: bs, recs: ar, maxKey: 266, attrs: 2 | 4})
	}
	extSpec := func(catExts []Extent, forks []fileFork) *builtTree {
		return buildTree(treeSpec{
			nodeSize: int(o.ExtentsNodeSize), blockSize: bs,
			recs:   extentsRecords(catExts, append(append([]OverflowRecord(nil), o.OverflowRecords...), fileOverflow(forks)...)),
			maxKey: 10, attrs: 2, // kBTBigKeysMask
		})
	}
	extBytes := len(extSpec(make([]Extent, nFrags), forks).data)
	extBlocks := extBytes / bs

	// Block layout.
	inUse := make([]bool, total)
	mark := func(start, n int) {
		for i := start; i < start+n; i++ {
			inUse[i] = true
		}
	}
	cur := ceil(vhOffset+vhSize, bs) // boot area and volume header
	mark(0, cur)
	allocBlocks := ceil(ceil(total, 8), bs)
	allocBlock := cur
	mark(cur, allocBlocks)
	cur += allocBlocks
	extBlock := cur
	mark(cur, extBlocks)
	cur += extBlocks
	var catExts []Extent
	if o.CatalogFragment == 0 {
		catExts = []Extent{{uint32(cur), uint32(catBlocks)}}
		mark(cur, catBlocks)
		cur += catBlocks
	} else {
		for left := catBlocks; left > 0; {
			n := min(left, int(o.CatalogFragment))
			catExts = append(catExts, Extent{uint32(cur), uint32(n)})
			mark(cur, n)
			cur += n + 1 // one free block between fragments
			left -= n
		}
	}
	catBlock := int(catExts[0].Start)
	var attrBlock, attrBlocks int
	if attrTree != nil {
		attrBlock, attrBlocks = cur, len(attrTree.data)/bs
		mark(cur, attrBlocks) // cur is well inside the volume: a layout past the end panics below
		cur += attrBlocks
	}
	// Pass 2: the file forks, after the metadata files.
	forks, forksEnd := allocForks(files, bs, cur)
	if forksEnd > total-ceil(1024, bs) {
		panic(fmt.Sprintf("hfsplustest: %d blocks of %d bytes are too few for the files", total, bs))
	}
	for _, fk := range forks {
		for _, e := range fk.data {
			mark(int(e.Start), int(e.Count))
		}
		for _, e := range fk.rsrc {
			mark(int(e.Start), int(e.Count))
		}
	}
	cur = forksEnd
	var jibBlock, jrnlBlock int
	if o.Journaled {
		jibBlock = cur
		jrnlBlock = cur + 1
		mark(cur, 1+journalBlks)
		cur += 1 + journalBlks
	}
	altBlocks := ceil(1024, bs)
	altStart := total - altBlocks
	if cur > altStart {
		panic(fmt.Sprintf("hfsplustest: %d blocks of %d bytes are too few for the metadata", total, bs))
	}
	mark(altStart, altBlocks)
	used := 0
	for _, u := range inUse {
		if u {
			used++
		}
	}
	ext := extSpec(catExts, forks)
	// The catalog again with the real fork placement (its size cannot change).
	{
		recs2, _, _ := catalogRecords(o, files, forks)
		sortCatalog(recs2, compare == 0xBC, o.foldMode())
		cat = buildTree(treeSpec{
			nodeSize: int(o.NodeSize), blockSize: bs, recs: recs2,
			maxKey: 516, keyCompare: compare, attrs: catAttrs,
			fixedIndex: o.IndexKeys, stale: stale,
		})
		if len(cat.data) != catBytes {
			panic("hfsplustest: the catalog changed size between passes")
		}
	}

	vol := make([]byte, total*bs)
	be := binary.BigEndian

	// Allocation file: an exact bitmap, MSB first; padding bits stay clear (as mkfs.hfsplus leaves them).
	bitmap := vol[allocBlock*bs : (allocBlock+allocBlocks)*bs]
	for n := 0; n < len(bitmap)*8; n++ {
		if n < total && inUse[n] {
			bitmap[n/8] |= 0x80 >> (n % 8)
		}
	}
	copy(vol[extBlock*bs:], ext.data)
	// The catalog, written across its extents.
	{
		data := cat.data
		for _, e := range catExts {
			n := int(e.Count) * bs
			copy(vol[int(e.Start)*bs:], data[:n])
			data = data[n:]
		}
	}

	// Attributes file and file content.
	if attrTree != nil {
		copy(vol[attrBlock*bs:], attrTree.data)
	}
	for i, f := range files {
		put := func(exts []Extent, data []byte) {
			for _, e := range exts {
				n := min(int(e.Count)*bs, len(data))
				copy(vol[int(e.Start)*bs:], data[:n])
				data = data[n:]
			}
		}
		put(forks[i].data, f.Data)
		put(forks[i].rsrc, f.Rsrc)
	}

	// Journal.
	lay := &Layout{
		BlockSize: o.BlockSize, Blocks: o.Blocks, VolumeBytes: int64(total) * int64(bs),
		AllocBlock: uint32(allocBlock), AllocBlocks: uint32(allocBlocks),
		ExtentsBlock: uint32(extBlock), ExtentsBlocks: uint32(extBlocks),
		CatalogBlock: uint32(catBlock), CatalogBlocks: uint32(catBlocks),
		NodeSize: uint32(o.NodeSize), ExtentsNodeSize: uint32(o.ExtentsNodeSize),
		CatalogExtents: catExts,
		CatalogNodes:   uint32(cat.totalNodes), ExtentsNodes: uint32(ext.totalNodes),
		CatalogDepth: cat.depth, CatalogRoot: cat.root, CatalogLevels: cat.levels,
		ExtentsDepth: ext.depth, ExtentsRoot: ext.root, ExtentsLevels: ext.levels,
		CNIDs:         cnids,
		Files:         map[string]FileLayout{},
		PrivateFolder: privCNID, Inodes: inodes,
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
	be.PutUint32(vh[32:], counts.files)          // fileCount
	be.PutUint32(vh[36:], counts.folders)        // folderCount (the root folder is not counted)
	be.PutUint32(vh[40:], o.BlockSize)           // blockSize
	be.PutUint32(vh[44:], o.Blocks)              // totalBlocks
	be.PutUint32(vh[48:], uint32(total-used))    // freeBlocks
	be.PutUint32(vh[52:], uint32(cur))           // nextAllocation
	be.PutUint32(vh[56:], 65536)                 // rsrcClumpSize
	be.PutUint32(vh[60:], 65536)                 // dataClumpSize
	be.PutUint32(vh[64:], counts.nextCNID)       // nextCatalogID
	be.PutUint32(vh[68:], 1)                     // writeCount
	be.PutUint64(vh[72:], 1)                     // encodingsBitmap: MacRoman
	be.PutUint64(vh[80+24:], 0x0123456789ABCDEF) // finderInfo words 6 and 7: the volume id
	putFork(vh[112:], uint64(allocBlocks*bs), uint32(allocBlocks*bs), uint32(allocBlocks), []Extent{{uint32(allocBlock), uint32(allocBlocks)}})
	putFork(vh[192:], uint64(extBytes), uint32(extBytes), uint32(extBlocks), []Extent{{uint32(extBlock), uint32(extBlocks)}})
	putFork(vh[272:], uint64(catBytes), uint32(catBytes), uint32(catBlocks), catExts)
	if attrTree != nil {
		putFork(vh[352:], uint64(len(attrTree.data)), uint32(len(attrTree.data)), uint32(attrBlocks), []Extent{{uint32(attrBlock), uint32(attrBlocks)}})
		lay.AttrBlock, lay.AttrBlocks, lay.AttrNodes, lay.AttrLevels = uint32(attrBlock), uint32(attrBlocks), uint32(attrTree.totalNodes), attrTree.levels
	}
	for i, f := range files {
		lay.Files[f.Path] = FileLayout{CNID: 16 + uint32(i), Data: forks[i].data, Rsrc: forks[i].rsrc}
	}
	// startup file: empty fork.
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

// counters are the header counts the catalog implies.
type counters struct{ files, folders, nextCNID uint32 }

const (
	catRecFolder = 1
	catRecFile   = 2
	catThreadDir = 3
	catThreadFil = 4
)

// catalogRecords builds the catalog's leaf records: the root folder, every file
// and folder with its thread, and the raw records. CNIDs run from 16 in file
// order.
func catalogRecords(o Options, files []File, forks []fileFork) ([]rec, counters, map[string]uint32) {
	cnids := map[string]uint32{"/": 2}
	dirs := map[string]bool{"/": true}
	valence := map[uint32]uint32{}
	subfolders := map[uint32]uint32{}
	type item struct {
		cnid, parent uint32
		name         []uint16
		f            File
	}
	var items []item
	var c counters
	next := uint32(16)
	for _, f := range files {
		p := path.Clean(f.Path)
		if !strings.HasPrefix(p, "/") || p == "/" {
			panic("hfsplustest: bad path " + f.Path)
		}
		parent, ok := cnids[path.Dir(p)]
		if !ok || !dirs[path.Dir(p)] {
			panic("hfsplustest: parent of " + f.Path + " is missing or not a directory")
		}
		cnid := next
		next++
		cnids[p] = cnid
		if f.Dir {
			dirs[p] = true
			c.folders++
			subfolders[parent]++
		} else {
			c.files++
		}
		valence[parent]++
		name := f.NameUnits
		if name == nil {
			name = unitsOf(path.Base(p))
		}
		items = append(items, item{cnid, parent, name, f})
	}
	c.nextCNID = next

	var recs []rec
	add := func(parent uint32, name []uint16, data []byte) {
		recs = append(recs, rec{key: keyOfUnits(parent, name), data: data, parent: parent, name: name})
	}
	// An HFSX volume's folder records carry kHFSHasFolderCountMask and the
	// number of subfolders (checked by fsck on HFSX volumes).
	folder := func(id uint32, f *File) []byte {
		return folderRecord(id, valence[id], subfolders[id], o.HFSX, f)
	}
	rootName := unitsOf(o.Label)
	add(1, rootName, folder(2, nil))
	add(2, nil, threadRecordUnits(catThreadDir, 1, rootName))
	for i := range items {
		it := &items[i]
		if it.f.Dir {
			add(it.parent, it.name, folder(it.cnid, &it.f))
			if !it.f.NoThread {
				add(it.cnid, nil, threadRecordUnits(catThreadDir, it.parent, it.name))
			}
		} else {
			add(it.parent, it.name, fileRecord(it.cnid, &it.f, &forks[it.cnid-16]))
			if !it.f.NoThread {
				add(it.cnid, nil, threadRecordUnits(catThreadFil, it.parent, it.name))
			}
		}
	}
	for _, r := range o.RawRecords {
		add(r.Parent, r.Name, r.Data)
	}
	return recs, c, cnids
}

// setCommon writes the fields folder and file records share: the dates and the
// BSD info (owner, group, fileMode).
func setCommon(b []byte, f *File, defMode uint16) {
	be := binary.BigEndian
	t := Times{fixedDate, fixedDate, fixedDate, fixedDate, 0}
	mode := defMode
	if f != nil {
		if f.Times != nil {
			t = *f.Times
		}
		if f.Mode != 0 {
			mode = f.Mode
		}
		if f.ZeroMode {
			mode = 0
		}
		be.PutUint32(b[32:], f.UID)
		be.PutUint32(b[36:], f.GID)
		b[41] = f.OwnerFlags
		be.PutUint32(b[44:], f.Special)
	}
	be.PutUint32(b[12:], t.Create)
	be.PutUint32(b[16:], t.ContentMod)
	be.PutUint32(b[20:], t.AttrMod)
	be.PutUint32(b[24:], t.Access)
	be.PutUint32(b[28:], t.Backup)
	be.PutUint16(b[42:], mode) // BSD fileMode
}

// folderRecord is an 88-byte HFSPlusCatalogFolder. f is nil for the root.
func folderRecord(id, valence, subfolders uint32, hfsx bool, f *File) []byte {
	be := binary.BigEndian
	b := make([]byte, 88)
	be.PutUint16(b[0:], catRecFolder)
	flags := uint16(0) // as mkfs.hfsplus writes it (fsck rejects kHFSThreadExistsMask on a folder)
	if hfsx {
		flags |= 0x10 // kHFSHasFolderCountMask: the folderCount field below is valid
		be.PutUint32(b[84:], subfolders)
	}
	be.PutUint16(b[2:], flags)
	be.PutUint32(b[4:], valence)
	be.PutUint32(b[8:], id)
	setCommon(b, f, 0o40755)
	return b
}

// fileRecord is a 248-byte HFSPlusCatalogFile. Its forks hold the extents fk
// placed; a fork without content only records its sizes, when set.
func fileRecord(id uint32, f *File, fk *fileFork) []byte {
	be := binary.BigEndian
	b := make([]byte, 248)
	be.PutUint16(b[0:], catRecFile)
	flags := uint16(2)
	if f.NoThread {
		flags = 0
	}
	if len(f.Attrs) > 0 {
		flags |= 0x04 // kHFSHasAttributesMask: fsck counts the files that have attributes
	}
	be.PutUint16(b[2:], flags)
	be.PutUint32(b[8:], id)
	setCommon(b, f, 0o100644)
	copy(b[48:52], f.FileType)
	copy(b[52:56], f.FileCreator)
	dataLogical := f.DataLogical
	if len(f.Data) > 0 && dataLogical == 0 {
		dataLogical = uint64(len(f.Data))
	}
	if fk.dataBlocks > 0 {
		putFork(b[88:], dataLogical, 0, fk.dataBlocks, fk.data)
	} else {
		be.PutUint64(b[88:], dataLogical)
	}
	rsrcLogical := f.RsrcLogical
	if len(f.Rsrc) > 0 && rsrcLogical == 0 {
		rsrcLogical = uint64(len(f.Rsrc))
	}
	if fk.rsrcBlks > 0 {
		putFork(b[168:], rsrcLogical, 0, fk.rsrcBlks, fk.rsrc)
	} else {
		be.PutUint64(b[168:], rsrcLogical)
		be.PutUint32(b[168+12:], f.RsrcBlocks)
	}
	return b
}

// threadRecordUnits is a folder or file thread: type, reserved, parent, name.
func threadRecordUnits(typ uint16, parent uint32, name []uint16) []byte {
	be := binary.BigEndian
	b := make([]byte, 10+2*len(name))
	be.PutUint16(b[0:], typ)
	be.PutUint32(b[4:], parent)
	be.PutUint16(b[8:], uint16(len(name)))
	for i, u := range name {
		be.PutUint16(b[10+2*i:], u)
	}
	return b
}

// staleRecord is a complete, valid catalog leaf record (a file named
// "stale.txt" in the root folder, CNID 4242) as left behind in node slack.
func staleRecord() []byte {
	return rec{key: keyOfUnits(2, unitsOf("stale.txt")), data: fileRecord(4242, &File{}, &fileFork{})}.bytes()
}

// putFork writes an HFSPlusForkData with up to eight inline extents.
func putFork(b []byte, logical uint64, clump, totalBlks uint32, exts []Extent) {
	be := binary.BigEndian
	be.PutUint64(b[0:], logical)
	be.PutUint32(b[8:], clump)
	be.PutUint32(b[12:], totalBlks)
	for i, e := range exts {
		if i == 8 {
			break
		}
		be.PutUint32(b[16+8*i:], e.Start)
		be.PutUint32(b[20+8*i:], e.Count)
	}
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

// keyOfUnits is a catalog key: keyLength, parentID, then the HFSUniStr255 name.
func keyOfUnits(parent uint32, name []uint16) []byte {
	if len(name) > 255 {
		panic("hfsplustest: name longer than 255 UTF-16 units")
	}
	k := make([]byte, 2+6+2*len(name))
	binary.BigEndian.PutUint16(k[0:], uint16(6+2*len(name)))
	binary.BigEndian.PutUint32(k[2:], parent)
	binary.BigEndian.PutUint16(k[6:], uint16(len(name)))
	for i, u := range name {
		binary.BigEndian.PutUint16(k[8+2*i:], u)
	}
	return k
}
