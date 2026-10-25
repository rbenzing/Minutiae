package ext4test

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// FileLoc is where BuildLayout put one file.
type FileLoc struct {
	Path         string
	Inode        uint32
	InodeOffset  int64 // image offset of the inode
	Generation   uint32
	Data         []int // physical data blocks, file order (pieces in order; holes absent)
	Tree         []int // extent leaf blocks (depth 1) or indirect blocks, in allocation order
	DirentBlock  int   // physical block of its dirent record; -1 when the parent is inline
	DirentOffset int   // offset of the record in that block (or in the inline area)
	Deleted      bool
}

// Layout describes an image built by BuildLayout and edits it so that every
// checksum stays valid. The helpers act on the image slice they are given
// (usually the one BuildLayout returned, or a clone of it).
type Layout struct {
	BlockSize, BlocksPerGroup, InodesPerGroup, InodeSize, FirstDataBlock int
	BlockBitmap, InodeBitmap, InodeTable                                 []int // per group
	Files                                                                []FileLoc

	b *builder // options, seed, descriptor size and group geometry
}

// BuildLayout is Build that also reports where everything went. The inline
// directory area of a DirentOffset is the directory's data: the 4-byte parent
// number, then i_block (offset 4 on), then the system.data value (offset 60 on).
func BuildLayout(o Options, files []File) ([]byte, *Layout) {
	b := newBuilder(o)
	b.layout()
	b.placeFiles(files)
	b.finish()

	l := &Layout{
		BlockSize: b.bs, BlocksPerGroup: b.bpg, InodesPerGroup: b.ipg, InodeSize: b.isz,
		FirstDataBlock: b.firstData, b: b,
	}
	for _, g := range b.groups {
		l.BlockBitmap = append(l.BlockBitmap, g.bitmapBB)
		l.InodeBitmap = append(l.InodeBitmap, g.bitmapIB)
		l.InodeTable = append(l.InodeTable, g.itable)
	}
	all := withParents(files)
	for i := range all {
		num := InodeNumber(i)
		loc := FileLoc{
			Path: all[i].Path, Inode: num, InodeOffset: l.inodeOffset(num),
			Generation: all[i].Generation, Deleted: all[i].Deleted,
		}
		if p := b.rec[num]; p != nil {
			loc.Data = append([]int(nil), p.data...)
			loc.Tree = append([]int(nil), p.tree...)
		}
		parent, name := splitPath(all[i].Path)
		parentNum := uint32(rootInode)
		parentInline := false
		for k := range all {
			if all[k].Path == parent {
				parentNum, parentInline = InodeNumber(k), all[k].Inline
			}
		}
		loc.DirentBlock, loc.DirentOffset = l.findDirent(parentNum, parentInline, name, num)
		l.Files = append(l.Files, loc)
	}
	return b.img, l
}

// findDirent locates the record for name in the directory's rendered data,
// including records hidden in the slack of a live one (a merged deleted entry).
func (l *Layout) findDirent(dir uint32, inline bool, name string, ino uint32) (blk, off int) {
	data := l.b.dirBytes[dir]
	if inline {
		for _, r := range [][2]int{{4, min(60, len(data))}, {60, len(data)}} {
			if r[0] >= r[1] {
				continue
			}
			if o, ok := scanRegion(data[r[0]:r[1]], name, ino); ok {
				return -1, r[0] + o
			}
		}
		panic(fmt.Sprintf("ext4test: no dirent for %q in inline directory %d", name, dir))
	}
	phys := l.b.rec[dir].data
	for k := 0; k*l.b.bs < len(data) && k < len(phys); k++ {
		if o, ok := scanRegion(data[k*l.b.bs:(k+1)*l.b.bs], name, ino); ok {
			return phys[k], o
		}
	}
	panic(fmt.Sprintf("ext4test: no dirent for %q in directory %d", name, dir))
}

// scanRegion walks the records of a directory region. After each live record
// it also walks the records hidden in that record's slack (rec_len beyond its
// minimal length), which is where an unlinked entry stays.
func scanRegion(buf []byte, name string, ino uint32) (int, bool) {
	rec := func(pos, end int) (recLen, nameLen int, ok bool) {
		if pos+direntHeader > end {
			return 0, 0, false
		}
		recLen = int(binary.LittleEndian.Uint16(buf[pos+4:]))
		nameLen = int(buf[pos+6])
		return recLen, nameLen, recLen >= direntHeader && pos+recLen <= end && pos+direntHeader+nameLen <= pos+recLen
	}
	match := func(pos, nameLen int) bool {
		got := binary.LittleEndian.Uint32(buf[pos:])
		return bytes.Equal(buf[pos+direntHeader:pos+direntHeader+nameLen], []byte(name)) && (got == ino || got == 0)
	}
	for off := 0; off < len(buf); {
		rl, nl, ok := rec(off, len(buf))
		if !ok {
			return 0, false
		}
		if match(off, nl) {
			return off, true
		}
		end := off + rl
		for pos := off + (direntHeader+nl+3)&^3; pos < end; {
			hl, hn, ok := rec(pos, end)
			if !ok {
				break
			}
			if match(pos, hn) {
				return pos, true
			}
			pos += hl
		}
		off = end
	}
	return 0, false
}

