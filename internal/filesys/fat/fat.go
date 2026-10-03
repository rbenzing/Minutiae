// Package fat is a read-only FAT12/FAT16/FAT32 parser. It works on an
// io.ReaderAt, never writes, and never panics on hostile input: every on-disk
// number is validated before it drives a loop or an allocation, and the
// cluster count is clamped to what the image and the FAT physically hold. It
// implements the filesys interfaces and imports no other Minutiae package.
package fat

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheSectors = 256 // sectors held by the metadata cache

	// maxWarnings bounds Info().Warnings: a hostile image can make every
	// directory or chain report a problem.
	maxWarnings = 1000

	// maxDirEntries bounds the entries read from one directory (65536 x 32 bytes).
	maxDirEntries = 65536
)

// FS is an opened FAT12/16/32 volume. After Open it is safe for concurrent use:
// the only mutable state is the warning list, which is mutex-protected.
type FS struct {
	b         *bpb
	fatType   int         // 12, 16 or 32
	count     uint32      // CountOfClusters after clamping
	activeFAT int         // FAT copy that is read
	fatOff    int64       // byte offset of the active FAT copy
	r         io.ReaderAt // cached view of the volume for metadata, clamped to its size
	size      int64       // volume size in bytes (declared size clamped to the image)
	label     string      // volume label: the root directory entry, else the BPB

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

// warn records a problem that does not stop the read. Identical messages are
// recorded once, and at most maxWarnings distinct messages are kept: after that
// a single "further warnings suppressed" line stands for the rest. It is safe
// for concurrent use. On-disk strings must be %q-quoted by the caller.
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

// Probe reports whether the first sector of the volume holds a FAT12/16/32 boot
// sector: the 0x55AA signature, an x86 jump (0xEB ?? 0x90 or 0xE9), a valid BPB
// that fits the volume, and not an exFAT or NTFS OEM id.
func Probe(r io.ReaderAt, size int64) bool {
	if size < bootSize {
		return false
	}
	var b [bootSize]byte
	if err := readFull(r, b[:], 0); err != nil {
		return false
	}
	_, _, err := parseBoot(b[:], size)
	return err == nil
}

// Open parses the boot sector of the FAT volume in r (size bytes). It fails
// with a *filesys.CorruptError for an invalid BPB. A volume larger than the
// image, a FAT too small for the declared clusters and a FAT32 BPB whose
// cluster count is that of a smaller type are reported through
// Info().Warnings, with the cluster count clamped to what physically exists.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	if size < bootSize {
		return nil, corrupt("FAT boot sector", 0, "image of %d bytes is too small for a boot sector", size)
	}
	var buf [bootSize]byte
	if err := readFull(r, buf[:], 0); err != nil {
		return nil, corrupt("FAT boot sector", 0, "read failed: %v", err)
	}
	b, warns, err := parseBoot(buf[:], size)
	if err != nil {
		return nil, err
	}
	bps := int64(b.bytsPerSec)
	volSize := int64(b.totSec) * bps // <= size: cannot overflow
	raw := io.NewSectionReader(r, 0, volSize)
	fatOff, ok := filesys.MulOK(int64(b.rsvd)+int64(b.activeFAT)*int64(b.fatSz), bps)
	if !ok {
		return nil, corrupt("FAT boot sector", 14, "FAT offset overflows")
	}
	f := &FS{
		b:         b,
		fatType:   b.fatType,
		count:     b.count,
		activeFAT: b.activeFAT,
		fatOff:    fatOff,
		r:         filesys.NewCachedReader(raw, b.bytsPerSec, cacheSectors),
		size:      volSize,
		label:     b.label,
	}
	for _, w := range warns {
		f.warn("%s", w)
	}
	f.readRootLabel()
	return f, nil
}

// Info describes the volume. The type follows the cluster count (FAT12 below
// 4085 clusters, FAT16 below 65525, else FAT32), except that the BPB layout
// wins when the two disagree (with a warning). The label is the volume-label
// entry of the root directory, else the BPB's ("NO NAME" counts as none). The
// UUID is the volume serial number as "XXXX-XXXX". Warnings is a snapshot: it
// holds the problems found at Open and those met since, without duplicates and
// capped at 1000 entries.
func (f *FS) Info() filesys.Info {
	f.wmu.Lock()
	warnings := slices.Clone(f.warnings)
	f.wmu.Unlock()
	return filesys.Info{
		Type:      fmt.Sprintf("fat%d", f.fatType),
		Label:     f.label,
		UUID:      fmt.Sprintf("%04X-%04X", f.b.volID>>16, f.b.volID&0xFFFF),
		BlockSize: f.b.bytsPerSec * f.b.secPerClus,
		Size:      f.size,
		Warnings:  warnings,
	}
}

// scanRoot calls fn with each 32-byte entry of the root directory up to the
// first end-of-directory entry (0x00, not passed to fn) or maxDirEntries
// entries, until fn returns true. If the FAT32 root chain breaks, the entries
// before the break are still passed and the chain error is returned.
func (f *FS) scanRoot(fn func(e []byte) (stop bool)) error {
	seen := 0
	visit := func(chunk []byte) (stop bool) {
		for i := 0; i+32 <= len(chunk); i += 32 {
			if seen >= maxDirEntries || chunk[i] == 0x00 {
				return true
			}
			seen++
			if fn(chunk[i : i+32]) {
				return true
			}
		}
		return false
	}
	bps := f.b.bytsPerSec
	if f.fatType != 32 {
		start := (int64(f.b.rsvd) + int64(f.b.numFATs)*int64(f.b.fatSz)) * int64(bps) // inside the volume: checked by parseBoot
		total := int64(f.b.rootEnt) * 32
		buf := make([]byte, bps)
		for off := int64(0); off < total; off += int64(bps) {
			chunk := buf[:min(int64(bps), total-off)]
			if err := readFull(f.r, chunk, start+off); err != nil {
				return corrupt("root directory", start+off, "read failed: %v", err)
			}
			if visit(chunk) {
				return nil
			}
		}
		return nil
	}
	cs := bps * f.b.secPerClus
	clusters, _, chainErr := f.chainN(f.b.rootClus, (maxDirEntries*32+cs-1)/cs)
	buf := make([]byte, cs)
	for _, c := range clusters {
		off, ok := f.clusterOffset(c)
		if !ok {
			return corrupt("root directory", -1, "cluster %d has no valid offset", c)
		}
		if err := readFull(f.r, buf, off); err != nil {
			return corrupt("root directory", off, "read failed: %v", err)
		}
		if visit(buf) {
			return nil
		}
	}
	return chainErr
}

// readRootLabel sets the label from the volume-label entry of the root
// directory. Reading problems are warnings: the BPB label stays in force.
func (f *FS) readRootLabel() {
	err := f.scanRoot(func(e []byte) bool {
		attr := e[11]
		if e[0] == 0xE5 || attr&0x0F == 0x0F || attr&0x18 != 0x08 {
			return false // deleted, long-name or not a volume label
		}
		if l := oemString(e[:11]); l != "" {
			f.label = l
			return true
		}
		return false
	})
	if err != nil {
		f.warn("root directory unreadable while looking for the volume label: %v", err)
	}
}
