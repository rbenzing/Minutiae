// Package ext4test synthesizes small, deterministic ext2/3/4 images for unit
// and hostile-input tests of the ext4 reader. It is an independent encoder of
// the on-disk format (it shares no code with the reader), so a reader/builder
// pair that agrees is evidence the format is understood, not that one side
// mirrors the other. Real mke2fs images live under tools/fixtures.
//
// Build lays out, per block group: a superblock and descriptor table backup
// where sparse_super puts one, the block bitmap, the inode bitmap, the inode
// table and the data area. Later additions (inodes, directories, file data)
// extend this file; Options and File only ever gain fields, so existing
// callers keep working.
package ext4test

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/bits"
)

// File describes one file, directory or symlink to place in the image.
type File struct {
	Path    string // "/a/b.txt"
	Data    []byte
	Mode    uint32 // default 0644 / 0755 for dirs
	Dir     bool
	Symlink string
	Inline  bool // store inline (requires Options.InlineData)
	// Deleted writes the entry and then unlinks it: the dirent is merged into
	// the previous record's rec_len, the inode gets a dtime and zero links, and
	// the extent header is kept.
	Deleted bool
	Times   [4]int64 // atime, ctime, mtime, crtime (unix seconds); 0 with Nsec 0 = unset

	// Inode details. Nsec holds the nanoseconds of Times, in the same order.
	// ExtraIsize is i_extra_isize (default 32; a multiple of 4, at least 4;
	// only for inodes larger than 128 bytes) and decides which extra time
	// fields exist. Xattrs go in the inode, or in a block with XattrBlock.
	UID, GID   uint32
	Nsec       [4]uint32
	Generation uint32
	ExtraIsize int
	Xattrs     []Xattr
	XattrBlock bool
}

// Options selects the features and geometry of the image.
type Options struct {
	BlockSize    int  // 1024, 2048 or 4096 (default 1024)
	Extents      bool // false = block maps (ext2/3 style)
	MetadataCsum bool
	InlineData   bool
	Encrypt      bool // set ENCRYPT feature; files under "/enc" get the ENCRYPT flag and names stored raw as given (test supplies "cipher" bytes)
	Bit64        bool
	Journal      bool // set the has_journal feature bit (no journal inode is created)
	GDTCsum      bool // uninit_bg group descriptor crc16 (ignored with MetadataCsum)
	Groups       int  // >= 1; default 1

	BlocksPerGroup int // default 1024; a multiple of 8, at most 8*BlockSize
	InodesPerGroup int // default 128; a multiple of 8, at least 16, at most 8*BlockSize
	InodeSize      int // default 256; a power of two from 128 to BlockSize

	Label string // at most 16 bytes
	UUID  [16]byte

	// CsumSeed, when non-zero with MetadataCsum, sets the metadata_csum_seed
	// feature and stores this value as s_checksum_seed, so the checksum seed
	// no longer depends on the UUID.
	CsumSeed uint32
}

// Fixed superblock timestamp (2023-11-14), so images are reproducible.
const fixedTime = 1700000000

// Feature bits.
const (
	compatExtAttr    = 0x8
	compatHasJournal = 0x4

	incompatFiletype   = 0x2
	incompatExtents    = 0x40
	incompat64Bit      = 0x80
	incompatInlineData = 0x8000
	incompatCsumSeed   = 0x2000
	incompatEncrypt    = 0x10000

	roSparseSuper  = 0x1
	roLargeFile    = 0x2
	roGDTCsum      = 0x10
	roMetadataCsum = 0x400
)

const (
	firstUserInode = 11 // inodes 1..10 are reserved; lost+found is not created
	descSize32     = 32
	descSize64     = 64
	maxGroups      = 1 << 14
)

type group struct {
	start    int // first block of the group
	hasSuper bool
	bitmapBB int // block bitmap block
	bitmapIB int // inode bitmap block
	itable   int // first inode table block
	data     int // first block of the data area
}

type builder struct {
	o            Options
	bs, isz      int
	descSize     int
	groups       []group
	bpg, ipg     int
	firstData    int
	totalBlocks  int
	gdtBlocks    int
	itableBlocks int
	seed         uint32 // metadata_csum seed: crc32c(~0, uuid), or s_checksum_seed
	used         []bool // per absolute block
	usedInodes   []int  // per group, inodes in use
	img          []byte
}

// Build returns an ext2/3/4 image for the options. It panics on invalid
// options (it is a test helper). Each file gets an inode (see InodeNumber) and
// the root directory inode exists; directory entries and file data are added
// by later tasks, so the inodes carry no block pointers yet.
func Build(o Options, files []File) []byte {
	b := newBuilder(o)
	b.layout()
	b.placeFiles(files)
	b.finish()
	return b.img
}

