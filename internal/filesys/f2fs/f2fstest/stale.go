package f2fstest

import (
	"encoding/binary"
	"slices"
)

// What the kernel leaves in an image after an unlink, and the helpers that
// patch it in: stale node blocks (no NAT entry, SIT bit clear), the per-segment
// summaries (SSA), node footers and the checkpoint's current-segment cursors.
// Like the rest of the package it shares no code with the reader.
//
// Layout facts taken from memory of the kernel headers (the 3D probe confirms
// them against a real image):
//   - struct node_footer at byte 4072: nid, ino, flag (u32 each), cp_ver (le64
//     at 4084), next_blkaddr (le32 at 4092);
//   - struct f2fs_summary is 7 bytes {nid le32, version u8, ofs_in_node le16},
//     512 of them, then the 507-byte journal, then the footer {entry_type u8 at
//     4091, check_sum le32}; SUM_TYPE_DATA = 0, SUM_TYPE_NODE = 1;
//   - the checkpoint's cur_node_segno[8] (le32) starts at 36, cur_node_blkoff[8]
//     (le16) at 68.

const (
	footerOff     = 4072
	ssaEntrySize  = 7
	ssaFooterType = 4091
	sumTypeNode   = 1 // SUM_TYPE_NODE
)

// CurSeg is a checkpoint current-segment cursor: the main-area segment number
// and the next block offset in it.
type CurSeg struct {
	Seg uint32
	Off uint16
}

// Footer is a node footer (struct node_footer). Flag carries the offset in the
// node tree in its bits from 3 up (bits 0-2 are COLD, FSYNC and DENT).
type Footer struct {
	NID, Ino, Flag uint32
	CPVer          uint64
	NextBlk        uint32
}

// SetFooter overwrites the footer of the node block at addr. It does not
// reseal an inode's checksum (see ResealInode).
func SetFooter(img []byte, addr uint32, ft Footer) {
	b := img[int(addr)*BlockSize : (int(addr)+1)*BlockSize]
	le := binary.LittleEndian
	le.PutUint32(b[footerOff:], ft.NID)
	le.PutUint32(b[footerOff+4:], ft.Ino)
	le.PutUint32(b[footerOff+8:], ft.Flag)
	le.PutUint64(b[footerOff+12:], ft.CPVer)
	le.PutUint32(b[footerOff+20:], ft.NextBlk)
}

// ResealInode recomputes the checksum of the inode block at addr in place. It
// does nothing on a volume without inode_checksum, or for an inode whose extra
// header does not reach the checksum word (as InodeBlock).
func ResealInode(o Options, img []byte, addr uint32) {
	resealBlock(o, img[int(addr)*BlockSize:(int(addr)+1)*BlockSize])
}

func resealBlock(o Options, b []byte) {
	if !o.InodeChksum || b[3]&ExtraAttr == 0 || binary.LittleEndian.Uint16(b[360:]) < 12 {
		return
	}
	binary.LittleEndian.PutUint32(b[368:], InodeChecksum(o, b))
}

// withStaleNodes appends o.StaleNodes to o.Nodes as Stale nodes (after the
// nodes whose default address depends on their index).
func withStaleNodes(o Options) []Node {
	if len(o.StaleNodes) == 0 {
		return o.Nodes
	}
	nodes := slices.Clone(o.Nodes)
	for _, n := range o.StaleNodes {
		if n.Addr == 0 {
			panic("f2fstest: a StaleNodes entry needs an Addr")
		}
		n.Stale = true
		nodes = append(nodes, n)
	}
	return nodes
}

// sitSlot returns the selected-copy SIT entry of main segment seg.
func sitSlot(img []byte, o Options, l Layout, seg uint32) []byte {
	if seg >= l.MainSegs {
		panic("f2fstest: segment outside the main area")
	}
	i := int(seg / SITPerBlock)
	blk := int(l.SITBlock(i, bitSet(o.SITBitmap, i))) * BlockSize
	off := blk + int(seg%SITPerBlock)*SITEntrySize
	return img[off : off+SITEntrySize]
}

