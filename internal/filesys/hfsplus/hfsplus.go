// Package hfsplus is a read-only HFS+/HFSX parser (macOS volumes and the user
// partition of older iOS devices). It works on an io.ReaderAt, never writes,
// and never panics on hostile input: every on-disk number is validated before
// it drives a loop or an allocation. It implements the filesys interfaces and
// imports no other Minutiae package. Everything on disk is big-endian.
package hfsplus

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	cacheBlockSize = 4096 // bytes per metadata cache block, whatever the volume's block size
	cacheBlocks    = 256  // blocks held by the metadata cache
)

// FS is an opened HFS+/HFSX volume. After Open it is safe for concurrent use:
// the only mutable state is the warning collector, which is mutex-protected.
type FS struct {
	vh   *volumeHeader
	base int64       // byte offset of the volume from the start of the ReaderAt (non-zero in an HFS wrapper)
	r    io.ReaderAt // cached view of the volume for metadata, relative to base and clamped to the volume
	size int64       // Info.Size: base plus the volume size, clamped to the image

	wrapped bool
	// caseSensitive is meaningful only when caseKnown: the catalog B-tree
	// header's keyCompareType decided it (0xBC binary = case-sensitive).
	caseSensitive, caseKnown bool
	journal                  journalState

	warnings filesys.Warnings

	// dirBudget is how many bytes of catalog nodes listings and fallback scans may
	// still read, shared by every call (see chargeDir); dirCap overrides the
	// per-listing entry cap (0 = maxDirEntries).
	dirMu     sync.Mutex
	dirBudget int64
	dirCap    int
	extentCap int // overrides maxExtentsPerFork when positive (tests)

	rootMu    sync.Mutex
	rootEntry *filesys.Entry // the root folder as its record describes it, once readable

	treeMu sync.Mutex
	trees  [numTreeKinds]*btree // lazily opened B-trees, see tree
	label  string               // the volume name, from the root folder thread
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

// warn records a problem that does not stop the read (see filesys.Warnings:
// identical messages once, at most filesys.MaxWarnings distinct ones). On-disk
// strings must be %q-quoted by the caller.
func (f *FS) warn(format string, a ...any) { f.warnings.Add(format, a...) }

// location says where the volume sits in the image and where its two volume
// headers are. pri is the raw primary header; altPos is the absolute offset of
// the alternate header, or -1 when it cannot be placed.
type location struct {
	base      int64
	truncated bool // the HFS wrapper's embedded extent ends beyond the image
	wrapped   bool
	pri       [vhSize]byte
	altPos    int64
}

// locate finds the volume: directly (a header at byte 1024) or through an HFS
// wrapper. It reads only the sector at 1024 and the primary header. A read
// error is returned as-is; a classic HFS volume without an embedded HFS+
// volume is filesys.ErrUnsupported; an unusable wrapper is a CorruptError.
// Whether the primary header is valid is for the caller to decide.
func locate(r io.ReaderAt, size int64) (*location, error) {
	if size < minVolumeSize {
		return nil, corrupt("HFS+ volume header", vhOffset, "image of %d bytes is too small for a volume header", size)
	}
	loc := &location{altPos: -1}
	if err := readFull(r, loc.pri[:], vhOffset); err != nil {
		return nil, fmt.Errorf("hfsplus: read volume header: %w", err)
	}
	if be16(loc.pri[:]) == mdbSig {
		w, err := parseWrapper(loc.pri[:], size)
		if err != nil {
			return nil, err
		}
		loc.base, loc.wrapped, loc.truncated = w.base, true, w.truncated
		if err := readFull(r, loc.pri[:], w.base+vhOffset); err != nil { // w.base+1536 <= size: checked by parseWrapper
			return nil, fmt.Errorf("hfsplus: read embedded volume header: %w", err)
		}
		// The alternate header is 1024 bytes before the end of the embedded
		// extent, when that lies after the primary header and inside the image.
		if alt := w.base + w.length - altFromEnd; alt >= w.base+vhOffset+vhSize && alt+vhSize <= size {
			loc.altPos = alt
		}
		return loc, nil
	}
	// The alternate header is 1024 bytes before the end of the volume; with
	// the volume's own block count unknown (the primary may be the damaged
	// one), take the end of the image rounded down to a sector.
	if alt := size/sectorSize*sectorSize - altFromEnd; alt >= vhOffset+vhSize {
		loc.altPos = alt
	}
	return loc, nil
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// Probe reports whether the image holds an HFS+ or HFSX volume: a volume
// header at byte 1024, or an HFS wrapper whose embedded header, at the
// embedded volume's byte 1024, passes the sanity checks of parseVolumeHeader
// (matching signature and version, a block size that is a multiple of 512 up
// to 1 GiB, at least one block, catalog ids from 16, and non-empty catalog and
// allocation forks whose first extent lies inside the volume). It reads
// nothing from an image shorter than 1536 bytes. Only the primary header is
// consulted: a volume whose primary header is destroyed is not claimed (Open
// can still read it from the alternate header).
func Probe(r io.ReaderAt, size int64) bool {
	if size < minVolumeSize {
		return false
	}
	loc, err := locate(r, size)
	if err != nil {
		return false
	}
	_, err = parseVolumeHeader(loc.pri[:])
	return err == nil
}

// Open parses the volume header (falling back to the alternate header when the
// primary one is unusable, with a warning) and the HFS wrapper, if any. It
// fails with filesys.ErrUnsupported for a classic HFS volume without an
// embedded HFS+ volume and with a *filesys.CorruptError when neither volume
// header is usable or the geometry is impossible. A volume that claims more
// than the image holds, a journal that is pending or unreadable and a volume
// that was not cleanly unmounted are reported through Info().Warnings.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	loc, err := locate(r, size)
	if err != nil {
		return nil, err
	}
	var warns []string
	vh, perr := validHeader(loc.pri[:])
	if perr != nil {
		// Where can the alternate header be? When the primary header parsed
		// but failed its checks, its own geometry says: 1024 bytes before the
		// end of the volume it declares (right for a wrapper whose embedded
		// extent is longer than the volume and for an image with trailing
		// bytes). The device end (or the wrapper extent's end) is the other
		// candidate; the first one that validates wins.
		var cands []int64
		if vh != nil {
			if end, ok := filesys.AddOK(loc.base, vh.volumeBytes()); ok {
				if alt := end - altFromEnd; alt >= loc.base+vhOffset+vhSize && alt+vhSize <= size {
					cands = append(cands, alt)
				}
			}
		}
		if loc.altPos >= 0 && !slices.Contains(cands, loc.altPos) {
			cands = append(cands, loc.altPos)
		}
		aerr := errors.New("the alternate header cannot be placed")
		for i, pos := range cands {
			var alt [vhSize]byte
			err := readFull(r, alt[:], pos)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("hfsplus: read alternate volume header: %w", err) // an I/O error is not a corrupt volume
			}
			var v *volumeHeader
			if err == nil {
				v, err = validHeader(alt[:])
			}
			if err == nil {
				vh, aerr = v, nil
				break
			}
			if i == 0 {
				aerr = err
			}
		}
		if aerr != nil {
			var ce *filesys.CorruptError
			if errors.As(perr, &ce) {
				return nil, corrupt(ce.Structure, ce.Offset, "%s; the alternate volume header is also unusable: %v", ce.Reason, aerr)
			}
			return nil, perr
		}
		warns = append(warns, fmt.Sprintf("primary volume header is unusable (%v); using the alternate volume header", perr))
	}

	if loc.truncated {
		warns = append(warns, "the HFS wrapper's embedded extent ends beyond the image: truncated image")
	}
	declared := vh.volumeBytes()
	volLen := declared
	if avail := size - loc.base; avail < declared {
		volLen = avail
		warns = append(warns, fmt.Sprintf("the volume declares %d bytes but the image holds %d from the volume start: truncated image", declared, avail))
	}
	end, ok := filesys.AddOK(loc.base, volLen)
	if !ok { // unreachable: both terms are bounded by int64 sizes that fit the image
		return nil, corrupt("HFS+ volume header", vhOffset, "volume end overflows")
	}
	vol := io.NewSectionReader(r, loc.base, volLen)
	f := &FS{
		vh:      vh,
		base:    loc.base,
		r:       filesys.NewCachedReader(vol, cacheBlockSize, cacheBlocks),
		size:    end,
		wrapped: loc.wrapped,

		dirBudget: maxDirBudget,
	}
	for _, w := range warns {
		f.warn("%s", w)
	}
	if vh.attributes&attrInconsistent != 0 {
		f.warn("the volume is marked inconsistent")
	}
	if vh.attributes&attrBootInconsistent != 0 {
		f.warn("the volume is marked boot-volume inconsistent")
	}
	f.loadCaseMode()
	f.loadJournal()
	f.loadLabel()
	if vh.attributes&attrJournaled == 0 && vh.attributes&attrUnmounted == 0 {
		f.warn("the volume was not cleanly unmounted (the unmounted attribute is clear and there is no journal)")
	}
	return f, nil
}

