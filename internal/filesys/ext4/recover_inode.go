package ext4

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Directory entry file types (the filetype feature).
const (
	ftRegular = 1
	ftDir     = 2
	ftChrdev  = 3
	ftBlkdev  = 4
	ftFifo    = 5
	ftSock    = 6
	ftSymlink = 7
)

// staleInode is an inode that recovery may believe: free in the bitmap, unlinked, and of the type its directory entry
// recorded.
type staleInode struct {
	in    *inode
	raw   []byte
	rec   *deletedRec
	notes []string // quiet decode notes; they become Candidate.Warnings
}

// modeFiletype maps an inode mode type to the dirent file type the kernel writes for it (0 = no such type).
func modeFiletype(mode uint16) byte {
	switch mode & modeTypeMask {
	case modeReg:
		return ftRegular
	case modeDir:
		return ftDir
	case 0x2000:
		return ftChrdev
	case 0x6000:
		return ftBlkdev
	case 0x1000:
		return ftFifo
	case 0xC000:
		return ftSock
	case modeSymlink:
		return ftSymlink
	}
	return 0
}

// staleInodeFor applies the stale-inode acceptance rules of 3C-R7, in order, to the inode a deleted record names.
// The answers are typed: filesys.ErrUndecided (nothing can be said), filesys.ErrReused (the inode is allocated or
// holds another kind of object), *filesys.CorruptError (structure that cannot be right), filesys.ErrUnsupported (the
// bytes are in the inode table, reported and not recovered), or the I/O error itself.
func (f *FS) staleInodeFor(r *deletedRec) (*staleInode, error) {
	const st = "ext4 stale inode"
	n := r.rec.inode
	if n == 0 {
		return nil, fmt.Errorf("%w: the deleted entry has lost its inode number", filesys.ErrUndecided)
	}
	state, reason, err := f.inodeState(n)
	if err != nil {
		return nil, err
	}
	switch state {
	case allocUsed:
		return nil, fmt.Errorf("%w: inode %d is allocated (live, hard-linked under another name, or reused)", filesys.ErrReused, n)
	case allocUnknown:
		return nil, fmt.Errorf("%w: the allocation state of inode %d is unknown: %s", filesys.ErrUndecided, n, reason)
	}

	off, err := f.inodeOffset(n)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, f.sb.inodeSize)
	if err := readFull(f.r, raw, off); err != nil {
		return nil, fmt.Errorf("ext4 inode %d: %w", n, err)
	}
	in, notes, err := f.decodeInodeQuiet(n, raw)
	if err != nil {
		if ce, ok := err.(*filesys.CorruptError); ok {
			ce.Offset = off + iSizeLo
		}
		return nil, err
	}
	in.offset = off
	if !in.csumOK {
		return nil, corrupt(st, off, "inode %d is free but its checksum does not match", n)
	}
	if in.links != 0 || in.times.Deleted.T.IsZero() {
		return nil, corrupt(st, off, "inode %d is free in the bitmap but has %d links (dtime set: %t)", n, in.links, !in.times.Deleted.T.IsZero())
	}
	if f.sb.hasIncompat(incompatFiletype) && r.rec.ftype != 0 {
		if want := modeFiletype(in.mode); want != r.rec.ftype {
			return nil, fmt.Errorf("%w: the entry recorded file type %d but inode %d is of type %#x", filesys.ErrReused, r.rec.ftype, n, in.mode&modeTypeMask)
		}
	}
	if in.flags&inodeFlagEAInode != 0 {
		return nil, corrupt(st, off, "inode %d is an EA inode, which no directory entry names", n)
	}
	if in.flags&inodeFlagInlineData != 0 {
		return nil, fmt.Errorf("%w: inode %d holds inline data, which is in the inode table (reported, not recovered)", filesys.ErrUnsupported, n)
	}
	if in.mode&modeTypeMask == modeSymlink && in.flags&inodeFlagExtents == 0 && in.size < fastSymlinkMax {
		return nil, fmt.Errorf("%w: inode %d is a fast symlink, its target is in the inode table (reported, not recovered)", filesys.ErrUnsupported, n)
	}
	return &staleInode{in: in, raw: raw, rec: r, notes: notes}, nil
}
