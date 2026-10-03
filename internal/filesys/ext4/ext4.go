// Package ext4 is a read-only ext2/ext3/ext4 parser. It works on an
// io.ReaderAt, never writes, and never panics on hostile input: every on-disk
// number is validated before it drives a loop or an allocation. It implements
// the filesys interfaces and imports no other Minutiae package.
package ext4

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheBlocks = 256 // blocks held by the metadata cache

	// maxWarnings bounds Info().Warnings: a hostile image can make every
	// directory block or extent node report a problem.
	maxWarnings = 1000
)

// FS is an opened ext2/3/4 filesystem. After Open it is safe for concurrent
// use: the only mutable state is the warning list, which is mutex-protected.
type FS struct {
	sb     *superblock
	groups []groupDesc
	r      io.ReaderAt // cached view of the filesystem for metadata, clamped to size
	data   io.ReaderAt // uncached view for file content, clamped to size
	size   int64       // filesystem size in bytes (declared size clamped to the image)

	wmu      sync.Mutex
	warnings []string
	warnSeen map[string]struct{}
	warnFull bool // the cap was reached and the "suppressed" line is in warnings
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
// damaged directory block ...). Identical messages are recorded once, and at
// most maxWarnings distinct messages are kept: after that a single "further
// warnings suppressed" line stands for the rest. It is safe for concurrent use.
func (f *FS) warn(format string, a ...any) {
	msg := format
	if len(a) > 0 {
		msg = fmt.Sprintf(format, a...)
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if _, dup := f.warnSeen[msg]; dup {
		return
	}
	if len(f.warnings) >= maxWarnings {
		if !f.warnFull {
			f.warnFull = true
			f.warnings = append(f.warnings, "further warnings suppressed")
		}
		return
	}
	if f.warnSeen == nil {
		f.warnSeen = map[string]struct{}{}
	}
	f.warnSeen[msg] = struct{}{}
	f.warnings = append(f.warnings, msg)
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
	f := &FS{
		sb:     sb,
		groups: groups,
		r:      cached,
		data:   raw,
		size:   sb.blocksCount * int64(sb.blockSize), // cannot overflow: <= size
	}
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
	f.wmu.Lock()
	warnings := slices.Clone(f.warnings)
	f.wmu.Unlock()
	return filesys.Info{
		Type:      f.sb.fsType(),
		Label:     f.sb.label,
		UUID:      formatUUID(f.sb.uuid),
		BlockSize: f.sb.blockSize,
		Size:      f.size,
		Features:  f.sb.featureNames(),
		Encrypted: f.sb.hasIncompat(incompatEncrypt),
		Warnings:  warnings,
	}
}

// Unallocated is not implemented yet (plan 2B, Task 5).
func (f *FS) Unallocated() ([]filesys.Run, error) {
	return nil, fmt.Errorf("ext4: listing unallocated space: %w", filesys.ErrUnsupported)
}
