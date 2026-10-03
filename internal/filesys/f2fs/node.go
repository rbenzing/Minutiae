package f2fs

import "encoding/binary"

// Node block layout: a 4 KiB block whose last 24 bytes are struct node_footer
// { nid le32, ino le32, flag le32, cp_ver le64, next_blkaddr le32 } (kernel
// include/linux/f2fs_fs.h), so the footer starts at 4096 - 24 = 4072.
const (
	nodeFooterOff = blockSize - 24
	footNID       = nodeFooterOff
	footIno       = nodeFooterOff + 4
)

// node reads the node block of nid: its NAT address must lie in the main
// area and the footer's nid must equal nid. Anything else is a
// *filesys.CorruptError, except a free nid, which wraps filesys.ErrNotFound.
func (f *FS) node(nid uint32) ([]byte, error) {
	const st = "f2fs node"
	addr, err := f.natLookup(nid)
	if err != nil {
		return nil, err
	}
	if addr < f.sb.mainAddr || uint64(addr) >= f.sb.mainEnd() {
		return nil, corrupt(st, int64(addr)*blockSize, "node id %d is at block %d, outside the main area %d..%d", nid, addr, f.sb.mainAddr, f.sb.mainEnd())
	}
	b, err := readBlock(f.r, addr)
	if err != nil {
		return nil, corrupt(st, int64(addr)*blockSize, "read of node id %d at block %d failed: %v", nid, addr, err)
	}
	if got := binary.LittleEndian.Uint32(b[footNID:]); got != nid {
		return nil, corrupt(st, int64(addr)*blockSize, "node block %d has footer nid %d, want %d", addr, got, nid)
	}
	return b, nil
}