// File returns the location of path. It panics when the path is absent.
func (l *Layout) File(path string) FileLoc {
	for _, f := range l.Files {
		if f.Path == path {
			return f
		}
	}
	panic(fmt.Sprintf("ext4test: no file %q in the layout", path))
}

func (l *Layout) inodeOffset(ino uint32) int64 {
	g := int(ino-1) / l.b.ipg
	return int64(l.b.groups[g].itable)*int64(l.b.bs) + int64(int(ino-1)%l.b.ipg)*int64(l.b.isz)
}

// groupOf returns the group of an absolute block and the block's index in it.
func (l *Layout) groupOf(blk int) (g, idx int) {
	if blk < l.b.firstData || blk >= l.b.totalBlocks {
		panic(fmt.Sprintf("ext4test: block %d is outside the image", blk))
	}
	g = (blk - l.b.firstData) / l.b.bpg
	return g, blk - l.b.groups[g].start
}

func setBit(bm []byte, i int, on bool) {
	if on {
		bm[i/8] |= 1 << (i % 8)
	} else {
		bm[i/8] &^= 1 << (i % 8)
	}
}

// descCopies calls fn for the descriptor of group g in every descriptor table
// copy (the primary and the backups).
func (l *Layout) descCopies(img []byte, g int, fn func(d []byte)) {
	b := l.b
	for _, gr := range b.groups {
		if !gr.hasSuper {
			continue
		}
		off := (gr.start+1)*b.bs + g*b.descSize
		d := img[off : off+b.descSize]
		fn(d)
		le16(d, 0x1E, b.descChecksum(d, g))
	}
}

// PatchDescriptor applies edit to group's descriptor (in every copy of the
// table) and then recomputes the descriptor checksum.
func (l *Layout) PatchDescriptor(img []byte, group int, edit func(d []byte)) {
	if group < 0 || group >= len(l.b.groups) {
		panic(fmt.Sprintf("ext4test: group %d", group))
	}
	l.descCopies(img, group, edit)
}

// putBitmapCsum stores the low (and, with 64-byte descriptors, high) half of a
// bitmap checksum at lo/hi in the descriptor.
func (l *Layout) putBitmapCsum(d []byte, c uint32, lo, hi int) {
	if !l.b.o.MetadataCsum {
		return
	}
	le16(d, lo, uint16(c))
	if l.b.descSize >= descSize64 {
		le16(d, hi, uint16(c>>16))
	}
}

// SetBlockFree sets or clears the block's bit in its group's bitmap and
// refreshes the bitmap checksum and the descriptor checksum. The free counts
// in the descriptors and the superblock are left as they are.
func (l *Layout) SetBlockFree(img []byte, blk int, free bool) {
	g, idx := l.groupOf(blk)
	bm := img[l.b.groups[g].bitmapBB*l.b.bs:][:l.b.bs]
	setBit(bm, idx, !free)
	c := rawCRC32C(l.b.seed, bm[:l.b.bpg/8])
	l.descCopies(img, g, func(d []byte) { l.putBitmapCsum(d, c, 0x18, 0x38) })
}

// SetInodeFree sets or clears the inode's bit in its group's inode bitmap and
// refreshes the inode bitmap checksum (crc32c of the first inodes_per_group/8
// bytes, low 16 bits at 0x1A, high 16 at 0x3A with 64-byte descriptors) and the
// descriptor checksum.
func (l *Layout) SetInodeFree(img []byte, ino uint32, free bool) {
	if ino == 0 || int(ino-1)/l.b.ipg >= len(l.b.groups) {
		panic(fmt.Sprintf("ext4test: inode %d is outside the image", ino))
	}
	g, idx := int(ino-1)/l.b.ipg, int(ino-1)%l.b.ipg
	bm := img[l.b.groups[g].bitmapIB*l.b.bs:][:l.b.bs]
	setBit(bm, idx, !free)
	c := rawCRC32C(l.b.seed, bm[:l.b.ipg/8])
	l.descCopies(img, g, func(d []byte) { l.putBitmapCsum(d, c, 0x1A, 0x3A) })
}

// PatchInode applies edit to the raw inode and then recomputes its checksum
// (with metadata_csum).
func (l *Layout) PatchInode(img []byte, ino uint32, edit func(raw []byte)) {
	off := l.inodeOffset(ino)
	raw := img[off : off+int64(l.b.isz)]
	edit(raw)
	if l.b.o.MetadataCsum {
		extra := 0
		if l.b.isz > inodeHeader {
			extra = int(binary.LittleEndian.Uint16(raw[0x80:]))
		}
		l.b.putInodeChecksum(raw, ino, extra)
	}
}

