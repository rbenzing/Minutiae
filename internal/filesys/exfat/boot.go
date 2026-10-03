package exfat

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Boot sector layout (offsets in bytes).
const (
	bootSize       = 512
	offOEM         = 3
	offVolLength   = 72
	offFatOffset   = 80
	offFatLength   = 84
	offHeapOffset  = 88
	offClusterCnt  = 92
	offRootCluster = 96
	offSerial      = 100
	offRevision    = 104
	offVolFlags    = 106
	offBpsShift    = 108
	offSpcShift    = 109
	offNumFats     = 110
	offPercentUsed = 112

	oemName = "EXFAT   "

	minSectorShift  = 9
	maxSectorShift  = 12
	maxClusterShift = 25 // BytesPerSectorShift + SectorsPerClusterShift: clusters of at most 32 MiB

	// maxClusterCount is the largest ClusterCount the format allows
	// (0xFFFFFFF5); the cluster numbers above it are reserved.
	maxClusterCount = 0xFFFFFFF5

	fatReservedClusters = 2
	bootRegionSectors   = 12 // sectors 0-10 hold the data, sector 11 the checksum
)

type boot struct {
	volLength    uint64 // sectors
	fatOffset    uint32 // sectors
	fatLength    uint32 // sectors
	heapOffset   uint32 // sectors
	clusterCount uint32
	rootCluster  uint32
	serial       uint32
	revision     uint16
	volFlags     uint16
	bpsShift     uint8
	spcShift     uint8
	numFats      uint8
}

// shiftsOK reports whether the two shifts describe a legal geometry.
func shiftsOK(bps, spc uint8) bool {
	return bps >= minSectorShift && bps <= maxSectorShift && int(bps)+int(spc) <= maxClusterShift
}

// probeBoot reports whether b (the first sector) names an exFAT volume with
// legal shifts.
func probeBoot(b []byte) bool {
	return len(b) >= bootSize && string(b[offOEM:offOEM+len(oemName)]) == oemName && shiftsOK(b[offBpsShift], b[offSpcShift])
}

// parseBoot decodes the main boot sector. It checks the fields that decide
// how everything else is interpreted; the geometry against the image size is
// checked by Open.
func parseBoot(b []byte) (*boot, error) {
	if len(b) < bootSize {
		return nil, corrupt("exFAT boot sector", 0, "%d bytes is too small for a boot sector", len(b))
	}
	if string(b[offOEM:offOEM+len(oemName)]) != oemName {
		return nil, corrupt("exFAT boot sector", offOEM, "FileSystemName is %q, not %q", b[offOEM:offOEM+len(oemName)], oemName)
	}
	le := binary.LittleEndian
	bt := &boot{
		volLength:    le.Uint64(b[offVolLength:]),
		fatOffset:    le.Uint32(b[offFatOffset:]),
		fatLength:    le.Uint32(b[offFatLength:]),
		heapOffset:   le.Uint32(b[offHeapOffset:]),
		clusterCount: le.Uint32(b[offClusterCnt:]),
		rootCluster:  le.Uint32(b[offRootCluster:]),
		serial:       le.Uint32(b[offSerial:]),
		revision:     le.Uint16(b[offRevision:]),
		volFlags:     le.Uint16(b[offVolFlags:]),
		bpsShift:     b[offBpsShift],
		spcShift:     b[offSpcShift],
		numFats:      b[offNumFats],
	}
	if !shiftsOK(bt.bpsShift, bt.spcShift) {
		return nil, corrupt("exFAT boot sector", offBpsShift, "BytesPerSectorShift %d and SectorsPerClusterShift %d are out of range (sector shift 9-12, sum at most 25)", bt.bpsShift, bt.spcShift)
	}
	if bt.numFats != 1 && bt.numFats != 2 {
		return nil, corrupt("exFAT boot sector", offNumFats, "NumberOfFats is %d, not 1 or 2", bt.numFats)
	}
	return bt, nil
}

// bootChecksum is the boot region checksum over the 11 sectors in region:
// VolumeFlags and PercentInUse change at run time and are skipped.
func bootChecksum(region []byte) uint32 {
	var sum uint32
	for i, c := range region {
		if i == offVolFlags || i == offVolFlags+1 || i == offPercentUsed {
			continue
		}
		sum = (sum&1)<<31 | sum>>1
		sum += uint32(c)
	}
	return sum
}