func newBuilder(o Options) *builder {
	b := &builder{o: o}
	b.bs = orDefault(o.BlockSize, 1024)
	if b.bs != 1024 && b.bs != 2048 && b.bs != 4096 {
		panic(fmt.Sprintf("ext4test: unsupported block size %d", b.bs))
	}
	n := orDefault(o.Groups, 1)
	if n < 1 || n > maxGroups {
		panic(fmt.Sprintf("ext4test: %d groups", n))
	}
	b.bpg = orDefault(o.BlocksPerGroup, 1024)
	b.ipg = orDefault(o.InodesPerGroup, 128)
	b.isz = orDefault(o.InodeSize, 256)
	switch {
	case b.bpg%8 != 0 || b.bpg > 8*b.bs:
		panic(fmt.Sprintf("ext4test: blocks per group %d", b.bpg))
	case b.ipg%8 != 0 || b.ipg < 16 || b.ipg > 8*b.bs:
		panic(fmt.Sprintf("ext4test: inodes per group %d", b.ipg))
	case b.isz < 128 || b.isz > b.bs || bits.OnesCount(uint(b.isz)) != 1:
		panic(fmt.Sprintf("ext4test: inode size %d", b.isz))
	case len(o.Label) > 16:
		panic("ext4test: label longer than 16 bytes")
	}
	b.descSize = descSize32
	if o.Bit64 {
		b.descSize = descSize64
	}
	if b.bs == 1024 {
		b.firstData = 1
	}
	b.totalBlocks = b.firstData + n*b.bpg
	b.gdtBlocks = (n*b.descSize + b.bs - 1) / b.bs
	b.itableBlocks = (b.ipg*b.isz + b.bs - 1) / b.bs
	b.seed = rawCRC32C(0xFFFFFFFF, o.UUID[:])
	if o.MetadataCsum && o.CsumSeed != 0 {
		b.seed = o.CsumSeed
	}
	b.groups = make([]group, n)
	b.used = make([]bool, b.totalBlocks)
	b.usedInodes = make([]int, n)
	b.img = make([]byte, b.totalBlocks*b.bs)
	return b
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// hasSuper follows sparse_super: group 0, 1 and powers of 3, 5 and 7.
func hasSuper(g int) bool {
	if g <= 1 {
		return true
	}
	for _, p := range []int{3, 5, 7} {
		for v := p; v <= g; v *= p {
			if v == g {
				return true
			}
		}
	}
	return false
}

// layout assigns every group its metadata blocks and marks them used.
func (b *builder) layout() {
	for g := range b.groups {
		gr := &b.groups[g]
		gr.start = b.firstData + g*b.bpg
		gr.hasSuper = hasSuper(g)
		pos := gr.start
		if gr.hasSuper {
			pos += 1 + b.gdtBlocks // superblock + descriptor table
		}
		gr.bitmapBB, gr.bitmapIB, gr.itable = pos, pos+1, pos+2
		gr.data = pos + 2 + b.itableBlocks
		if gr.data >= gr.start+b.bpg {
			panic(fmt.Sprintf("ext4test: group %d has no room for data (%d blocks of metadata in %d)", g, gr.data-gr.start, b.bpg))
		}
		for blk := gr.start; blk < gr.data; blk++ {
			b.used[blk] = true
		}
	}
	b.usedInodes[0] = firstUserInode - 1
}

// finish renders the bitmaps, group descriptors and superblocks.
func (b *builder) finish() {
	gdt := make([]byte, len(b.groups)*b.descSize)
	freeBlocks, freeInodes := 0, 0
	for g := range b.groups {
		gr := &b.groups[g]
		bbm := make([]byte, b.bs)
		for i := range b.bpg {
			if b.used[gr.start+i] {
				bbm[i/8] |= 1 << (i % 8)
			}
		}
		padBitmap(bbm, b.bpg)
		ibm := make([]byte, b.bs)
		for i := range b.usedInodes[g] {
			ibm[i/8] |= 1 << (i % 8)
		}
		padBitmap(ibm, b.ipg)
		copy(b.img[gr.bitmapBB*b.bs:], bbm)
		copy(b.img[gr.bitmapIB*b.bs:], ibm)

		fb := 0
		for i := range b.bpg {
			if !b.used[gr.start+i] {
				fb++
			}
		}
		fi := b.ipg - b.usedInodes[g]
		freeBlocks += fb
		freeInodes += fi

		d := gdt[g*b.descSize : (g+1)*b.descSize]
		le32(d, 0x0, uint32(gr.bitmapBB))
		le32(d, 0x4, uint32(gr.bitmapIB))
		le32(d, 0x8, uint32(gr.itable))
		le16(d, 0xC, uint16(fb))
		le16(d, 0xE, uint16(fi))
		if b.o.MetadataCsum {
			bc := rawCRC32C(b.seed, bbm[:b.bpg/8])
			ic := rawCRC32C(b.seed, ibm[:b.ipg/8])
			le16(d, 0x18, uint16(bc))
			le16(d, 0x1A, uint16(ic))
			if b.descSize >= descSize64 {
				le16(d, 0x38, uint16(bc>>16))
				le16(d, 0x3A, uint16(ic>>16))
			}
		}
		if b.descSize >= descSize64 {
			le16(d, 0x2C, uint16(uint32(fb)>>16))
			le16(d, 0x2E, uint16(uint32(fi)>>16))
		}
		le16(d, 0x1E, b.descChecksum(d, g))
	}
	for g := range b.groups {
		gr := &b.groups[g]
		if !gr.hasSuper {
			continue
		}
		copy(b.img[(gr.start+1)*b.bs:], gdt)
		off := gr.start * b.bs
		if g == 0 {
			off = 1024
		}
		copy(b.img[off:], b.superblock(g, freeBlocks, freeInodes))
	}
}

// padBitmap sets the bits from n to the end of the block, as mke2fs does.
func padBitmap(bm []byte, n int) {
	for i := n; i < len(bm)*8; i++ {
		bm[i/8] |= 1 << (i % 8)
	}
}

// descChecksum returns bg_checksum for descriptor d of group g.
func (b *builder) descChecksum(d []byte, g int) uint16 {
	var num [4]byte
	binary.LittleEndian.PutUint32(num[:], uint32(g))
	switch {
	case b.o.MetadataCsum:
		c := rawCRC32C(b.seed, num[:])
		c = rawCRC32C(c, d[:0x1E])
		c = rawCRC32C(c, []byte{0, 0})
		c = rawCRC32C(c, d[0x20:])
		return uint16(c)
	case b.o.GDTCsum:
		c := crc16(0xFFFF, b.o.UUID[:])
		c = crc16(c, num[:])
		c = crc16(c, d[:0x1E])
		c = crc16(c, d[0x20:])
		return c
	}
	return 0
}

// superblock renders the 1024-byte superblock (the primary for g == 0, or the
// backup that lives in group g).
func (b *builder) superblock(g, freeBlocks, freeInodes int) []byte {
	sb := make([]byte, 1024)
	o := b.o
	log := uint32(bits.TrailingZeros(uint(b.bs)) - 10)
	le32(sb, 0x0, uint32(len(b.groups)*b.ipg))
	le32(sb, 0x4, uint32(b.totalBlocks))
	le32(sb, 0xC, uint32(freeBlocks))
	le32(sb, 0x10, uint32(freeInodes))
	le32(sb, 0x14, uint32(b.firstData))
	le32(sb, 0x18, log)
	le32(sb, 0x1C, log) // log_cluster_size
	le32(sb, 0x20, uint32(b.bpg))
	le32(sb, 0x24, uint32(b.bpg)) // clusters_per_group
	le32(sb, 0x28, uint32(b.ipg))
	le32(sb, 0x30, fixedTime) // s_wtime
	le16(sb, 0x36, 0xFFFF)    // max_mnt_count
	le16(sb, 0x38, 0xEF53)
	le16(sb, 0x3A, 1) // state: clean
	le16(sb, 0x3C, 1) // errors: continue
	le32(sb, 0x4C, 1) // rev_level
	le32(sb, 0x54, firstUserInode)
	le16(sb, 0x58, uint16(b.isz))
	le16(sb, 0x5A, uint16(g))

	compat := uint32(compatExtAttr)
	if o.Journal {
		compat |= compatHasJournal
	}
	incompat := uint32(incompatFiletype)
	if o.Extents {
		incompat |= incompatExtents
	}
	if o.Bit64 {
		incompat |= incompat64Bit
	}
	if o.InlineData {
		incompat |= incompatInlineData
	}
	if o.Encrypt {
		incompat |= incompatEncrypt
	}
	if o.MetadataCsum && o.CsumSeed != 0 {
		incompat |= incompatCsumSeed
		le32(sb, 0x270, o.CsumSeed)
	}
	ro := uint32(roSparseSuper | roLargeFile)
	switch {
	case o.MetadataCsum:
		ro |= roMetadataCsum
	case o.GDTCsum:
		ro |= roGDTCsum
	}
	le32(sb, 0x5C, compat)
	le32(sb, 0x60, incompat)
	le32(sb, 0x64, ro)
	copy(sb[0x68:0x78], o.UUID[:])
	copy(sb[0x78:0x88], o.Label)
	sb[0xFC] = 1 // def_hash_version: half_md4
	if o.Bit64 {
		le16(sb, 0xFE, uint16(b.descSize))
	}
	le32(sb, 0x108, fixedTime) // s_mkfs_time
	if b.isz > 128 {
		le16(sb, 0x15C, 32) // s_min_extra_isize
		le16(sb, 0x15E, 32) // s_want_extra_isize
	}
	if o.MetadataCsum {
		sb[0x175] = 1 // checksum type: crc32c
		le32(sb, 0x3FC, rawCRC32C(0xFFFFFFFF, sb[:0x3FC]))
	}
	return sb
}

func le16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func le32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// rawCRC32C is the kernel's crc32c_le: no pre- or post-inversion.
func rawCRC32C(seed uint32, p []byte) uint32 {
	return ^crc32.Update(^seed, castagnoli, p)
}

// crc16 is the kernel's crc16 (reflected polynomial 0xA001, no final xor).
func crc16(crc uint16, p []byte) uint16 {
	for _, c := range p {
		crc ^= uint16(c)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