func mainSeg(l Layout, addr uint32) (seg uint32, off int) {
	if addr < l.Main || addr >= l.BlockCount {
		panic("f2fstest: address outside the main area")
	}
	return (addr - l.Main) / BlocksPerSeg, int((addr - l.Main) % BlocksPerSeg)
}

// SetSITBit sets or clears the valid bit of the main-area block at addr in the
// SIT block copy that o.SITBitmap selects, keeping vblocks in step.
func SetSITBit(img []byte, o Options, addr uint32, valid bool) {
	l := Geometry(o)
	seg, i := mainSeg(l, addr)
	e := sitSlot(img, o, l, seg)
	bit := byte(0x80 >> (i % 8))
	cur := &e[2+i/8]
	if (*cur&bit != 0) == valid {
		return
	}
	vb := binary.LittleEndian.Uint16(e)
	if valid {
		*cur |= bit
		vb++
	} else {
		*cur &^= bit
		vb--
	}
	binary.LittleEndian.PutUint16(e, vb)
}

// SetSITType sets the segment type (top 6 bits of vblocks) of main segment
// segno, keeping its valid-block count.
func SetSITType(img []byte, o Options, segno uint32, typ uint16) {
	e := sitSlot(img, o, Geometry(o), segno)
	vb := binary.LittleEndian.Uint16(e)
	binary.LittleEndian.PutUint16(e, vb&0x3ff|typ<<10)
}

// ssaBlock returns the summary block of main segment seg.
func ssaBlock(img []byte, l Layout, seg uint32) []byte {
	if seg >= l.MainSegs {
		panic("f2fstest: segment outside the main area")
	}
	a := int(l.SSA+seg) * BlockSize
	return img[a : a+BlockSize]
}

// SetSSAEntry sets the nid of the summary entry of the main-area block at addr.
func SetSSAEntry(img []byte, o Options, addr uint32, nid uint32) {
	l := Geometry(o)
	seg, i := mainSeg(l, addr)
	binary.LittleEndian.PutUint32(ssaBlock(img, l, seg)[i*ssaEntrySize:], nid)
}

// SetSSAType sets the footer entry_type of the summary block of segment segno.
func SetSSAType(img []byte, o Options, segno uint32, typ uint8) {
	ssaBlock(img, Geometry(o), segno)[ssaFooterType] = typ
}

// writeSSA writes one summary block per main segment when o.SSA is set: for
// every placed block (stale ones too: the kernel leaves their summaries) the
// entry {nid of the node, or of the dnode that maps the data block, and the
// slot in it}. A segment is a node segment (footer SUM_TYPE_NODE) when it lies
// among the last o.NodeSegs. Without NodeSegs the layout is interleaved and every
// footer type stays 0.
func writeSSA(img []byte, o Options, l Layout) {
	if !o.SSA {
		return
	}
	le := binary.LittleEndian
	put := func(addr, nid uint32, ofs uint16) {
		if addr < l.Main || addr >= l.BlockCount {
			return
		}
		seg, i := mainSeg(l, addr)
		b := ssaBlock(img, l, seg)
		le.PutUint32(b[i*ssaEntrySize:], nid)
		le.PutUint16(b[i*ssaEntrySize+5:], ofs)
	}
	for idx, n := range o.Nodes {
		addr := n.Addr
		if addr == 0 {
			addr = l.Main + uint32(idx)
		}
		if n.Block == nil || addr < l.Main || addr >= l.BlockCount {
			continue
		}
		put(addr, n.NID, 0)
	}
	for _, d := range o.Data {
		if d.Addr >= l.Main && d.Addr < l.BlockCount && int(d.Addr+1)*BlockSize <= len(img) {
			put(d.Addr, d.sumNID, d.sumOfs)
		}
	}
	for seg := range l.MainSegs {
		if o.NodeSegs > 0 && seg >= l.MainSegs-uint32(o.NodeSegs) {
			ssaBlock(img, l, seg)[ssaFooterType] = sumTypeNode
		}
	}
}