// validHeader parses a header and checks its geometry. When the header parses
// but its geometry is impossible the header is returned together with the
// error, so the caller can still use its declared size.
func validHeader(b []byte) (*volumeHeader, error) {
	vh, err := parseVolumeHeader(b)
	if err != nil {
		return nil, err
	}
	if err := vh.checkGeometry(); err != nil {
		return vh, err
	}
	return vh, nil
}

// Catalog B-tree header: the header node is node 0, kind 1; its header record
// starts at byte 14 and keyCompareType is the byte at offset 37 of the record
// (TN1150 "Header Record"; confirmed on real images: 0xCF for H+, 0xBC for
// HFSX made with -s).
const (
	nodeDescSize       = 14
	offNodeKind        = 8
	nodeKindHeader     = 1
	offBTKeyCompare    = 37
	keyCompareBinary   = 0xBC // kHFSBinaryCompare: case-sensitive
	keyCompareFolding  = 0xCF // kHFSCaseFolding: case-insensitive (0 means the same)
	btHeaderPrefixSize = nodeDescSize + offBTKeyCompare + 1
)

// loadCaseMode decides case sensitivity. An H+ volume is ALWAYS case-insensitive
// (the HFS+ driver folds case whatever the catalog header says); only an HFSX
// volume takes its mode from the catalog B-tree header's keyCompareType: 0xBC
// (binary) is case-sensitive, 0xCF or 0 is case folding. The signature alone
// does not decide it (an HFSX volume can fold case). The full B-tree layer
// reads the same byte again; here it only has to name the volume's mode.
func (f *FS) loadCaseMode() {
	hfsx := f.vh.hfsx()
	if !hfsx {
		f.caseKnown = true // case-insensitive, whatever the header says
	}
	e := f.vh.catalog.extents[0] // non-empty and inside the volume: checked by parseVolumeHeader/checkGeometry
	var b [btHeaderPrefixSize]byte
	if err := readFull(f.r, b[:], int64(e.start)*int64(f.vh.blockSize)); err != nil {
		f.warn("the catalog B-tree header cannot be read, so the key comparison mode is unknown: %v", err)
		return
	}
	if int8(b[offNodeKind]) != nodeKindHeader {
		f.warn("the catalog B-tree has no header node (node kind %d), so the key comparison mode is unknown", int8(b[offNodeKind]))
		return
	}
	kc := b[nodeDescSize+offBTKeyCompare]
	switch {
	case !hfsx:
		if kc != keyCompareFolding && kc != 0 {
			f.warn("the catalog B-tree of this HFS+ volume has keyCompareType %#02x; HFS+ is always case-insensitive and is read as such", kc)
		}
	case kc == keyCompareBinary:
		f.caseSensitive, f.caseKnown = true, true
	case kc == keyCompareFolding || kc == 0:
		f.caseKnown = true
	default:
		f.warn("the catalog B-tree has unknown keyCompareType %#02x, so case sensitivity is unknown", kc)
	}
}

