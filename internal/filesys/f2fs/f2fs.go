// Package f2fs is a read-only F2FS parser (Android userdata and other
// partitions). It works on an io.ReaderAt, never writes, and never panics on
// hostile input: every on-disk number is validated before it drives a loop or
// an allocation. It implements the filesys interfaces and imports no other
// Minutiae package.
package f2fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const cacheBlocks = 256 // blocks held by the metadata cache

// FS is an opened F2FS filesystem, as of its last valid checkpoint. After Open
// it is safe for concurrent use: the only mutable state is the warning
// collector, which is mutex-protected.
type FS struct {
	sb   *superblock
	cp   *checkpoint
	data io.ReaderAt // uncached view of the whole image, for file content
	r    io.ReaderAt // cached view of the filesystem for metadata, clamped to size
	size int64       // filesystem size in bytes (declared size clamped to the image)

	nat natState // NAT journal, read on first use
	sit sitState // SIT journal, read on first use

	// dirCap overrides the per-scan directory entry cap (0 = maxDirEntries).
	dirCap int

	// unallocCap bounds the runs Unallocated reports (0 = maxUnallocRuns).
	unallocCap int

	// dmu guards dirBudget, the bytes of dentry blocks this instance may still
	// read (dirBudgetTotal at the start), shared by every listing and lookup.
	dmu            sync.Mutex
	dirBudget      int64
	dirBudgetTotal int64
	// dirClaims maps the byte address of each dentry block read so far to the
	// directory (nid) that read it first (also guarded by dmu); a block another
	// directory maps is skipped. Bounded by dirBudgetTotal/blockSize entries.
	dirClaims map[int64]uint32

	warnings filesys.Warnings
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

// isIOError reports whether err is a genuine read failure rather than the
// source ending early (a truncated image), which is a property of the data.
func isIOError(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)
}

// readError converts a failed metadata read. A genuine I/O error is returned
// wrapped as-is (it is not evidence of a corrupt filesystem, and callers can
// tell it apart with errors.Is); a short read is a *filesys.CorruptError.
func readError(structure string, off int64, err error) error {
	if isIOError(err) {
		return fmt.Errorf("%s: read at offset %d failed: %w", structure, off, err)
	}
	return corrupt(structure, off, "read failed: %v", err)
}

// ioError marks a failure to read part of a checkpoint pack so that pack
// selection can tell it from an invalid pack.
type ioError struct{ err error }

func (e *ioError) Error() string { return e.err.Error() }
func (e *ioError) Unwrap() error { return e.err }

// readFailed describes a failed checkpoint read: a genuine I/O error becomes
// an *ioError (surfaced by selectCheckpoint), a short read a plain reason.
func readFailed(what string, err error) error {
	if isIOError(err) {
		return &ioError{fmt.Errorf("%s failed: %w", what, err)}
	}
	return fmt.Errorf("%s failed: %v", what, err)
}

// warn records a problem that does not stop the read in the shared,
// deduplicated and capped filesys.Warnings collector. Safe for concurrent use.
func (f *FS) warn(format string, a ...any) { f.warnings.Add(format, a...) }

// superblockLooksValid reports whether the 8 KiB head holds an F2FS superblock
// at off: the magic, major_ver 1 and 4 KiB blocks (the only geometry this
// reader supports).
func superblockLooksValid(r io.ReaderAt, off int64) bool {
	var b [sbLogBlockSize + 4]byte
	if readFull(r, b[:], off) != nil {
		return false
	}
	le := binary.LittleEndian
	return le.Uint32(b[sbMagic:]) == superMagic && le.Uint16(b[sbMajorVer:]) == 1 && le.Uint32(b[sbLogBlockSize:]) == blockShift
}

// Probe reports whether the first 8 KiB of the volume hold a valid PRIMARY F2FS
// superblock at byte 1024: the magic, major_ver 1 and 4 KiB blocks (the only
// geometry this reader supports), so that detection can fall through to other
// drivers for anything else. The backup copy is deliberately not consulted: a
// stale or forged backup signature inside another filesystem must not claim
// the volume ahead of that filesystem's own driver (see ProbeBackup).
func Probe(r io.ReaderAt, size int64) bool {
	return size >= minImage && superblockLooksValid(r, superOffset)
}

