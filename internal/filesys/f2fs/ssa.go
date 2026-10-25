package f2fs

import "encoding/binary"

// The segment summary area (SSA): one summary block per main-area segment,
// laid out as the summary block of nat.go (512 entries of 7 bytes, the
// journal, then the footer). The entry for block i of a node segment is the
// nid of the node stored there. The footer's entry_type is at byte 4091.
//
// From memory of the kernel headers (the 3D probe, Q-facts, confirms):
// SUM_TYPE_DATA = 0 and SUM_TYPE_NODE = 1.
const (
	sumTypeNode  = 1 // SUM_TYPE_NODE, from memory
	ssaTypeOff   = blockSize - sumFooterSize
	ssaEntrySize = 7
)

// ssaBlock reads the summary block of main-area segment seg (the SIT
// numbering). A segment outside the main area is a *filesys.CorruptError.
func (f *FS) ssaBlock(seg uint32) ([]byte, error) {
	const st = "f2fs SSA"
	sb := f.sb
	if seg >= sb.segMain || uint64(seg) >= uint64(sb.segSSA)<<segShift {
		return nil, corrupt(st, -1, "segment %d is outside the main area (%d segments) or the SSA", seg, sb.segMain)
	}
	addr := uint64(sb.ssaAddr) + uint64(seg)
	if addr >= uint64(sb.blocks) {
		return nil, corrupt(st, -1, "the summary block of segment %d (block %d) lies outside the image", seg, addr)
	}
	b, err := readBlock(f.r, uint32(addr)) // addr < blocks <= 2^32
	if err != nil {
		return nil, readError(st, int64(addr)*blockSize, err)
	}
	return b, nil
}

// ssaEntryNID is the nid of summary entry i (0..511) of a summary block; an
// index outside the block gives 0 (no node).
func ssaEntryNID(b []byte, i int) uint32 {
	if i < 0 || (i+1)*ssaEntrySize > sumEntriesSize || len(b) < blockSize {
		return 0
	}
	return binary.LittleEndian.Uint32(b[i*ssaEntrySize:])
}

// ssaType is the footer entry_type of a summary block (sumTypeNode for a node
// segment).
func ssaType(b []byte) uint8 {
	if len(b) < blockSize {
		return 0
	}
	return b[ssaTypeOff]
}
