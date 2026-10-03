package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Offsets inside a group descriptor.
const (
	gdChecksum      = 0x1E // bg_checksum
	gdSize64        = 64   // descriptors of this size or larger carry the _hi halves
	gdAfterChecksum = 0x20
	// gdBitmapCsumHiEnd is the descriptor size that holds bg_block_bitmap_csum_hi.
	gdBitmapCsumHiEnd = 0x3A
)

// groupDesc is one decoded block group descriptor.
type groupDesc struct {
	blockBitmap, inodeBitmap, inodeTable uint64
	flags                                uint16
	// bitmapCsum is bg_block_bitmap_csum (lo, with hi in the upper half when
	// bitmapCsumHi is set: descriptors of 0x3A bytes or more).
	bitmapCsum   uint32
	bitmapCsumHi bool
	// csumBad is set when the descriptor checksum does not verify: its flags
	// cannot be trusted.
	csumBad bool
	// bad is set when a location lies outside the filesystem or the descriptor
	// could not be read (truncated table); later reads of
	// that group's metadata must treat it as corrupt rather than follow it.
	bad bool
}

// hasSuper reports whether the group holds a superblock (and descriptor table)
// backup, per the sparse_super / sparse_super2 rules.
func (sb *superblock) hasSuper(group uint64) bool {
	switch {
	case group == 0:
		return true
	case sb.hasCompat(compatSparseSuper2):
		return group == uint64(sb.backupBGs[0]) || group == uint64(sb.backupBGs[1])
	case !sb.hasRoCompat(roSparseSuper):
		return true
	case group == 1:
		return true
	}
	for _, p := range []uint64{3, 5, 7} {
		for v := p; v <= group; v *= p {
			if v == group {
				return true
			}
		}
	}
	return false
}

// descBlock returns the block that holds descriptor block i of the table.
// Without META_BG (or for i before first_meta_bg) the table is contiguous after
// the superblock; with META_BG the descriptor block for each metagroup sits at
// the start of that metagroup's first group.
func (sb *superblock) descBlock(i uint64) uint64 {
	if !sb.hasIncompat(incompatMetaBG) || i < uint64(sb.firstMetaBG) {
		return uint64(sb.logicalSuperBlock()) + 1 + i
	}
	perBlock := uint64(sb.blockSize / sb.descSize)
	group := i * perBlock // cannot overflow: i < groups/perBlock
	loc := uint64(sb.firstDataBlock) + group*uint64(sb.blocksPerGroup)
	if sb.hasSuper(group) {
		loc++
	}
	if sb.blockSize == 1024 && i == 0 && sb.firstDataBlock == 0 {
		loc++ // 1 KiB blocks without first_data_block: group 0's table follows block 1
	}
	return loc
}

// descChecksum computes the checksum the kernel stores in bg_checksum for the
// descriptor gd of group n, or false when the filesystem keeps none.
func (sb *superblock) descChecksum(gd []byte, n uint32) (uint16, bool) {
	var num [4]byte
	binary.LittleEndian.PutUint32(num[:], n)
	switch {
	case sb.metadataCsum():
		c := rawCRC32C(sb.csumSeed, num[:])
		c = rawCRC32C(c, gd[:gdChecksum])
		c = rawCRC32C(c, []byte{0, 0})
		c = rawCRC32C(c, gd[gdAfterChecksum:])
		return uint16(c), true
	case sb.gdtCsum():
		c := crc16(0xFFFF, sb.uuid[:])
		c = crc16(c, num[:])
		c = crc16(c, gd[:gdChecksum])
		c = crc16(c, gd[gdAfterChecksum:])
		return c, true
	}
	return 0, false
}

