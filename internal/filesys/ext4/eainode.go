package ext4

import (
	"errors"
	"fmt"
	"io"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// maxEAValue is the largest value an EA inode may hold: the kernel's EXT4_XATTR_SIZE_MAX (1 << 24). A declared
// size above it is corrupt, and no allocation is ever sized from a declared size.
const maxEAValue = 1 << 24

// eaHashChunk is the read size when the value is hashed: the value is streamed, never held.
const eaHashChunk = 64 << 10

// eaInodeValue opens the value of attribute x of inode parent, held in the EA inode x.Inum, as a streaming file of
// the EA inode's real image runs. The EA inode is read WITHOUT its own attributes (rawInode), so an EA inode that
// names itself, or a loop of them, cannot recurse. It must carry the EA-inode flag, be a regular file holding
// its data in blocks, have an i_size equal to the entry's e_value_size (at most maxEAValue), and its runs must
// cover the size with real blocks (a hole or an uninitialized extent is not a value the kernel writes). Any
// mismatch is a *filesys.CorruptError for this value alone. The crc32c of the value, seeded with the filesystem
// seed, is compared with the EA inode's i_atime; a mismatch is a warning and hashBad, and the value is still
// returned.
func (f *FS) eaInodeValue(parent *inode, x xattr) (fl *file, hashBad bool, err error) {
	const st = "ext4 EA inode"
	if x.Inum == 0 {
		return nil, false, fmt.Errorf("%w: attribute %q of inode %d does not live in an EA inode", filesys.ErrUnsupported, x.Name, parent.num)
	}
	in, err := f.rawInode(x.Inum)
	if err != nil {
		return nil, false, fmt.Errorf("attribute %q of inode %d: EA inode %d: %w", x.Name, parent.num, x.Inum, err)
	}
	bad := func(format string, a ...any) error {
		return corrupt(st, in.offset, "attribute %q of inode %d: EA inode %d: "+format, append([]any{x.Name, parent.num, x.Inum}, a...)...)
	}
	switch {
	case in.flags&inodeFlagEAInode == 0:
		return nil, false, bad("the EA-inode flag is not set")
	case in.mode&modeTypeMask != modeReg:
		return nil, false, bad("not a regular file (mode %#o)", in.mode)
	case in.flags&inodeFlagInlineData != 0:
		return nil, false, bad("an EA inode cannot hold inline data")
	case in.size != int64(x.ValueSize):
		return nil, false, bad("i_size %d, but the attribute declares %d bytes", in.size, x.ValueSize)
	case in.size > maxEAValue:
		return nil, false, bad("%d bytes exceed the %d-byte limit", in.size, maxEAValue)
	}
	runs, err := f.runs(in)
	if err != nil {
		return nil, false, fmt.Errorf("attribute %q of inode %d: EA inode %d: %w", x.Name, parent.num, x.Inum, err)
	}
	for _, r := range runs {
		if r.Offset < 0 {
			return nil, false, bad("the value has a hole or an unwritten extent, which the kernel never writes")
		}
	}
	if !in.csumOK {
		f.warn("EA inode %d (attribute %q of inode %d): inode checksum mismatch", x.Inum, x.Name, parent.num)
	}
	fl = &file{size: in.size, runs: runs, r: f.data}
	var pos int64
	for _, r := range runs {
		fl.starts = append(fl.starts, pos)
		pos += r.Length
	}

	buf := make([]byte, eaHashChunk)
	sum := f.sb.csumSeed
	for off := int64(0); off < fl.size; {
		n := int(min(int64(len(buf)), fl.size-off))
		got, rerr := fl.ReadAt(buf[:n], off)
		if got != n && (rerr == nil || errors.Is(rerr, io.EOF)) {
			rerr = io.ErrUnexpectedEOF
		}
		if got != n {
			return nil, false, bad("the value cannot be read to verify its hash: %v", rerr)
		}
		sum = rawCRC32C(sum, buf[:n])
		off += int64(n)
	}
	if sum != in.atimeRaw {
		f.warn("attribute %q of inode %d: the value in EA inode %d has crc32c %#08x, but its i_atime holds %#08x", x.Name, parent.num, x.Inum, sum, in.atimeRaw)
		hashBad = true
	}
	return fl, hashBad, nil
}