// Info describes the volume. Type is "hfsplus" for an H+ volume and "hfsx" for
// HX; BlockSize is the allocation block size; Size is the end of the volume
// from the start of the image (the wrapper offset plus the volume size,
// clamped to the image); UUID is the 64-bit volume id from the header's Finder
// info (empty when zero). Warnings is a snapshot: it holds the problems found
// at Open and those met since, without duplicates and capped at 1000 entries.
func (f *FS) Info() filesys.Info {
	typ := "hfsplus"
	if f.vh.hfsx() {
		typ = "hfsx"
	}
	var feats []string
	if f.caseKnown {
		if f.caseSensitive {
			feats = append(feats, "case-sensitive")
		} else {
			feats = append(feats, "case-insensitive")
		}
	}
	if f.vh.attributes&attrJournaled != 0 {
		feats = append(feats, "journaled")
	}
	if f.wrapped {
		feats = append(feats, "hfs-wrapper")
	}
	if f.vh.attributes&attrCNIDsReused != 0 {
		feats = append(feats, "cnids-reused")
	}
	if f.vh.attributes&attrSoftwareLock != 0 {
		feats = append(feats, "software-locked")
	}
	if f.vh.attributes&attrHardwareLock != 0 {
		feats = append(feats, "hardware-locked")
	}
	if f.vh.attributes&attrContentProtect != 0 {
		feats = append(feats, "content-protection")
	}
	return filesys.Info{
		Type:      typ,
		Label:     f.label,
		UUID:      f.vh.uuid(),
		BlockSize: int(f.vh.blockSize),
		Size:      f.size,
		Features:  feats,
		Warnings:  f.warnings.Snapshot(),
	}
}
