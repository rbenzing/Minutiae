// Package f2fstest synthesizes small, deterministic F2FS images for unit and
// hostile-input tests of the f2fs reader. It is an independent encoder of the
// on-disk format (it shares no code with the reader), so a reader/builder pair
// that agrees is evidence the format is understood, not that one side mirrors
// the other. Real mkfs.f2fs images live under tools/fixtures.
//
// Build lays out segment 0 (the two superblock copies, then padding up to the
// first segment boundary, as mkfs does), the checkpoint area (two packs, one
// per segment), the SIT, NAT and SSA areas and the main area. It uses far fewer
// main-area segments than mkfs would; a minimal image is about 18 MiB, most of
// it untouched zero pages. Later additions (NAT, inodes, directories, file
// data, SIT) extend this file; Options and File only ever gain fields, so
// existing callers keep working.
package f2fstest

import (
	"encoding/binary"
	"hash/crc32"
	"unicode/utf16"
)

// Geometry constants of every F2FS volume this builder (and the reader) knows.
const (
	BlockSize    = 4096
	BlocksPerSeg = 512

	// Reserved inode numbers.
	NodeIno = 1
	MetaIno = 2
	RootIno = 3
)

// File describes one file, directory or symlink to place in the image.
// Files are not laid out yet: Build accepts and ignores them until the inode,
// data and directory tasks of the plan add that (the fields are fixed now so
// callers written against this shape keep working).
type File struct {
	Path       string // "/a/b.txt"
	Data       []byte
	Dir        bool
	Symlink    string
	Inline     bool // store inline
	Deleted    bool // write the entry, then delete its dentry
	Encrypted  bool
	Compressed bool
	Mode       uint32
	Times      [4]int64 // atime, ctime, mtime, crtime (unix seconds)
	UID, GID   uint32
}

// Options configure the image. The zero value is a valid, minimal volume.
type Options struct {
	Segments int // main-area segments (default 16)
	Label    string
	UUID     [16]byte

	// Features written to the superblock. InlineXattr sets the
	// flexible_inline_xattr feature.
	ExtraAttr, InodeChksum, SBChksum, InlineXattr bool
	Encrypt                                       bool

	// LargeNATBitmap sets CP_LARGE_NAT_BITMAP_FLAG: the checkpoint checksum
	// moves to offset 192 (covering the rest of the block too) and the NAT
	// then SIT version bitmaps follow it.
	LargeNATBitmap bool

	// Version is checkpoint_ver of the newer pack (default 2); the other pack
	// carries Version-1.
	Version uint64

	// Checkpoint layout.
	//
	// CPPayload is cp_payload: extra checkpoint blocks after the header that
	// hold bitmaps. Pack2Newer makes pack 2 the newer one. NoPack2 leaves
	// pack 2 unwritten (all zero), as mkfs does. Unclean clears
	// CP_UMOUNT_FLAG. NATBitmap and SITBitmap are the checkpoint's version
	// bitmaps (default all zero = the first copy of every block); they are
	// truncated to the size the geometry demands (64 bytes each).
	CPPayload            int
	Pack2Newer, NoPack2  bool
	Unclean              bool
	NATBitmap, SITBitmap []byte
}

// Layout is the block geometry Build derives from Options (all in blocks).
type Layout struct {
	Seg0, CP, SIT, NAT, SSA, Main               uint32 // first block of each area
	CPSegs, SITSegs, NATSegs, SSASegs, MainSegs uint32
	BlockCount                                  uint32 // block_count: the end of the main area
	CPPayload                                   uint32
	PackBlocks                                  uint32 // cp_pack_total_block_count
	StartSum                                    uint32 // cp_pack_start_sum
}

// Feature and checkpoint-flag bits, as in include/linux/f2fs_fs.h.
const (
	featEncrypt       = 0x1
	featExtraAttr     = 0x8
	featInodeChksum   = 0x20
	featFlexInlineXat = 0x40
	featSBChksum      = 0x800

	cpUmountFlag      = 0x1
	cpLargeNATBitmapF = 0x400

	superMagic = 0xF2F52010
	sbOffset   = 1024
	sbSize     = 3072
)

func (o Options) segments() uint32 {
	if o.Segments <= 0 {
		return 16
	}
	return uint32(o.Segments)
}

func (o Options) version() uint64 {
	if o.Version == 0 {
		return 2
	}
	return o.Version
}

