// Package ext4 is a read-only ext2/ext3/ext4 parser. It works on an
// io.ReaderAt, never writes, and never panics on hostile input: every on-disk
// number is validated before it drives a loop or an allocation. It implements
// the filesys interfaces and imports no other Minutiae package.
package ext4

import (
	"errors"
	"io"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheBlocks = 256 // blocks held by the metadata cache
)

// FS is an opened ext2/3/4 filesystem.
type FS struct {
	sb       *superblock
	groups   []groupDesc
	r        io.ReaderAt // cached view of the filesystem for metadata, clamped to size
	data     io.ReaderAt // uncached view for file content, clamped to size
	size     int64       // filesystem size in bytes (declared size clamped to the image)
	warnings []string
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
	return &FS{
		sb:       sb,
		groups:   groups,
		r:        cached,
		data:     raw,
		size:     sb.blocksCount * int64(sb.blockSize), // cannot overflow: <= size
		warnings: append(warns, gw...),
	}, nil
}

// Info describes the filesystem. The type is ext4 when any ext4-only feature
// is set, ext3 when it has a journal, and ext2 otherwise.
func (f *FS) Info() filesys.Info {
	return filesys.Info{
		Type:      f.sb.fsType(),
		Label:     f.sb.label,
		UUID:      formatUUID(f.sb.uuid),
		BlockSize: f.sb.blockSize,
		Size:      f.size,
		Features:  f.sb.featureNames(),
		Encrypted: f.sb.hasIncompat(incompatEncrypt),
		Warnings:  slices.Clone(f.warnings),
	}
}
