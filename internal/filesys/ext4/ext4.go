// Package ext4 is a read-only ext2/ext3/ext4 parser. It works on an
// io.ReaderAt, never writes, and never panics on hostile input: every on-disk
// number is validated before it drives a loop or an allocation. It implements
// the filesys interfaces and imports no other Minutiae package.
package ext4

import (
	"errors"
	"io"
	"sync/atomic"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheBlocks = 256 // blocks held by the metadata cache
)

// FS is an opened ext2/3/4 filesystem. After Open it is safe for concurrent
// use: the only mutable state is the warning collector, which is mutex-protected.
type FS struct {
	sb     *superblock
	groups []groupDesc
	r      io.ReaderAt // cached view of the filesystem for metadata, clamped to size
	data   io.ReaderAt // uncached view for file content, clamped to size
	size   int64       // filesystem size in bytes (declared size clamped to the image)

	warnings filesys.Warnings

	// metaRanges are the block ranges [start, end) of group 0's metadata that
	// file data must not overlap: boot block, superblock and descriptor table,
	// then the group's bitmaps and inode table.
	metaRanges [][2]uint64
	// dirRecordCap bounds the entries one directory yields (maxDirRecords).
	dirRecordCap int
	// slackScanCap bounds the slack bytes one directory has searched for deleted
	// entries (maxSlackScan).
	slackScanCap int64
	// slackWork counts the slack work done: one per 4-byte candidate checked and
	// one per byte indexed by plausibleSlack. Tests bound it; nothing reads it.
	slackWork atomic.Int64
}

// readFull reads exactly len(p) bytes at off; a read that returns all the
// bytes together with io.EOF succeeds.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// warn records a problem that does not stop the read (a checksum mismatch, a
// damaged directory block ...) in the shared, deduplicated and capped
// filesys.Warnings collector. It is safe for concurrent use.
func (f *FS) warn(format string, a ...any) { f.warnings.Add(format, a...) }

// Probe reports whether the first 2 KiB of the filesystem hold an ext
// superblock: the 0xEF53 magic and a log_block_size of at most 6.
func Probe(r io.ReaderAt, size int64) bool {
	if size < superOffset+superSize {
		return false
	}
	var b [0x40]byte
	if err := readFull(r, b[:], superOffset); err != nil {
		return false
	}
	return probeSuper(b[:])
}

// Open parses the superblock and group descriptors of the ext2/3/4 filesystem
// in r (size bytes). It fails with filesys.ErrUnsupported for features it
// cannot read (bigalloc, unknown incompatible bits, an external journal) and
// with a *filesys.CorruptError for a structurally invalid superblock or an
// unreadable descriptor table. Checksum mismatches and a truncated image are
// reported through Info().Warnings.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	if size < superOffset+superSize {
		return nil, corrupt("ext4 superblock", superOffset, "image of %d bytes is too small for a superblock", size)
	}
	raw := io.NewSectionReader(r, 0, size)
	buf := make([]byte, superSize)
	if err := readFull(raw, buf, superOffset); err != nil {
		return nil, corrupt("ext4 superblock", superOffset, "read failed: %v", err)
	}
	sb, warns, err := parseSuper(buf, size)
	if err != nil {
		return nil, err
	}
	cached := filesys.NewCachedReader(raw, sb.blockSize, cacheBlocks)
	groups, gw, err := loadGroups(cached, sb)
	if err != nil {
		return nil, err
	}
	f := &FS{
		sb:     sb,
		groups: groups,
		r:      cached,
		data:   raw,
		size:   sb.blocksCount * int64(sb.blockSize), // cannot overflow: <= size

		dirRecordCap: maxDirRecords,
		slackScanCap: maxSlackScan,
	}
	f.metaRanges = metadataRanges(sb, groups)
	for _, w := range append(warns, gw...) {
		f.warn("%s", w)
	}
	return f, nil
}

// Info describes the filesystem. The type is ext2 when there is no journal and
// every feature bit is an ext2 one, ext3 when there is a journal and every bit
// is an ext3 one, and ext4 otherwise (so an unfamiliar feature bit also makes
// it ext4). Warnings is a snapshot: it holds the problems found at Open and
// those met since by directory, extent and checksum reads, without duplicates
// and capped at 1000 entries.
func (f *FS) Info() filesys.Info {
	return filesys.Info{
		Type:      f.sb.fsType(),
		Label:     f.sb.label,
		UUID:      formatUUID(f.sb.uuid),
		BlockSize: f.sb.blockSize,
		Size:      f.size,
		Features:  f.sb.featureNames(),
		Encrypted: f.sb.hasIncompat(incompatEncrypt),
		Warnings:  f.warnings.Snapshot(),
	}
}

// metadataRanges lists the block ranges that hold group 0's metadata: from
// block 0 through the superblock and group descriptor table, and the group's
// two bitmaps and inode table (when its descriptor is usable).
func metadataRanges(sb *superblock, groups []groupDesc) [][2]uint64 {
	gdtBlocks := (uint64(sb.groups)*uint64(sb.descSize) + uint64(sb.blockSize) - 1) / uint64(sb.blockSize) // <= 32 MiB table: cannot overflow
	rs := [][2]uint64{{0, max(uint64(sb.firstDataBlock), uint64(sb.logicalSuperBlock())+1+gdtBlocks)}}
	if len(groups) > 0 && !groups[0].bad {
		g := groups[0]
		rs = append(rs,
			[2]uint64{g.blockBitmap, g.blockBitmap + 1},
			[2]uint64{g.inodeBitmap, g.inodeBitmap + 1},
			[2]uint64{g.inodeTable, g.inodeTable + uint64(sb.itableBlocks)})
	}
	return rs
}
