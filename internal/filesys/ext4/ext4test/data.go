package ext4test

import (
	"encoding/binary"
	"fmt"
)

const (
	extentMaxInit   = 32768 // longest initialized extent; uninitialized ones are 1 shorter
	extentHeaderLen = 12
	extentEntryLen  = 12
	maxInodeExtents = 4 // entries that fit in i_block next to the header
)

// content returns the pieces of f's data and its i_size. Directories carry no
// data here (their blocks are written with the directory entries).
func (b *builder) content(f *File) ([]Piece, int64) {
	var ps []Piece
	switch {
	case f.Dir:
		return nil, 0
	case f.Symlink != "":
		ps = []Piece{{Data: []byte(f.Symlink)}}
	case len(f.Pieces) > 0:
		if len(f.Data) > 0 {
			panic("ext4test: Data and Pieces are exclusive")
		}
		ps = f.Pieces
	case len(f.Data) > 0:
		ps = []Piece{{Data: f.Data}}
	}
	var end int64
	for _, p := range ps {
		end = max(end, int64(p.Block)*int64(b.bs)+int64(len(p.Data)))
	}
	if f.Inline {
		end = int64(len(f.Data))
	}
	if f.Size != 0 {
		end = f.Size
	}
	return ps, end
}

// extent is one run of consecutive logical blocks mapped to consecutive
// physical blocks.
type extent struct {
	lblk, n, phys int
	uninit        bool
}

// place allocates and fills the data blocks of every piece. It returns one
// entry per piece-block run: (logical block, physical block, count, uninit).
func (b *builder) place(f *File, ps []Piece) []extent {
	var out []extent
	prevEnd := 0
	for _, p := range ps {
		if len(p.Data) == 0 {
			continue
		}
		if p.Block < prevEnd {
			panic(fmt.Sprintf("ext4test: piece at block %d overlaps or precedes the previous piece (ends at %d)", p.Block, prevEnd))
		}
		nblk := (len(p.Data) + b.bs - 1) / b.bs
		prevEnd = p.Block + nblk
		phys := b.alloc(nblk)
		copy(b.img[phys*b.bs:], p.Data)
		if f.Scatter {
			b.alloc(1) // an unused block, so the next piece is not adjacent
		}
		out = append(out, extent{p.Block, nblk, phys, p.Uninit})
	}
	return out
}

// putExtents builds the extent tree of f in iblock (the 60-byte i_block).
func (b *builder) putExtents(iblock []byte, num uint32, f *File, ps []Piece) {
	placed := b.place(f, ps)
	var exts []extent
	for _, e := range placed {
		longest := extentMaxInit
		if e.uninit {
			longest-- // ee_len > 32768 means uninitialized, so 32768 is the init maximum
		}
		for done := 0; done < e.n; done += longest {
			exts = append(exts, extent{e.lblk + done, min(longest, e.n-done), e.phys + done, e.uninit})
		}
	}
	le16(iblock, 0, extentMagic)
	le16(iblock, 4, maxInodeExtents)
	if f.ExtentLeaves == 0 {
		if len(exts) > maxInodeExtents {
			panic(fmt.Sprintf("ext4test: %d extents do not fit in i_block; set ExtentLeaves", len(exts)))
		}
		le16(iblock, 2, uint16(len(exts)))
		for i, e := range exts {
			putExtent(iblock[extentHeaderLen+i*extentEntryLen:], e)
		}
		return
	}
	n := f.ExtentLeaves
	if n > maxInodeExtents || n > len(exts) {
		panic(fmt.Sprintf("ext4test: %d leaves for %d extents", n, len(exts)))
	}
	le16(iblock, 2, uint16(n))
	le16(iblock, 6, 1) // depth
	for i := range n {
		chunk := exts[i*len(exts)/n : (i+1)*len(exts)/n]
		leaf := b.alloc(1)
		b.putLeaf(b.img[leaf*b.bs:(leaf+1)*b.bs], num, f.Generation, chunk)
		idx := iblock[extentHeaderLen+i*extentEntryLen:]
		le32(idx, 0, uint32(chunk[0].lblk))
		le32(idx, 4, uint32(leaf))
		le16(idx, 8, uint16(uint64(leaf)>>32))
	}
}

func putExtent(e []byte, x extent) {
	n := x.n
	if x.uninit {
		n += extentMaxInit
	}
	le32(e, 0, uint32(x.lblk))
	le16(e, 4, uint16(n))
	le16(e, 6, uint16(uint64(x.phys)>>32))
	le32(e, 8, uint32(x.phys))
}

// putLeaf fills an extent leaf block and, with metadata_csum, its tail
// checksum: crc32c over the block up to the tail, seeded with the filesystem
// seed folded with the inode number and generation (kernel
// ext4_extent_block_csum).
func (b *builder) putLeaf(buf []byte, num, gen uint32, exts []extent) {
	maxEntries := (b.bs - extentHeaderLen) / extentEntryLen
	le16(buf, 0, extentMagic)
	le16(buf, 2, uint16(len(exts)))
	le16(buf, 4, uint16(maxEntries))
	for i, e := range exts {
		putExtent(buf[extentHeaderLen+i*extentEntryLen:], e)
	}
	if b.o.MetadataCsum {
		var w [4]byte
		binary.LittleEndian.PutUint32(w[:], num)
		c := rawCRC32C(b.seed, w[:])
		binary.LittleEndian.PutUint32(w[:], gen)
		c = rawCRC32C(c, w[:])
		tail := extentHeaderLen + maxEntries*extentEntryLen
		le32(buf, tail, rawCRC32C(c, buf[:tail]))
	}
}

// putBlockMap builds the ext2/3 block pointers of f's pieces in iblock.
func (b *builder) putBlockMap(iblock []byte, f *File, ps []Piece) {
	for _, p := range ps {
		if p.Uninit {
			panic("ext4test: uninitialized pieces need Options.Extents")
		}
	}
	for _, e := range b.place(f, ps) {
		for k := range e.n {
			b.mapBlock(iblock, e.lblk+k, e.phys+k)
		}
	}
}

// mapBlock records logical block lblk -> phys in the direct, indirect, double
// or triple indirect pointers, allocating indirect blocks on demand.
func (b *builder) mapBlock(iblock []byte, lblk, phys int) {
	p := b.bs / 4
	switch l := lblk; {
	case l < 12:
		le32(iblock, l*4, uint32(phys))
	case l-12 < p:
		b.chain(iblock, 12, []int{l - 12}, phys)
	case l-12-p < p*p:
		l -= 12 + p
		b.chain(iblock, 13, []int{l / p, l % p}, phys)
	case l-12-p-p*p < p*p*p:
		l -= 12 + p + p*p
		b.chain(iblock, 14, []int{l / (p * p), l / p % p, l % p}, phys)
	default:
		panic(fmt.Sprintf("ext4test: logical block %d is beyond a block map", lblk))
	}
}

// chain follows (allocating as needed) the indirect blocks from i_block slot
// through the path of indexes and stores phys in the last one.
func (b *builder) chain(iblock []byte, slot int, path []int, phys int) {
	buf, off := iblock, slot*4
	for _, idx := range path {
		blk := int(binary.LittleEndian.Uint32(buf[off:]))
		if blk == 0 {
			blk = b.alloc(1)
			le32(buf, off, uint32(blk))
		}
		buf, off = b.img[blk*b.bs:(blk+1)*b.bs], idx*4
	}
	le32(buf, off, uint32(phys))
}
