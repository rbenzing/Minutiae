// Package exfat is a read-only exFAT parser. It works on an io.ReaderAt, never
// writes, and never panics on hostile input: every on-disk number is
// validated before it drives a loop or an allocation. It implements the
// filesys interfaces and imports no other Minutiae package.
//
// Directory entry sets are validated (File, Stream, Name entries and the set
// checksum); a set that does not verify is still listed, with the attribute
// checksum=bad. Entry sets whose InUse bits are cleared are listed as
// deleted; their content is never opened (recovery is roadmap sub-project 3),
// but the first cluster and size stay in the attributes. TexFAT (a second
// FAT and bitmap) is not interpreted: the first FAT and the first bitmap are
// read.
package exfat

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheBlockSize = 4096
	cacheBlocks    = 256 // blocks held by the metadata cache

	// maxWarnings bounds Info().Warnings: a hostile image can make every
	// directory entry report a problem.
	maxWarnings = 1000
)

// FS is an opened exFAT volume. After Open it is safe for concurrent use: the
// only mutable state is the warning list, which is mutex-protected.
type FS struct {
	r    io.ReaderAt // cached view of the volume for metadata, clamped to size
	data io.ReaderAt // uncached view for file content, clamped to size
	size int64       // volume size in bytes (VolumeLength clamped to the image)

	ss, cs       int64  // bytes per sector and per cluster
	heapOff      int64  // byte offset of cluster 2
	fatOff       int64  // byte offset of the FAT
	fatEntries   uint32 // FAT entries that exist and are within the cluster count
	clusterCount uint32 // clusters 2 .. clusterCount+1
	rootCluster  uint32
	serial       uint32
	revision     uint16
	volFlags     uint16

	label  string
	bitmap meta // the Allocation Bitmap entry of the root directory
	// upcase maps every UTF-16 unit to its up-case form; nil when the volume
	// has no usable table (Lookup then folds with strings.EqualFold).
	upcase *[65536]uint16

	// dirRecordCap bounds the entry sets one directory yields (maxDirRecords).
	dirRecordCap int

	wmu      sync.Mutex
	warnings []string
	warnSeen map[string]struct{}
	warnFull bool // the cap was reached and the "suppressed" line is in warnings
}

// meta is a system file named by a root directory entry (bitmap, up-case).
type meta struct {
	present bool
	first   uint32
	length  uint64
	aux     uint32 // bitmap: BitmapFlags; up-case table: TableChecksum
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

func corrupt(structure string, off int64, format string, a ...any) error {
	return &filesys.CorruptError{Structure: structure, Offset: off, Reason: fmt.Sprintf(format, a...)}
}

// warn records a problem that does not stop the read (a checksum mismatch, a
// damaged directory ...). Identical messages are recorded once, and at most
// maxWarnings distinct messages are kept: after that a single "further
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

// Probe reports whether the first sector of the volume is an exFAT boot
// sector: the file system name "EXFAT   " and legal sector and cluster
// shifts. It reads nothing from a volume of fewer than 512 bytes.
func Probe(r io.ReaderAt, size int64) bool {
	if size < bootSize {
		return false
	}
	var b [bootSize]byte
	if err := readFull(r, b[:], 0); err != nil {
		return false
	}
	return probeBoot(b[:])
}

// Open parses the boot sector of the exFAT volume in r (size bytes), verifies
// the boot region checksum, and reads the root directory's system entries
// (allocation bitmap, up-case table, volume label). It fails with a
// *filesys.CorruptError for a boot sector that cannot be laid out. A wrong
// boot checksum, a volume that claims more than the image holds and an
// unusable up-case table are reported through Info().Warnings.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	if size < bootSize {
		return nil, corrupt("exFAT boot sector", 0, "image of %d bytes is too small for a boot sector", size)
	}
	raw := io.NewSectionReader(r, 0, size)
	var sec [bootSize]byte
	if err := readFull(raw, sec[:], 0); err != nil {
		return nil, corrupt("exFAT boot sector", 0, "read failed: %v", err)
	}
	bt, err := parseBoot(sec[:])
	if err != nil {
		return nil, err
	}
	g, err := bt.layout(size)
	if err != nil {
		return nil, err
	}
	vol := io.NewSectionReader(r, 0, g.size)
	f := &FS{
		r:            filesys.NewCachedReader(vol, cacheBlockSize, cacheBlocks),
		data:         vol,
		size:         g.size,
		ss:           g.ss,
		cs:           g.cs,
		heapOff:      g.heapOff,
		fatOff:       g.fatOff,
		fatEntries:   g.fatEntries,
		clusterCount: g.clusterCount,
		rootCluster:  g.rootCluster,
		serial:       bt.serial,
		revision:     bt.revision,
		volFlags:     bt.volFlags,
		dirRecordCap: maxDirRecords,
	}
	for _, w := range g.warnings {
		f.warn("%s", w)
	}
	if bt.revision>>8 != 1 {
		f.warn("FileSystemRevision is %d.%02d; this reader understands revision 1", bt.revision>>8, bt.revision&0xFF)
	}
	f.checkBoot(vol)
	f.loadRootMeta()
	return f, nil
}

// checkBoot verifies the checksum sector of the main boot region.
func (f *FS) checkBoot(vol io.ReaderAt) {
	if f.size < bootRegionSectors*f.ss {
		f.warn("boot region checksum cannot be verified: the volume is shorter than the 12 sectors of the boot region")
		return
	}
	region := make([]byte, bootRegionSectors*f.ss) // at most 48 KiB
	if err := readFull(vol, region, 0); err != nil {
		f.warn("boot region checksum cannot be verified: %v", err)
		return
	}
	if stored, want, ok := checkBootChecksum(region, int(f.ss)); !ok {
		f.warn("boot region checksum mismatch (stored %#08x, computed %#08x)", stored, want)
	}
}

// Info describes the filesystem. Warnings is a snapshot: it holds the problems
// found at Open and those met since by directory, chain and bitmap reads,
// without duplicates and capped at 1000 entries.
func (f *FS) Info() filesys.Info {
	f.wmu.Lock()
	warnings := slices.Clone(f.warnings)
	f.wmu.Unlock()
	major, minor := f.revision>>8, f.revision&0xFF
	features := []string{fmt.Sprintf("revision %d.%02d", major, minor)}
	if f.volFlags&0x2 != 0 {
		features = append(features, "volume dirty")
	}
	if f.volFlags&0x4 != 0 {
		features = append(features, "media failure")
	}
	return filesys.Info{
		Type:      "exfat",
		Label:     f.label,
		UUID:      fmt.Sprintf("%04X-%04X", f.serial>>16, f.serial&0xFFFF),
		BlockSize: int(f.cs), // at most 32 MiB
		Size:      f.size,
		Features:  features,
		Warnings:  warnings,
	}
}
