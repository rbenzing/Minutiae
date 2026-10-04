package f2fstest

import (
	"encoding/binary"
	"hash/crc32"
)

// Directory entry encoding for the builder: dentry blocks and the inline
// dentry area of an inode. Like the rest of the package it shares no code with
// the reader; the geometry below is derived from the kernel's
// include/linux/f2fs_fs.h independently.
//
// A dentry block (struct f2fs_dentry_block, 4096 bytes) is
//
//	dentry_bitmap[SIZE_OF_DENTRY_BITMAP = 27]
//	reserved[SIZE_OF_RESERVED = 3]
//	dentry[NR_DENTRY_IN_BLOCK = 214]   (struct f2fs_dir_entry, 11 bytes each)
//	filename[214][F2FS_SLOT_LEN = 8]
//
// with NR_DENTRY_IN_BLOCK = 4096*8 / ((11+8)*8 + 1) = 214 and SIZE_OF_RESERVED =
// 4096 - (11+8)*214 - 27 = 3. The bitmap is little-endian bit order (kernel
// test_bit_le: slot s is bit s%8 of byte s/8, least significant bit first, not
// the most-significant-first order of the version bitmaps). A name of n bytes
// occupies ceil(n/8) consecutive slots, all with their bitmap bit set; the
// struct f2fs_dir_entry is filled only for the first slot.
const (
	// NrDentryInBlock is NR_DENTRY_IN_BLOCK.
	NrDentryInBlock = 214

	dentrySize     = 11 // struct f2fs_dir_entry: hash_code le32, ino le32, name_len le16, file_type u8
	slotLen        = 8  // F2FS_SLOT_LEN
	dentryBitmapSz = 27
	dentryReserved = 3
	blockDentryOff = dentryBitmapSz + dentryReserved
	blockNameOff   = blockDentryOff + NrDentryInBlock*dentrySize
)

// File types of a dentry (enum F2FS_FT_*).
const (
	FTUnknown = 0
	FTReg     = 1
	FTDir     = 2
	FTChr     = 3
	FTBlk     = 4
	FTFifo    = 5
	FTSock    = 6
	FTSymlink = 7
)

// Dentry is one directory entry to encode.
type Dentry struct {
	Name []byte
	Ino  uint32
	Type uint8
	// Hash is hash_code. Zero is replaced by a CRC of the name: the reader
	// does not check the hash, and the builder does not implement the kernel's
	// TEA dentry hash.
	Hash uint32
	// Deleted clears the bitmap bits after the entry is written, as
	// f2fs_delete_entry does (the dentry and its name stay on disk).
	Deleted bool
	// NameLen, when not zero, replaces len(Name) in the dentry (hostile
	// input); the slots the entry occupies are still those of the name.
	NameLen int
}

// SlotsFor is GET_DENTRY_SLOTS: ceil(n / 8).
func SlotsFor(n int) int { return (n + slotLen - 1) / slotLen }

// DentryArea is a region that holds dentries: a dentry block or the inline
// dentry area of an inode, laid out inside B.
type DentryArea struct {
	B                     []byte
	Bitmap, Entries, Name int // offsets into B
	Max                   int // number of slots
}

// BlockArea returns the dentry-block layout over the 4096-byte block b.
func BlockArea(b []byte) DentryArea {
	if len(b) != BlockSize {
		panic("f2fstest: a dentry block is 4096 bytes")
	}
	return DentryArea{B: b, Bitmap: 0, Entries: blockDentryOff, Name: blockNameOff, Max: NrDentryInBlock}
}

// bitmapSet sets or clears bitmap bit i (least significant bit first).
func (a DentryArea) bitmapSet(i int, on bool) {
	if on {
		a.B[a.Bitmap+i/8] |= 1 << (i % 8)
	} else {
		a.B[a.Bitmap+i/8] &^= 1 << (i % 8)
	}
}