// Geometry returns the layout Build uses for o.
func Geometry(o Options) Layout {
	l := Layout{
		CPSegs: 2, SITSegs: 2, NATSegs: 2, SSASegs: 1, MainSegs: o.segments(),
		Seg0:      BlocksPerSeg, // mkfs aligns segment 0 to a zone boundary
		CPPayload: uint32(max(o.CPPayload, 0)),
	}
	l.SSASegs = (l.MainSegs + BlocksPerSeg - 1) / BlocksPerSeg // one SSA block per main segment
	l.CP = l.Seg0
	l.SIT = l.CP + l.CPSegs*BlocksPerSeg
	l.NAT = l.SIT + l.SITSegs*BlocksPerSeg
	l.SSA = l.NAT + l.NATSegs*BlocksPerSeg
	l.Main = l.SSA + l.SSASegs*BlocksPerSeg
	l.BlockCount = l.Main + l.MainSegs*BlocksPerSeg
	// header + payload + 3 data summaries + 3 node summaries + trailing copy
	l.StartSum = 1 + l.CPPayload
	l.PackBlocks = l.StartSum + 3 + 3 + 1
	return l
}

// Build returns the image.
func Build(o Options, files []File) []byte {
	_ = files // laid out by later tasks
	l := Geometry(o)
	img := make([]byte, int(l.BlockCount)*BlockSize)

	sb := superblock(o, l)
	copy(img[sbOffset:], sb)
	copy(img[BlockSize+sbOffset:], sb)

	v1, v2 := o.version(), o.version()-1
	if o.Pack2Newer {
		v1, v2 = v2, v1
	}
	writePack(img, o, l, l.CP, v1)
	if !o.NoPack2 {
		writePack(img, o, l, l.CP+BlocksPerSeg, v2)
	}
	return img
}

func features(o Options) uint32 {
	var f uint32
	for _, x := range []struct {
		on  bool
		bit uint32
	}{
		{o.Encrypt, featEncrypt},
		{o.ExtraAttr, featExtraAttr},
		{o.InodeChksum, featInodeChksum},
		{o.InlineXattr, featFlexInlineXat},
		{o.SBChksum, featSBChksum},
	} {
		if x.on {
			f |= x.bit
		}
	}
	return f
}

// superblock encodes struct f2fs_super_block (3072 bytes). Offsets are those
// of the kernel header: see the reader's super.go for the derivation.
func superblock(o Options, l Layout) []byte {
	b := make([]byte, sbSize)
	le := binary.LittleEndian
	put32 := func(off int, v uint32) { le.PutUint32(b[off:], v) }
	put32(0, superMagic)
	le.PutUint16(b[4:], 1) // major_ver
	le.PutUint16(b[6:], 0) // minor_ver
	put32(8, 9)            // log_sectorsize
	put32(12, 3)           // log_sectors_per_block
	put32(16, 12)          // log_blocksize
	put32(20, 9)           // log_blocks_per_seg
	put32(24, 1)           // segs_per_sec
	put32(28, 1)           // secs_per_zone
	if o.SBChksum {
		put32(32, sbSize-4) // checksum_offset
	}
	le.PutUint64(b[36:], uint64(l.BlockCount))
	put32(44, l.MainSegs) // section_count
	put32(48, l.CPSegs+l.SITSegs+l.NATSegs+l.SSASegs+l.MainSegs)
	put32(52, l.CPSegs)
	put32(56, l.SITSegs)
	put32(60, l.NATSegs)
	put32(64, l.SSASegs)
	put32(68, l.MainSegs)
	put32(72, l.Seg0)
	put32(76, l.CP)
	put32(80, l.SIT)
	put32(84, l.NAT)
	put32(88, l.SSA)
	put32(92, l.Main)
	put32(96, RootIno)
	put32(100, NodeIno)
	put32(104, MetaIno)
	copy(b[108:124], o.UUID[:])
	name := utf16.Encode([]rune(o.Label))
	if len(name) > 511 {
		name = name[:511] // keep the terminating NUL
	}
	for i, c := range name {
		le.PutUint16(b[124+2*i:], c)
	}
	put32(1664, l.CPPayload)
	copy(b[1668:], "f2fstest")
	copy(b[1924:], "f2fstest")
	put32(2180, features(o))
	if o.SBChksum {
		put32(sbSize-4, rawCRC32(superMagic, b[:sbSize-4]))
	}
	return b
}