// checkBootChecksum compares the checksum sector (the 12th sector) with the
// checksum of the first 11. region holds all twelve sectors.
func checkBootChecksum(region []byte, ss int) (stored, want uint32, ok bool) {
	want = bootChecksum(region[:(bootRegionSectors-1)*ss])
	sector := region[(bootRegionSectors-1)*ss : bootRegionSectors*ss]
	for i := 0; i+4 <= len(sector); i += 4 {
		if v := binary.LittleEndian.Uint32(sector[i:]); v != want {
			return v, want, false
		}
	}
	return want, want, true
}

// geometry is the validated layout of a volume.
type geometry struct {
	ss, cs       int64 // bytes per sector and per cluster
	size         int64 // volume size in bytes (VolumeLength clamped to the image)
	heapOff      int64 // byte offset of cluster 2
	fatOff       int64 // byte offset of the first FAT
	fatEntries   uint32
	clusterCount uint32
	rootCluster  uint32
	warnings     []string
}

// layout validates the boot sector fields against the image size and returns
// the geometry the reader uses. A volume that claims more than the image holds
// is clamped (with a warning); one that cannot be laid out is corrupt.
func (bt *boot) layout(imageSize int64) (*geometry, error) {
	g := &geometry{ss: 1 << bt.bpsShift}
	g.cs = g.ss << bt.spcShift // at most 32 MiB
	if bt.volLength == 0 {
		return nil, corrupt("exFAT boot sector", offVolLength, "VolumeLength is 0")
	}
	declared := int64(math.MaxInt64)
	if bt.volLength <= uint64(math.MaxInt64/g.ss) {
		declared = int64(bt.volLength) * g.ss
	}
	g.size = min(declared, imageSize)
	if declared > imageSize {
		g.warnings = append(g.warnings, fmt.Sprintf("VolumeLength is %d sectors (%d bytes) but the image holds %d bytes; the volume is read as far as the image goes", bt.volLength, declared, imageSize))
	}
	if bt.fatOffset == 0 || bt.fatLength == 0 {
		return nil, corrupt("exFAT boot sector", offFatOffset, "FatOffset %d / FatLength %d: the FAT must exist and follow the boot sector", bt.fatOffset, bt.fatLength)
	}
	if bt.fatOffset < 24 {
		g.warnings = append(g.warnings, fmt.Sprintf("FatOffset is %d sectors, below the 24 sectors of the boot regions", bt.fatOffset))
	}
	fatEnd := uint64(bt.fatOffset) + uint64(bt.fatLength)*uint64(bt.numFats) // cannot overflow: 33 bits
	if fatEnd > uint64(bt.heapOffset) {
		return nil, corrupt("exFAT boot sector", offHeapOffset, "ClusterHeapOffset %d lies inside the FAT region (ends at sector %d)", bt.heapOffset, fatEnd)
	}
	g.heapOff = int64(bt.heapOffset) * g.ss // < 2^44
	g.fatOff = int64(bt.fatOffset) * g.ss
	if g.heapOff >= g.size {
		return nil, corrupt("exFAT boot sector", offHeapOffset, "the cluster heap (byte %d) lies beyond the %d-byte volume", g.heapOff, g.size)
	}
	if bt.clusterCount == 0 {
		return nil, corrupt("exFAT boot sector", offClusterCnt, "ClusterCount is 0")
	}
	cc := uint64(bt.clusterCount)
	fits := uint64(g.size-g.heapOff) / uint64(g.cs)
	limit := min(fits, maxClusterCount)
	if cc > limit {
		g.warnings = append(g.warnings, fmt.Sprintf("ClusterCount is %d but only %d clusters fit in the volume; reading %d", cc, limit, limit))
		cc = limit
	}
	if cc == 0 {
		return nil, corrupt("exFAT boot sector", offClusterCnt, "no complete cluster fits in the volume")
	}
	g.clusterCount = uint32(cc)
	g.rootCluster = bt.rootCluster
	if g.rootCluster < fatReservedClusters || uint64(g.rootCluster-fatReservedClusters) >= cc {
		return nil, corrupt("exFAT boot sector", offRootCluster, "FirstClusterOfRootDirectory %d is not a cluster of the volume (2-%d)", g.rootCluster, cc+1)
	}
	fatBytes := int64(bt.fatLength) * g.ss // < 2^44
	need := cc + fatReservedClusters
	have := uint64(fatBytes / 4)
	g.fatEntries = uint32(min(have, need))
	if have < need {
		g.warnings = append(g.warnings, fmt.Sprintf("the FAT holds %d entries but %d clusters need %d; clusters beyond it cannot be followed", have, cc, need))
	}
	return g, nil
}
