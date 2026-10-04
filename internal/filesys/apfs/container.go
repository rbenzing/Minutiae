package apfs

import (
	"fmt"
	"math"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// nx_superblock_t layout (Apple File System Reference; offsets computed from
// the field order).
const (
	nxMagic = "NXSB" // 'BSXN' read as a little-endian u32

	minBlockSize = 4096
	maxBlockSize = 65536

	nxMaxFileSystems = 100 // NX_MAX_FILE_SYSTEMS

	// maxAreaBlocks caps nx_xp_desc_blocks and nx_xp_data_blocks.
	maxAreaBlocks = 1 << 20

	// The high bit of an area block count marks a B-tree of fragments.
	areaNonContiguous = 0x80000000

	// nx_incompatible_features
	incompatVersion1 = 0x1
	incompatVersion2 = 0x2
	incompatFusion   = 0x100
	knownIncompat    = incompatVersion1 | incompatVersion2 | incompatFusion

	// nx_features
	featDefrag = 0x1
	featLCFD   = 0x2

	// nx_flags
	nxFlagCryptoSW = 0x4
)

// nxSuper is the decoded nx_superblock_t of the selected checkpoint.
type nxSuper struct {
	xid                  uint64
	blockSize            uint32
	blockCount           uint64
	features             uint64
	roCompat             uint64
	incompat             uint64
	uuid                 [16]byte
	nextOid, nextXid     uint64
	descBlocks           uint32
	dataBlocks           uint32
	descBase, dataBase   uint64
	descNext, dataNext   uint32
	descIndex, descLen   uint32
	dataIndex, dataLen   uint32
	spacemanOid, omapOid uint64
	reaperOid            uint64
	maxFS                uint32
	flags                uint64
	fsOid                [nxMaxFileSystems]uint64
}

// parseNX decodes a superblock block (at least 1352 bytes: through nx_test_oid; the rest of the block is unused).
func parseNX(b []byte) nxSuper {
	n := nxSuper{
		xid:         le.Uint64(b[16:]),
		blockSize:   le.Uint32(b[36:]),
		blockCount:  le.Uint64(b[40:]),
		features:    le.Uint64(b[48:]),
		roCompat:    le.Uint64(b[56:]),
		incompat:    le.Uint64(b[64:]),
		nextOid:     le.Uint64(b[88:]),
		nextXid:     le.Uint64(b[96:]),
		descBlocks:  le.Uint32(b[104:]),
		dataBlocks:  le.Uint32(b[108:]),
		descBase:    le.Uint64(b[112:]),
		dataBase:    le.Uint64(b[120:]),
		descNext:    le.Uint32(b[128:]),
		dataNext:    le.Uint32(b[132:]),
		descIndex:   le.Uint32(b[136:]),
		descLen:     le.Uint32(b[140:]),
		dataIndex:   le.Uint32(b[144:]),
		dataLen:     le.Uint32(b[148:]),
		spacemanOid: le.Uint64(b[152:]),
		omapOid:     le.Uint64(b[160:]),
		reaperOid:   le.Uint64(b[168:]),
		maxFS:       le.Uint32(b[180:]),
		flags:       le.Uint64(b[1264:]),
	}
	copy(n.uuid[:], b[72:88])
	for i := range n.fsOid {
		n.fsOid[i] = le.Uint64(b[184+8*i:])
	}
	return n
}

func validBlockSize(bs uint32) bool {
	return bs >= minBlockSize && bs <= maxBlockSize && bs&(bs-1) == 0
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("apfs: %w: %s", filesys.ErrUnsupported, fmt.Sprintf(format, a...))
}

// validate checks the geometry a superblock claims before anything is
// allocated or looped over on its account: block size, block count, the two
// checkpoint areas (contiguous, bounded, inside the container, disjoint, not
// block 0) and the checkpoint ring cursor.
func (n *nxSuper) validate() error {
	if n.descBlocks&areaNonContiguous != 0 || n.dataBlocks&areaNonContiguous != 0 {
		return unsupported("non-contiguous checkpoint area (a B-tree of fragments)")
	}
	if !validBlockSize(n.blockSize) {
		return corrupt("container superblock", 36, "block size %d is not a power of two in [%d, %d]", n.blockSize, minBlockSize, maxBlockSize)
	}
	if n.blockCount == 0 || n.blockCount > math.MaxInt64 {
		return corrupt("container superblock", 40, "block count %d is unusable", n.blockCount)
	}
	if _, ok := filesys.MulOK(int64(n.blockCount), int64(n.blockSize)); !ok {
		return corrupt("container superblock", 40, "block count %d x block size %d overflows", n.blockCount, n.blockSize)
	}
	if n.descBlocks == 0 || n.descBlocks > maxAreaBlocks {
		return corrupt("container superblock", 104, "descriptor area of %d blocks is not in [1, %d]", n.descBlocks, maxAreaBlocks)
	}
	if n.dataBlocks == 0 || n.dataBlocks > maxAreaBlocks {
		return corrupt("container superblock", 108, "data area of %d blocks is not in [1, %d]", n.dataBlocks, maxAreaBlocks)
	}
	dEnd, ok1 := addU64(n.descBase, uint64(n.descBlocks))
	aEnd, ok2 := addU64(n.dataBase, uint64(n.dataBlocks))
	if n.descBase == 0 || !ok1 || dEnd > n.blockCount {
		return corrupt("container superblock", 112, "descriptor area [%d, +%d) is not inside the container of %d blocks", n.descBase, n.descBlocks, n.blockCount)
	}
	if n.dataBase == 0 || !ok2 || aEnd > n.blockCount {
		return corrupt("container superblock", 120, "data area [%d, +%d) is not inside the container of %d blocks", n.dataBase, n.dataBlocks, n.blockCount)
	}
	if n.descBase < aEnd && n.dataBase < dEnd {
		return corrupt("container superblock", 112, "descriptor area [%d, %d) overlaps the data area [%d, %d)", n.descBase, dEnd, n.dataBase, aEnd)
	}
	if n.descLen == 0 || n.descLen > n.descBlocks {
		return corrupt("container superblock", 140, "descriptor length %d is not in [1, %d]", n.descLen, n.descBlocks)
	}
	if n.descIndex >= n.descBlocks {
		return corrupt("container superblock", 136, "descriptor index %d is beyond the ring of %d blocks", n.descIndex, n.descBlocks)
	}
	if n.maxFS > nxMaxFileSystems {
		return corrupt("container superblock", 180, "%d file systems exceeds the maximum of %d", n.maxFS, nxMaxFileSystems)
	}
	return nil
}

func addU64(a, b uint64) (uint64, bool) {
	s := a + b
	return s, s >= a
}

// features names the container feature bits.
func (n *nxSuper) featureNames() []string {
	var out []string
	if n.incompat&incompatVersion2 != 0 {
		out = append(out, "version2")
	}
	if n.features&featDefrag != 0 {
		out = append(out, "defrag")
	}
	if n.features&featLCFD != 0 {
		out = append(out, "lcfd")
	}
	if n.flags&nxFlagCryptoSW != 0 {
		out = append(out, "crypto-sw")
	}
	if u := n.features &^ (featDefrag | featLCFD); u != 0 {
		out = append(out, fmt.Sprintf("features=%#x", u))
	}
	if n.roCompat != 0 {
		out = append(out, fmt.Sprintf("ro-compat=%#x", n.roCompat))
	}
	return out
}

func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
