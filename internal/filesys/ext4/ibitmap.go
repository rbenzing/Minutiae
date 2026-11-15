package ext4

import (
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// bgInodeUninit is the group descriptor flag for a group whose inode bitmap was never written.
const bgInodeUninit = 0x1

// inodeAlloc is the allocation state of an inode according to the inode bitmap.
type inodeAlloc uint8

const (
	allocFree inodeAlloc = iota
	allocUsed
	allocUnknown
)

// inodeUninitHonoured reports whether INODE_UNINIT and bg_itable_unused mean anything: like BLOCK_UNINIT they only
// exist on filesystems with group descriptor checksums.
func (sb *superblock) inodeUninitHonoured() bool { return sb.blockUninitHonoured() }

// inodeState reports whether the inode bitmap marks inode n allocated (3C-R7 step 2). The answer is allocUnknown, with
// the reason, whenever the bitmap cannot be believed: a number outside the filesystem, a descriptor that is unusable or
// fails its checksum, an INODE_UNINIT group or an inode in the never-used tail of its table, a bitmap that cannot be
// read from the image, a bitmap that fails its checksum. An unknown state is never reported as free. The error is a
// read failure of the underlying reader only.
func (f *FS) inodeState(n uint32) (inodeAlloc, string, error) {
	sb := f.sb
	if n < 1 || n > sb.inodesCount {
		return allocUnknown, fmt.Sprintf("inode %d is outside 1..%d", n, sb.inodesCount), nil
	}
	g := uint64(n-1) / uint64(sb.inodesPerGroup)
	idx := uint64(n-1) % uint64(sb.inodesPerGroup)
	if g >= uint64(len(f.groups)) {
		return allocUnknown, fmt.Sprintf("inode %d is in group %d, but only %d groups have descriptors", n, g, len(f.groups)), nil
	}
	bm, reason, err := f.inodeBitmap(int(g))
	if err != nil || reason != "" {
		return allocUnknown, reason, err
	}
	d := &f.groups[g]
	if sb.inodeUninitHonoured() {
		per := uint64(sb.inodesPerGroup)
		if unused := uint64(d.itableUnused); unused > per || idx >= per-unused {
			return allocUnknown, fmt.Sprintf("inode %d lies in the unused tail of group %d's inode table (bg_itable_unused %d), so its bitmap bit was never maintained", n, g, d.itableUnused), nil
		}
	}
	if bm[idx/8]&(1<<(idx%8)) != 0 {
		return allocUsed, "", nil
	}
	return allocFree, "", nil
}

// inodeBitmap reads and verifies the inode bitmap of group g. A non-empty reason means the bitmap cannot be believed
// (the bitmap is then nil); the error is a read failure of the underlying reader only.
func (f *FS) inodeBitmap(g int) (bm []byte, reason string, err error) {
	sb := f.sb
	d := &f.groups[g]
	switch {
	case d.bad:
		return nil, fmt.Sprintf("the descriptor of group %d is unreadable or names locations outside the filesystem", g), nil
	case d.csumBad:
		return nil, fmt.Sprintf("the descriptor checksum of group %d is wrong, so its flags and bitmap location cannot be trusted", g), nil
	}
	if sb.inodeUninitHonoured() && d.flags&bgInodeUninit != 0 {
		return nil, fmt.Sprintf("group %d is INODE_UNINIT: its inode bitmap was never written", g), nil
	}
	nb := (int64(sb.inodesPerGroup) + 7) / 8
	if nb > int64(sb.blockSize) {
		return nil, fmt.Sprintf("the inode bitmap of group %d does not fit one block", g), nil
	}
	off, ok := filesys.MulOK(int64(d.inodeBitmap), int64(sb.blockSize))
	if !ok {
		return nil, fmt.Sprintf("the inode bitmap location of group %d overflows", g), nil
	}
	bm = make([]byte, nb)
	if err := readFull(f.r, bm, off); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Sprintf("the inode bitmap of group %d lies beyond the end of the image", g), nil
		}
		return nil, "", fmt.Errorf("ext4 inode bitmap of group %d: %w", g, err)
	}
	if sb.metadataCsum() && sb.checksumType == csumTypeCRC32C {
		got := rawCRC32C(sb.csumSeed, bm[:sb.inodesPerGroup/8])
		if !d.inodeBitmapCsumHi {
			got &= 0xFFFF
		}
		if got != d.inodeBitmapCsum {
			return nil, fmt.Sprintf("the inode bitmap checksum of group %d does not match (stored %#x, computed %#x)", g, d.inodeBitmapCsum, got), nil
		}
	}
	return bm, "", nil
}