// ProbeBackup is the last-resort probe: it reports whether the BACKUP
// superblock at byte 4096+1024 is valid while the primary is not (a destroyed
// primary). Detection registers it after every other driver, so a volume
// another driver recognizes is never claimed through a backup signature alone.
// Open already falls back to the backup on its own.
func ProbeBackup(r io.ReaderAt, size int64) bool {
	return size >= minImage && !superblockLooksValid(r, superOffset) && superblockLooksValid(r, backupBlock*blockSize+superOffset)
}

// loadSuper reads the primary superblock and, when it is unusable, the backup.
// It returns the warnings to record. When both copies fail the error is a
// *filesys.CorruptError describing both.
func loadSuper(raw io.ReaderAt) (*superblock, []string, error) {
	var errs [2]error
	for i, base := range []int64{superOffset, backupBlock*blockSize + superOffset} {
		buf := make([]byte, superSize)
		if err := readFull(raw, buf, base); err != nil {
			if isIOError(err) {
				return nil, nil, readError("f2fs superblock", base, err)
			}
			errs[i] = corrupt("f2fs superblock", base, "read failed: %v", err)
			continue
		}
		sb, err := parseSuper(buf, base)
		if err != nil {
			errs[i] = err
			continue
		}
		if i == 1 {
			return sb, append([]string{fmt.Sprintf("primary superblock is unusable (%v); using the backup superblock at block %d", errs[0], backupBlock)}, sb.warns...), nil
		}
		return sb, sb.warns, nil
	}
	var ce *filesys.CorruptError
	if errors.As(errs[0], &ce) {
		return nil, nil, corrupt(ce.Structure, ce.Offset, "%s (backup superblock: %v)", ce.Reason, errs[1])
	}
	return nil, nil, errs[0]
}

// Open parses the superblock (primary, else backup) and selects the valid
// checkpoint pack with the highest version. It fails with a
// *filesys.CorruptError for an invalid superblock or when no checkpoint pack
// is valid. A truncated image, a skipped (invalid) checkpoint pack, a backup
// superblock in use and an unclean unmount are reported in Info().Warnings.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	if size < minImage {
		return nil, corrupt("f2fs superblock", superOffset, "image of %d bytes is too small for the two superblock copies", size)
	}
	raw := io.NewSectionReader(r, 0, size)
	sb, warns, err := loadSuper(raw)
	if err != nil {
		return nil, err
	}
	// Clamp to what the image actually holds.
	sb.blocks = int64(sb.blockCount) // <= 2^32
	if have := size / blockSize; have < sb.blocks {
		sb.blocks = have
		warns = append(warns, fmt.Sprintf("truncated image: the superblock declares %d blocks of %d bytes but the image holds %d", sb.blockCount, blockSize, have))
	}
	cached := filesys.NewCachedReader(raw, blockSize, cacheBlocks)
	cp, cw, err := selectCheckpoint(cached, sb)
	if err != nil {
		return nil, err
	}
	f := &FS{sb: sb, cp: cp, r: cached, data: raw, size: sb.blocks * blockSize, dirBudget: maxDirBudget, dirBudgetTotal: maxDirBudget}
	if capacity := sb.natCapacity(); uint64(cp.nextFreeNid) > capacity || uint64(cp.validNodes) > capacity {
		f.warn("checkpoint next_free_nid %d or valid_node_count %d exceeds the NAT capacity of %d node ids", cp.nextFreeNid, cp.validNodes, capacity)
	}
	for _, w := range append(append(warns, cw...), cp.warnings()...) {
		f.warn("%s", w)
	}
	return f, nil
}

// Info describes the filesystem. Warnings is a snapshot: it holds the problems
// found at Open and those met since by later reads, without duplicates and
// capped at filesys.MaxWarnings entries.
func (f *FS) Info() filesys.Info {
	features := f.sb.featureList()
	if f.cp.pack2Blank {
		features = append(features, "checkpoint pack 2 blank")
	}
	return filesys.Info{
		Type:      "f2fs",
		Label:     f.sb.label,
		UUID:      formatUUID(f.sb.uuid),
		BlockSize: blockSize,
		Size:      f.size,
		Features:  features,
		Encrypted: f.sb.has(featEncrypt),
		Warnings:  f.warnings.Snapshot(),
	}
}