// writePack writes one checkpoint pack at block base: the header block, the
// payload blocks, three data summary blocks (hot, warm, cold; empty journals),
// three node summary blocks and the trailing copy of the header block.
func writePack(img []byte, o Options, l Layout, base uint32, ver uint64) {
	le := binary.LittleEndian
	region := make([]byte, (1+int(l.CPPayload))*BlockSize)
	put32 := func(off int, v uint32) { le.PutUint32(region[off:], v) }
	sitBytes := int(l.SITSegs/2) * BlocksPerSeg / 8
	natBytes := int(l.NATSegs/2) * BlocksPerSeg / 8

	le.PutUint64(region[0:], ver)
	le.PutUint64(region[8:], uint64(l.MainSegs)*BlocksPerSeg) // user_block_count
	// valid_block_count stays 0: no files yet
	put32(32, l.MainSegs)
	flags := uint32(cpUmountFlag)
	if o.Unclean {
		flags = 0
	}
	if o.LargeNATBitmap {
		flags |= cpLargeNATBitmapF
	}
	put32(132, flags)
	put32(136, l.PackBlocks)
	put32(140, l.StartSum)
	put32(152, RootIno+1) // next_free_nid
	put32(156, uint32(sitBytes))
	put32(160, uint32(natBytes))
	crcOff := BlockSize - 4
	if o.LargeNATBitmap {
		crcOff = 192
	}
	put32(164, uint32(crcOff))

	// Version bitmaps, placed as the kernel's __bitmap_ptr reads them.
	var sitOff, natOff int
	switch {
	case o.LargeNATBitmap:
		natOff = 192 + 4
		sitOff = natOff + natBytes
	case l.CPPayload > 0:
		natOff = 192
		sitOff = BlockSize
	default:
		sitOff = 192
		natOff = sitOff + sitBytes
	}
	copy(region[sitOff:sitOff+sitBytes], o.SITBitmap)
	copy(region[natOff:natOff+natBytes], o.NATBitmap)

	sealHeader(region[:BlockSize])
	copy(img[int(base)*BlockSize:], region)

	// Data summaries: hot (NAT journal), warm, cold (SIT journal); footer
	// entry_type SUM_TYPE_DATA = 0. Node summaries follow, SUM_TYPE_NODE = 1.
	// The journals are empty (n_nats = n_sits = 0).
	for i := range 3 {
		img[(int(base+l.StartSum)+3+i)*BlockSize+BlockSize-5] = 1
	}
	copy(img[int(base+l.PackBlocks-1)*BlockSize:], region[:BlockSize])
}

// sealHeader stores the checkpoint checksum of header block blk (which must
// carry a legal checksum_offset): the raw CRC-32 seeded with F2FS_SUPER_MAGIC
// over the bytes before the checksum word, chained over the rest of the block
// when the word is not the block's last.
func sealHeader(blk []byte) {
	off := int(binary.LittleEndian.Uint32(blk[164:]))
	c := rawCRC32(superMagic, blk[:off])
	if off < BlockSize-4 {
		c = rawCRC32(c, blk[off+4:])
	}
	binary.LittleEndian.PutUint32(blk[off:], c)
}

// SealCheckpoint recomputes the checksum of checkpoint pack 1 or 2 of img
// after a test has edited its header block, and refreshes the pack's trailing
// copy of that block (when cp_pack_total_block_count is still sane), so an
// edited field is judged on its own merits rather than by the checksum.
func SealCheckpoint(img []byte, pack int) {
	le := binary.LittleEndian
	base := int(le.Uint32(img[sbOffset+76:]))
	if pack == 2 {
		base += BlocksPerSeg
	}
	blk := img[base*BlockSize : (base+1)*BlockSize]
	if off := le.Uint32(blk[164:]); off < 192 || off > BlockSize-4 {
		return
	}
	sealHeader(blk)
	if n := int(le.Uint32(blk[136:])); n >= 2 && n <= BlocksPerSeg {
		copy(img[(base+n-1)*BlockSize:], blk)
	}
}

// rawCRC32 is the kernel's crc32_le: the reflected CRC-32 register from seed
// without initial or final inversion.
func rawCRC32(seed uint32, p []byte) uint32 {
	return ^crc32.Update(^seed, crc32.IEEETable, p)
}
