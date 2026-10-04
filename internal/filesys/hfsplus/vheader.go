package hfsplus

import (
	"encoding/binary"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Volume header layout (bytes). Everything on disk is big-endian. Offsets are
// from Apple TN1150 / xnu hfs_format.h as remembered and were validated against
// real mkfs.hfsplus images (H+ with 4096- and 1024-byte blocks, journaled H+,
// case-sensitive HX): every field and fork read here, the unmounted and
// journaled attribute bits and the alternate header at size-1024 are confirmed.
// The cnids-reused, inconsistent and software-lock bits are from memory only.
const (
	vhOffset      = 1024              // the primary header, from the start of the volume
	vhSize        = 512               // header size
	minVolumeSize = vhOffset + vhSize // the least an image can be to hold a header
	altFromEnd    = 1024              // the alternate header starts this far before the end of the volume
	maxBlockSize  = 1 << 30           // largest allocation block size accepted
	firstUserCNID = 16                // first catalog node id handed to user objects
	maxInlineExts = 8                 // extents held in a fork data record
	extentRecSize = 8                 // sizeof(HFSPlusExtentDescriptor)

	sigHFSPlus     = 0x482B // "H+"
	sigHFSX        = 0x4858 // "HX"
	versionHFSPlus = 4
	versionHFSX    = 5

	offVHSignature   = 0
	offVHVersion     = 2
	offVHAttributes  = 4
	offVHLastMounted = 8
	offVHJournalInfo = 12
	offVHFileCount   = 32
	offVHFolderCount = 36
	offVHBlockSize   = 40
	offVHTotalBlocks = 44
	offVHFreeBlocks  = 48
	offVHNextAlloc   = 52
	offVHNextCNID    = 64
	offVHFinderInfo  = 80 // 8 x u32; words 6 and 7 hold the 64-bit volume id
	offVHAllocFile   = 112
	offVHExtentsFile = 192
	offVHCatalogFile = 272
	offVHAttrsFile   = 352
	offVHStartupFile = 432
)

// Volume attribute bits (from memory, TN1150 "Volume Attributes"; the bits the
// reader uses are checked against real images).
const (
	attrUnmounted    = 1 << 8  // kHFSVolumeUnmountedBit: set on a clean unmount
	attrCNIDsReused  = 1 << 12 // kHFSCatalogNodeIDsReusedBit
	attrJournaled    = 1 << 13 // kHFSVolumeJournaledBit
	attrInconsistent = 1 << 14 // kHFSVolumeInconsistentBit
	attrSoftwareLock = 1 << 15 // kHFSVolumeSoftwareLockBit
)

// extent is one HFSPlusExtentDescriptor: count allocation blocks from start.
type extent struct{ start, count uint32 }

// forkData is a decoded HFSPlusForkData.
type forkData struct {
	logicalSize uint64
	clumpSize   uint32
	totalBlocks uint32
	extents     [maxInlineExts]extent
}

// inline returns the inline extents up to (excluding) the first empty one, and
// their block total.
func (fd *forkData) inline() (exts []extent, blocks uint64) {
	for i := range fd.extents {
		e := fd.extents[i]
		if e.count == 0 {
			break
		}
		exts = append(exts, e)
		blocks += uint64(e.count) // at most 8 x 2^32: cannot overflow
	}
	return exts, blocks
}

type volumeHeader struct {
	signature        uint16
	version          uint16
	attributes       uint32
	lastMounted      uint32
	journalInfoBlock uint32
	fileCount        uint32
	folderCount      uint32
	blockSize        uint32
	totalBlocks      uint32
	freeBlocks       uint32
	nextAllocation   uint32
	nextCatalogID    uint32
	finderInfo       [8]uint32

	allocation, extents, catalog, attributesFile, startup forkData
}

func (vh *volumeHeader) hfsx() bool { return vh.signature == sigHFSX }

// volumeBytes is the size the header declares for the volume.
func (vh *volumeHeader) volumeBytes() int64 {
	return int64(vh.totalBlocks) * int64(vh.blockSize) // <= 2^32 x 2^30 = 2^62: cannot overflow
}

// uuid renders the volume id (finderInfo words 6 and 7) as 16 lowercase hex
// digits, or "" when it is zero.
func (vh *volumeHeader) uuid() string {
	hi, lo := vh.finderInfo[6], vh.finderInfo[7]
	if hi == 0 && lo == 0 {
		return ""
	}
	return fmt.Sprintf("%08x%08x", hi, lo)
}

func parseFork(b []byte) forkData {
	be := binary.BigEndian
	fd := forkData{
		logicalSize: be.Uint64(b[0:]),
		clumpSize:   be.Uint32(b[8:]),
		totalBlocks: be.Uint32(b[12:]),
	}
	for i := range fd.extents {
		o := 16 + i*extentRecSize
		fd.extents[i] = extent{start: be.Uint32(b[o:]), count: be.Uint32(b[o+4:])}
	}
	return fd
}

// parseVolumeHeader decodes a 512-byte volume header and runs the cheap
// sanity checks Probe relies on: signature and version agree, the block size
// is a multiple of 512 up to 1 GiB, there is at least one block, catalog ids
// start at 16, and the catalog and allocation files are non-empty forks whose
// first extent lies inside the volume. The full geometry check (checkGeometry)
// is separate: Open runs it on the header it chooses.
func parseVolumeHeader(b []byte) (*volumeHeader, error) {
	if len(b) < vhSize {
		return nil, corrupt("HFS+ volume header", vhOffset, "%d bytes is too small for a volume header", len(b))
	}
	be := binary.BigEndian
	vh := &volumeHeader{
		signature:        be.Uint16(b[offVHSignature:]),
		version:          be.Uint16(b[offVHVersion:]),
		attributes:       be.Uint32(b[offVHAttributes:]),
		lastMounted:      be.Uint32(b[offVHLastMounted:]),
		journalInfoBlock: be.Uint32(b[offVHJournalInfo:]),
		fileCount:        be.Uint32(b[offVHFileCount:]),
		folderCount:      be.Uint32(b[offVHFolderCount:]),
		blockSize:        be.Uint32(b[offVHBlockSize:]),
		totalBlocks:      be.Uint32(b[offVHTotalBlocks:]),
		freeBlocks:       be.Uint32(b[offVHFreeBlocks:]),
		nextAllocation:   be.Uint32(b[offVHNextAlloc:]),
		nextCatalogID:    be.Uint32(b[offVHNextCNID:]),
		allocation:       parseFork(b[offVHAllocFile:]),
		extents:          parseFork(b[offVHExtentsFile:]),
		catalog:          parseFork(b[offVHCatalogFile:]),
		attributesFile:   parseFork(b[offVHAttrsFile:]),
		startup:          parseFork(b[offVHStartupFile:]),
	}
	for i := range vh.finderInfo {
		vh.finderInfo[i] = be.Uint32(b[offVHFinderInfo+4*i:])
	}
	switch {
	case vh.signature == sigHFSPlus && vh.version == versionHFSPlus:
	case vh.signature == sigHFSX && vh.version == versionHFSX:
	default:
		return nil, corrupt("HFS+ volume header", vhOffset, "signature %#04x with version %d is not HFS+ (H+, version 4) or HFSX (HX, version 5)", vh.signature, vh.version)
	}
	if vh.blockSize == 0 || vh.blockSize%512 != 0 || vh.blockSize > maxBlockSize {
		return nil, corrupt("HFS+ volume header", vhOffset+offVHBlockSize, "allocation block size %d is not a multiple of 512 in [512, %d]", vh.blockSize, maxBlockSize)
	}
	if vh.totalBlocks == 0 {
		return nil, corrupt("HFS+ volume header", vhOffset+offVHTotalBlocks, "the volume has no allocation blocks")
	}
	if vh.nextCatalogID < firstUserCNID {
		return nil, corrupt("HFS+ volume header", vhOffset+offVHNextCNID, "next catalog node id %d is below the first user id %d", vh.nextCatalogID, firstUserCNID)
	}
	for _, c := range []struct {
		name string
		fd   *forkData
	}{{"allocation file", &vh.allocation}, {"catalog file", &vh.catalog}} {
		if c.fd.totalBlocks == 0 {
			return nil, corrupt("HFS+ volume header", vhOffset, "the %s fork has no blocks", c.name)
		}
		e := c.fd.extents[0]
		if e.count == 0 {
			return nil, corrupt("HFS+ volume header", vhOffset, "the %s fork's first extent is empty", c.name)
		}
		if end, ok := filesys.AddOK(int64(e.start), int64(e.count)); !ok || end > int64(vh.totalBlocks) {
			return nil, corrupt("HFS+ volume header", vhOffset, "the %s fork's first extent (%d+%d) is outside the %d-block volume", c.name, e.start, e.count, vh.totalBlocks)
		}
	}
	return vh, nil
}

// checkGeometry validates the five system forks against the volume: every
// inline extent lies in [0, totalBlocks), the extents of a fork add up to no
// more than its totalBlocks (the rest, if any, lives in the extents-overflow
// tree), the logical size fits the blocks, and the extents-overflow file,
// which cannot itself overflow, has exactly its blocks inline. Nothing is
// allocated or looped over beyond the 8 inline extents of each fork.
func (vh *volumeHeader) checkGeometry() error {
	forks := []struct {
		name string
		fd   *forkData
	}{
		{"allocation file", &vh.allocation},
		{"extents-overflow file", &vh.extents},
		{"catalog file", &vh.catalog},
		{"attributes file", &vh.attributesFile},
		{"startup file", &vh.startup},
	}
	for _, c := range forks {
		fd := c.fd
		if fd.totalBlocks > vh.totalBlocks {
			return corrupt("HFS+ volume header", vhOffset, "the %s fork claims %d blocks in a %d-block volume", c.name, fd.totalBlocks, vh.totalBlocks)
		}
		if fd.logicalSize > uint64(fd.totalBlocks)*uint64(vh.blockSize) { // <= 2^62: cannot overflow
			return corrupt("HFS+ volume header", vhOffset, "the %s fork has logical size %d beyond its %d blocks of %d bytes", c.name, fd.logicalSize, fd.totalBlocks, vh.blockSize)
		}
		var sum uint64
		for i, e := range fd.extents {
			if e.count == 0 {
				break
			}
			if end, ok := filesys.AddOK(int64(e.start), int64(e.count)); !ok || end > int64(vh.totalBlocks) {
				return corrupt("HFS+ volume header", vhOffset, "extent %d of the %s fork (%d+%d) is outside the %d-block volume", i, c.name, e.start, e.count, vh.totalBlocks)
			}
			sum += uint64(e.count)
		}
		if sum > uint64(fd.totalBlocks) {
			return corrupt("HFS+ volume header", vhOffset, "the extents of the %s fork add up to %d blocks, more than its %d", c.name, sum, fd.totalBlocks)
		}
		if sum == 0 && fd.totalBlocks != 0 {
			return corrupt("HFS+ volume header", vhOffset, "the %s fork claims %d blocks but has no extents", c.name, fd.totalBlocks)
		}
	}
	if _, sum := vh.extents.inline(); sum != uint64(vh.extents.totalBlocks) {
		// The extents-overflow file never has overflow records of its own.
		return corrupt("HFS+ volume header", vhOffset, "the extents-overflow file claims %d blocks but its inline extents hold %d", vh.extents.totalBlocks, sum)
	}
	return nil
}