// loadGroups reads and validates the group descriptor table. Unusable
// descriptors are flagged and summarised in the returned warnings; only an
// unreadable table is an error.
func loadGroups(r io.ReaderAt, sb *superblock) ([]groupDesc, []string, error) {
	const st = "ext4 group descriptor table"
	le := binary.LittleEndian
	perBlock := int64(sb.blockSize / sb.descSize)
	groups := make([]groupDesc, 0, sb.groups)
	buf := make([]byte, sb.blockSize)
	var badCsum, badLoc int
	firstBadCsum, firstBadLoc := int64(-1), int64(-1)

	// A descriptor block outside the (clamped) filesystem or past the end of the
	// image ends the readable part of the table: the remaining groups stay in
	// the slice, flagged unreadable, so a truncated image or a head capture
	// still opens.
	for i := int64(0); i*perBlock < sb.groups; i++ {
		loc := sb.descBlock(uint64(i))
		off, ok := filesys.MulOK(int64(loc), int64(sb.blockSize))
		if !ok {
			return nil, nil, corrupt(st, -1, "descriptor block %d offset overflows", i)
		}
		n := min(perBlock, sb.groups-i*perBlock)
		chunk := buf[:n*int64(sb.descSize)]
		if loc >= uint64(sb.blocksCount) {
			break
		}
		if err := readFull(r, chunk, off); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, nil, corrupt(st, off, "read failed: %v", err)
		}
		for k := range n {
			g := i*perBlock + k
			gd := chunk[k*int64(sb.descSize) : (k+1)*int64(sb.descSize)]
			d := groupDesc{
				blockBitmap: uint64(le.Uint32(gd[0x0:])),
				inodeBitmap: uint64(le.Uint32(gd[0x4:])),
				inodeTable:  uint64(le.Uint32(gd[0x8:])),
				flags:       le.Uint16(gd[0x12:]),
				bitmapCsum:  uint32(le.Uint16(gd[0x18:])),
			}
			if sb.descSize >= gdSize64 {
				d.blockBitmap |= uint64(le.Uint32(gd[0x20:])) << 32
				d.inodeBitmap |= uint64(le.Uint32(gd[0x24:])) << 32
				d.inodeTable |= uint64(le.Uint32(gd[0x28:])) << 32
				if sb.descSize >= gdBitmapCsumHiEnd {
					d.bitmapCsum |= uint32(le.Uint16(gd[0x38:])) << 16
					d.bitmapCsumHi = true
				}
			}
			if want, ok := sb.descChecksum(gd, uint32(g)); ok && want != le.Uint16(gd[gdChecksum:]) {
				if badCsum == 0 {
					firstBadCsum = g
				}
				badCsum++
				d.csumBad = true
			}
			if !sb.groupLocated(&d) {
				d.bad = true
				if badLoc == 0 {
					firstBadLoc = g
				}
				badLoc++
			}
			groups = append(groups, d)
		}
	}

	var warns []string
	if readable := int64(len(groups)); readable < sb.groups {
		warns = append(warns, fmt.Sprintf("descriptor table truncated: %d of %d groups readable", readable, sb.groups))
		for int64(len(groups)) < sb.groups {
			groups = append(groups, groupDesc{bad: true})
		}
	}
	if badCsum > 0 {
		warns = append(warns, fmt.Sprintf("group descriptor checksum mismatch in %d of %d groups (first: group %d)", badCsum, sb.groups, firstBadCsum))
	}
	if badLoc > 0 {
		warns = append(warns, fmt.Sprintf("%d of %d groups have bitmaps or an inode table outside the filesystem (first: group %d)", badLoc, sb.groups, firstBadLoc))
	}
	return groups, warns, nil
}

// groupLocated reports whether the group's bitmaps and inode table lie inside
// the filesystem.
func (sb *superblock) groupLocated(d *groupDesc) bool {
	total := uint64(sb.blocksCount)
	first := uint64(sb.firstDataBlock)
	if d.blockBitmap < first || d.blockBitmap >= total || d.inodeBitmap < first || d.inodeBitmap >= total {
		return false
	}
	end := d.inodeTable + uint64(sb.itableBlocks)
	return d.inodeTable >= first && end >= d.inodeTable && end <= total
}
