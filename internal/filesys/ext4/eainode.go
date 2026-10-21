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

	hashBad, err = f.eaHashVerdict(parent, x, in, fl)
	if err != nil {
		return nil, false, err
	}
	return fl, hashBad, nil
}

const (
	// maxEAHashBytes bounds the value bytes hashed per filesystem. A hostile image can name thousands of EA
	// inodes of 16 MiB each that share their blocks; the hash work is bounded here, and a value whose hash
	// cannot be afforded is an error (filesys.ErrLimit), never one passed unchecked.
	maxEAHashBytes = 512 << 20
	// maxEAVerdicts bounds the verdicts remembered; beyond it a verdict is computed again, within the byte bound.
	maxEAVerdicts = 1 << 16
)

// eaHashVerdict reports whether the crc32c of the value in EA inode in (seeded with the filesystem seed) differs
// from its i_atime. The verdict depends on the EA inode alone, so it is computed once per EA inode and kept: any
// number of attributes naming one EA inode read its value once (the repeated-read cost of ruling Z66). A
// mismatch is a warning, once, and the value is still returned. The bytes hashed are charged to maxEAHashBytes;
// a value that does not fit is filesys.ErrLimit. An unreadable value is remembered too (Z77).
func (f *FS) eaHashVerdict(parent *inode, x xattr, in *inode, fl *file) (hashBad bool, err error) {
	f.eaMu.Lock()
	defer f.eaMu.Unlock()
	if v, ok := f.eaVerdicts[x.Inum]; ok {
		return v.bad, v.err
	}
	if f.eaHashed.Add(fl.size) > f.eaHashCap {
		return false, fmt.Errorf("ext4: %w: attribute %q of inode %d: the %d-byte limit on the EA-inode value bytes hashed per filesystem is spent, so the value of EA inode %d cannot be verified", filesys.ErrLimit, x.Name, parent.num, f.eaHashCap, x.Inum)
	}
	hashBad, err = f.hashEAValue(parent, x, in, fl)
	// The bytes are charged once, so the outcome is kept whatever it is (Z77): a value that cannot be read must not
	// spend the bound again each time it is listed or opened. Only the limit error above is not kept, because it
	// charges nothing and may succeed once the work is not shared.
	if f.eaVerdicts == nil {
		f.eaVerdicts = make(map[uint32]eaVerdict)
	}
	if len(f.eaVerdicts) < maxEAVerdicts {
		f.eaVerdicts[x.Inum] = eaVerdict{bad: hashBad, err: err}
	}
	return hashBad, err
}

// eaVerdict is the remembered outcome of hashing one EA inode's value: the hash is bad, or the value cannot be read.
type eaVerdict struct {
	bad bool
	err error
}

// hashEAValue streams the value of EA inode in through crc32c (seeded with the filesystem seed) and compares it
// with i_atime. A mismatch is a warning and the value is still returned; an unreadable value is ErrCorrupt.
func (f *FS) hashEAValue(parent *inode, x xattr, in *inode, fl *file) (hashBad bool, err error) {
	const st = "ext4 EA inode"
	buf := make([]byte, eaHashChunk)
	sum := f.sb.csumSeed
	for off := int64(0); off < fl.size; {
		n := int(min(int64(len(buf)), fl.size-off))
		got, rerr := fl.ReadAt(buf[:n], off)
		if got != n && (rerr == nil || errors.Is(rerr, io.EOF)) {
			rerr = io.ErrUnexpectedEOF
		}
		if got != n {
			return false, corrupt(st, in.offset, "attribute %q of inode %d: EA inode %d: the value cannot be read to verify its hash: %v", x.Name, parent.num, x.Inum, rerr)
		}
		sum = rawCRC32C(sum, buf[:n])
		off += int64(n)
	}
	hashBad = sum != in.atimeRaw
	if hashBad {
		f.warn("attribute %q of inode %d: the value in EA inode %d has crc32c %#08x, but its i_atime holds %#08x", x.Name, parent.num, x.Inum, sum, in.atimeRaw)
	}
	return hashBad, nil
}
