package f2fstest

import "encoding/binary"

// Node and inode encoding for the builder: the NAT, its journal and inode node
// blocks. Like the rest of the package it shares no code with the reader.

// putNATEntry encodes struct f2fs_nat_entry (packed: version u8, ino le32,
// block_addr le32 = 9 bytes) into b.
func putNATEntry(b []byte, e NATEntry) {
	b[0] = e.Version
	binary.LittleEndian.PutUint32(b[1:], e.Ino)
	binary.LittleEndian.PutUint32(b[5:], e.Addr)
}

// bitSet tests a version bitmap bit, most significant bit first (the kernel
// f2fs_test_bit).
func bitSet(bm []byte, i int) bool {
	return i/8 < len(bm) && bm[i/8]&(0x80>>(i%8)) != 0
}

// writeNodes places o.Nodes: the NAT entry of each into the NAT block copy the
// version bitmap selects, and its block into the main area.
func writeNodes(img []byte, o Options, l Layout) {
	for idx, n := range o.Nodes {
		addr := n.Addr
		if addr == 0 {
			addr = l.Main + uint32(idx)
		}
		if n.Block != nil {
			if len(n.Block) != BlockSize {
				panic("f2fstest: node block must be 4096 bytes")
			}
			if addr >= l.Main && addr < l.BlockCount {
				copy(img[int(addr)*BlockSize:], n.Block)
			}
		}
		if n.NoNAT {
			continue
		}
		if n.NID >= l.NATCapacity() {
			panic("f2fstest: node id beyond the NAT capacity")
		}
		i := int(n.NID / NATPerBlock)
		blk := l.NATBlock(i, bitSet(o.NATBitmap, i))
		putNATEntry(img[int(blk)*BlockSize+int(n.NID%NATPerBlock)*9:], NATEntry{Ino: n.NID, Addr: addr})
	}
}

// Inode i_inline flag bits (struct f2fs_inode.i_inline).
const (
	InlineXattr  = 0x01
	InlineData   = 0x02
	InlineDentry = 0x04
	DataExist    = 0x08
	InlineDots   = 0x10
	ExtraAttr    = 0x20
)

// Inode describes one inode node block (struct f2fs_inode plus the node
// footer). Times are unix seconds.
type Inode struct {
	NID                             uint32 // node id; also the footer's nid and ino
	Mode                            uint16
	Advise, Inline                  uint8 // Inline: the Inline* flags; ExtraAttr is added by Extra
	UID, GID, Links                 uint32
	Size, Blocks                    uint64
	Atime, Ctime, Mtime             uint64
	AtimeNsec, CtimeNsec, MtimeNsec uint32
	Generation, CurDepth, XattrNID  uint32
	Flags, PIno                     uint32
	Name                            string
	DirLevel                        uint8
	Ext                             [3]uint32
	NIDs                            [5]uint32
	Addrs                           []uint32 // data address slots, from i_addr[ExtraIsize/4]

	Extra           bool   // write the extra attribute header (sets ExtraAttr)
	ExtraIsize      uint16 // default 36 (the full header)
	InlineXattrSize uint16 // i_inline_xattr_size, in words
	ProjID          uint32
	Crtime          uint64
	CrtimeNsec      uint32

	// BadChecksum leaves a wrong inode checksum; SkipChecksum leaves it zero.
	BadChecksum, SkipChecksum bool
	// FooterIno / FooterNID override the footer (0 = NID).
	FooterIno, FooterNID uint32
	// RawOverrides are applied last (byte offset -> bytes), before the checksum.
	RawOverrides map[int][]byte
}

// InodeBlock encodes in as a 4096-byte inode node block. When the volume has
// INODE_CHKSUM (o.InodeChksum) and the extra header reaches i_inode_checksum
// the checksum is stored (see InodeChecksum).
func InodeBlock(o Options, in Inode) []byte {
	le := binary.LittleEndian
	b := make([]byte, BlockSize)
	le.PutUint16(b[0:], in.Mode)
	b[2] = in.Advise
	b[3] = in.Inline
	le.PutUint32(b[4:], in.UID)
	le.PutUint32(b[8:], in.GID)
	le.PutUint32(b[12:], in.Links)
	le.PutUint64(b[16:], in.Size)
	le.PutUint64(b[24:], in.Blocks)
	le.PutUint64(b[32:], in.Atime)
	le.PutUint64(b[40:], in.Ctime)
	le.PutUint64(b[48:], in.Mtime)
	le.PutUint32(b[56:], in.AtimeNsec)
	le.PutUint32(b[60:], in.CtimeNsec)
	le.PutUint32(b[64:], in.MtimeNsec)
	le.PutUint32(b[68:], in.Generation)
	le.PutUint32(b[72:], in.CurDepth)
	le.PutUint32(b[76:], in.XattrNID)
	le.PutUint32(b[80:], in.Flags)
	le.PutUint32(b[84:], in.PIno)
	name := in.Name
	if len(name) > 255 {
		name = name[:255]
	}
	le.PutUint32(b[88:], uint32(len(name)))
	copy(b[92:347], name)
	b[347] = in.DirLevel
	for i, v := range in.Ext {
		le.PutUint32(b[348+4*i:], v)
	}

	const iAddr = 360
	extra := 0
	if in.Extra {
		b[3] |= ExtraAttr
		extra = 36
		if in.ExtraIsize != 0 {
			extra = int(in.ExtraIsize)
		}
		// Header fields; only those wholly inside extra bytes exist.
		var hdr [36]byte
		le.PutUint16(hdr[0:], uint16(extra))
		le.PutUint16(hdr[2:], in.InlineXattrSize)
		le.PutUint32(hdr[4:], in.ProjID)
		le.PutUint64(hdr[12:], in.Crtime)
		le.PutUint32(hdr[20:], in.CrtimeNsec)
		copy(b[iAddr:iAddr+min(extra, len(hdr))], hdr[:])
	}
	for i, v := range in.Addrs {
		p := iAddr + extra + 4*i
		if p+4 > 4052 {
			panic("f2fstest: too many inode address slots")
		}
		le.PutUint32(b[p:], v)
	}
	for i, v := range in.NIDs {
		le.PutUint32(b[4052+4*i:], v)
	}
	fn, fi := in.NID, in.NID
	if in.FooterNID != 0 {
		fn = in.FooterNID
	}
	if in.FooterIno != 0 {
		fi = in.FooterIno
	}
	le.PutUint32(b[4072:], fn)
	le.PutUint32(b[4076:], fi)
	for off, v := range in.RawOverrides {
		copy(b[off:], v)
	}
	if o.InodeChksum && in.Extra && extra >= 12 && !in.SkipChecksum {
		c := InodeChecksum(o, b)
		if in.BadChecksum {
			c ^= 0xdeadbeef
		}
		le.PutUint32(b[368:], c)
	}
	return b
}

// InodeChecksum computes the checksum an inode block of this volume carries:
// crc32_le seeded with crc32_le(~0, uuid) chained over the footer ino
// (bytes 4076..4080), i_generation (68..72), the inode up to the checksum word
// at 368, four zero bytes, then the rest of the block from 372.
func InodeChecksum(o Options, b []byte) uint32 {
	seed := rawCRC32(^uint32(0), o.UUID[:])
	c := rawCRC32(seed, b[4076:4080])
	c = rawCRC32(c, b[68:72])
	c = rawCRC32(c, b[:368])
	c = rawCRC32(c, []byte{0, 0, 0, 0})
	return rawCRC32(c, b[372:])
}