// PatchSuper applies edit to every superblock copy and then recomputes that
// copy's checksum (with metadata_csum).
func (l *Layout) PatchSuper(img []byte, edit func(sb []byte)) {
	b := l.b
	for g, gr := range b.groups {
		if !gr.hasSuper {
			continue
		}
		off := gr.start * b.bs
		if g == 0 {
			off = 1024
		}
		sb := img[off : off+1024]
		edit(sb)
		if b.o.MetadataCsum {
			le32(sb, 0x3FC, rawCRC32C(0xFFFFFFFF, sb[:0x3FC]))
		}
	}
}

// UnlinkEffects selects what Unlink does to a deleted file's inode and bitmaps.
type UnlinkEffects struct {
	FreeBlocks, FreeInode bool // data + tree blocks; the inode
	ZeroSize              bool // i_size_lo and i_size_high
	ZeroBlocksCount       bool // i_blocks_lo
	ClearExtentRoot       bool // in-inode header: entries 0, depth 0, max 4; the 48 entry bytes zeroed
	ZeroBlockPointers     bool // the 15 i_block pointers zeroed
}

// DebugfsRm is what `debugfs rm` leaves: the map and size intact.
var DebugfsRm = UnlinkEffects{FreeBlocks: true, FreeInode: true}

// Unlink applies e to the file at path, which must have been built with
// Deleted (the dirent is already merged and the inode already has its dtime).
func (l *Layout) Unlink(img []byte, path string, e UnlinkEffects) {
	loc := l.File(path)
	if !loc.Deleted {
		panic(fmt.Sprintf("ext4test: Unlink of %q, which was not built Deleted", path))
	}
	if e.FreeBlocks {
		for _, blk := range loc.Data {
			l.SetBlockFree(img, blk, true)
		}
		for _, blk := range loc.Tree {
			l.SetBlockFree(img, blk, true)
		}
	}
	if e.FreeInode {
		l.SetInodeFree(img, loc.Inode, true)
	}
	l.PatchInode(img, loc.Inode, func(raw []byte) {
		if e.ZeroSize {
			le32(raw, 0x4, 0)
			le32(raw, 0x6C, 0)
		}
		if e.ZeroBlocksCount {
			le32(raw, 0x1C, 0)
		}
		if e.ClearExtentRoot {
			le16(raw, 0x28+2, 0) // eh_entries
			le16(raw, 0x28+4, maxInodeExtents)
			le16(raw, 0x28+6, 0) // eh_depth
			clear(raw[0x28+extentHeaderLen : 0x28+60])
		}
		if e.ZeroBlockPointers {
			clear(raw[0x28 : 0x28+60])
		}
	})
}

// Extent is one extent for WriteLeaf.
type Extent struct {
	Lblk, Len, Phys int
	Uninit          bool
}

// WriteLeaf overwrites block blk with an extent leaf for inode ino
// (generation gen) holding exts, eh_max (bs-12)/12. With metadata_csum the
// tail checksum is correct when goodCsum, and deliberately wrong otherwise.
func (l *Layout) WriteLeaf(img []byte, blk int, ino, gen uint32, exts []Extent, goodCsum bool) {
	buf := img[blk*l.b.bs : (blk+1)*l.b.bs]
	clear(buf)
	in := make([]extent, len(exts))
	for i, e := range exts {
		in[i] = extent{e.Lblk, e.Len, e.Phys, e.Uninit}
	}
	l.b.putLeaf(buf, ino, gen, in)
	if l.b.o.MetadataCsum && !goodCsum {
		tail := extentHeaderLen + (l.b.bs-extentHeaderLen)/extentEntryLen*extentEntryLen
		buf[tail] ^= 0xFF
	}
}

// ShareDirBlock makes dirB's first mapped block also dirA's first block
// (a cross-linked directory block). dirA's old first block stays allocated.
func (l *Layout) ShareDirBlock(img []byte, dirA, dirB string) {
	a, b := l.File(dirA), l.File(dirB)
	if len(b.Data) == 0 || len(a.Data) == 0 {
		panic("ext4test: ShareDirBlock needs two directories with blocks")
	}
	phys := uint32(b.Data[0])
	l.PatchInode(img, a.Inode, func(raw []byte) {
		if binary.LittleEndian.Uint32(raw[0x20:])&flagExtents != 0 {
			le32(raw, 0x28+extentHeaderLen+8, phys) // ee_start_lo of the first extent
			le16(raw, 0x28+extentHeaderLen+6, 0)
		} else {
			le32(raw, 0x28, phys) // direct pointer 0
		}
	})
}