// Put writes d at slot. The bitmap bits of every slot of the name are set (and
// cleared again for a Deleted entry). It panics if the entry does not fit.
func (a DentryArea) Put(slot int, d Dentry) {
	n := SlotsFor(len(d.Name))
	if n == 0 {
		n = 1
	}
	if slot < 0 || slot+n > a.Max {
		panic("f2fstest: dentry does not fit the area")
	}
	le := binary.LittleEndian
	h := d.Hash
	if h == 0 {
		h = crc32.ChecksumIEEE(d.Name)
	}
	nl := len(d.Name)
	if d.NameLen != 0 {
		nl = d.NameLen
	}
	p := a.B[a.Entries+slot*dentrySize:]
	le.PutUint32(p[0:], h)
	le.PutUint32(p[4:], d.Ino)
	le.PutUint16(p[8:], uint16(nl))
	p[10] = d.Type
	copy(a.B[a.Name+slot*slotLen:a.Name+(slot+n)*slotLen], d.Name)
	for i := range n {
		a.bitmapSet(slot+i, !d.Deleted)
	}
}

// Pack puts the entries one after another from slot first and returns the
// slot after the last one. It returns false (having written nothing past the
// entries that fit) when an entry does not fit.
func (a DentryArea) Pack(first int, ds []Dentry) (next int, ok bool) {
	slot := first
	for _, d := range ds {
		n := max(SlotsFor(len(d.Name)), 1)
		if slot+n > a.Max {
			return slot, false
		}
		a.Put(slot, d)
		slot += n
	}
	return slot, true
}

// DentryBlocks packs ds into as many dentry blocks as needed, in order. The
// first block starts with "." (ino dir) and ".." (ino parent) when dots is
// set. An entry never spans two blocks.
func DentryBlocks(dots bool, dir, parent uint32, ds []Dentry) [][]byte {
	var blocks [][]byte
	cur := make([]byte, BlockSize)
	area := BlockArea(cur)
	slot := 0
	if dots {
		slot, _ = area.Pack(0, []Dentry{
			{Name: []byte("."), Ino: dir, Type: FTDir},
			{Name: []byte(".."), Ino: parent, Type: FTDir},
		})
	}
	for _, d := range ds {
		n := max(SlotsFor(len(d.Name)), 1)
		if n > NrDentryInBlock {
			panic("f2fstest: name longer than a dentry block")
		}
		if slot+n > NrDentryInBlock {
			blocks = append(blocks, cur)
			cur = make([]byte, BlockSize)
			area = BlockArea(cur)
			slot = 0
		}
		area.Put(slot, d)
		slot += n
	}
	return append(blocks, cur)
}

// Inline dentry geometry. From the kernel's macros (include/linux/f2fs_fs.h):
//
//	MAX_INLINE_DATA   = 4 * (CUR_ADDRS_PER_INODE - inline_xattr_addrs - DEF_INLINE_RESERVED_SIZE(1))
//	NR_INLINE_DENTRY  = MAX_INLINE_DATA * 8 / ((SIZE_OF_DIR_ENTRY + F2FS_SLOT_LEN) * 8 + 1)
//	INLINE_DENTRY_BITMAP_SIZE = ceil(NR_INLINE_DENTRY / 8)
//	INLINE_RESERVED_SIZE = MAX_INLINE_DATA - ((11 + 8) * NR_INLINE_DENTRY + bitmap size)
//
// and make_dentry_ptr_inline: bitmap = &i_addr[extra/4 + 1], dentry = bitmap +
// bitmap size + reserved size, filename = dentry + 11 * NR_INLINE_DENTRY. Without
// an extra header and with the default 50-word inline xattr reservation that is
// MAX_INLINE_DATA 3488, NR_INLINE_DENTRY 182, bitmap 23, reserved 7.

// InlineDentryGeometry returns NR_INLINE_DENTRY, the bitmap size and the
// reserved size for an inode with the given number of data address slots
// (923 - extra/4 - inline xattr words).
func InlineDentryGeometry(addrSlots int) (nr, bitmap, reserved int) {
	maxData := max(addrSlots-1, 0) * 4
	nr = maxData * 8 / ((dentrySize+slotLen)*8 + 1)
	bitmap = (nr + 7) / 8
	reserved = maxData - ((dentrySize+slotLen)*nr + bitmap)
	return nr, bitmap, reserved
}

// inlineArea lays the inline dentry area over b, the inode block, for an inode
// with the given extra header size (bytes) and inline xattr words.
func inlineArea(b []byte, extra, xattrWords int) DentryArea {
	nr, bm, rs := InlineDentryGeometry(923 - extra/4 - xattrWords)
	start := 360 + extra + 4 // &i_addr[extra/4 + DEF_INLINE_RESERVED_SIZE]
	return DentryArea{B: b, Bitmap: start, Entries: start + bm + rs, Name: start + bm + rs + dentrySize*nr, Max: nr}
}
