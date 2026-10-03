package ext4

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"math/bits"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Superblock location and size.
const (
	superOffset = 1024
	superSize   = 1024
	superMagic  = 0xEF53

	maxLogBlockSize = 6 // blocks of 1 KiB << 6 = 64 KiB
	goodOldInodeSz  = 128
	goodOldFirstIno = 11

	// maxGDTBytes bounds the group descriptor table that Open will read, so a
	// hostile block count cannot drive a huge allocation.
	maxGDTBytes = 32 << 20

	csumTypeCRC32C = 1
)

// Feature bits (compat, incompat, ro_compat words).
const (
	compatDirPrealloc  = 0x1
	compatImagic       = 0x2
	compatHasJournal   = 0x4
	compatExtAttr      = 0x8
	compatResizeInode  = 0x10
	compatDirIndex     = 0x20
	compatSparseSuper2 = 0x200

	incompatCompression = 0x1
	incompatFiletype    = 0x2
	incompatRecover     = 0x4
	incompatJournalDev  = 0x8
	incompatMetaBG      = 0x10
	incompatExtents     = 0x40
	incompat64Bit       = 0x80
	incompatMMP         = 0x100
	incompatFlexBG      = 0x200
	incompatEAInode     = 0x400
	incompatDirdata     = 0x1000
	incompatCsumSeed    = 0x2000
	incompatLargedir    = 0x4000
	incompatInlineData  = 0x8000
	incompatEncrypt     = 0x10000
	incompatCasefold    = 0x20000

	roSparseSuper  = 0x1
	roLargeFile    = 0x2
	roBtreeDir     = 0x4
	roHugeFile     = 0x8
	roGDTCsum      = 0x10
	roDirNlink     = 0x20
	roExtraIsize   = 0x40
	roQuota        = 0x100
	roBigalloc     = 0x200
	roMetadataCsum = 0x400
	incompatKnown  = incompatFiletype | incompatRecover | incompatMetaBG | incompatExtents | incompat64Bit | incompatMMP | incompatFlexBG | incompatEAInode | incompatDirdata | incompatCsumSeed | incompatLargedir | incompatInlineData | incompatEncrypt | incompatCasefold
)

type featureName struct {
	bit  uint32
	name string
}

// Names follow the e2fsprogs spelling.
var (
	compatNames = []featureName{
		{0x1, "dir_prealloc"},
		{0x2, "imagic_inodes"},
		{compatHasJournal, "has_journal"},
		{0x8, "ext_attr"},
		{0x10, "resize_inode"},
		{0x20, "dir_index"},
		{compatSparseSuper2, "sparse_super2"},
		{0x400, "fast_commit"},
		{0x800, "stable_inodes"},
		{0x1000, "orphan_file"},
	}
	incompatNames = []featureName{
		{incompatCompression, "compression"},
		{incompatFiletype, "filetype"},
		{incompatRecover, "needs_recovery"},
		{incompatJournalDev, "journal_dev"},
		{incompatMetaBG, "meta_bg"},
		{incompatExtents, "extent"},
		{incompat64Bit, "64bit"},
		{incompatMMP, "mmp"},
		{incompatFlexBG, "flex_bg"},
		{incompatEAInode, "ea_inode"},
		{incompatDirdata, "dirdata"},
		{incompatCsumSeed, "metadata_csum_seed"},
		{incompatLargedir, "large_dir"},
		{incompatInlineData, "inline_data"},
		{incompatEncrypt, "encrypt"},
		{incompatCasefold, "casefold"},
	}
	roCompatNames = []featureName{
		{roSparseSuper, "sparse_super"},
		{0x2, "large_file"},
		{0x4, "btree_dir"},
		{roHugeFile, "huge_file"},
		{roGDTCsum, "uninit_bg"},
		{roDirNlink, "dir_nlink"},
		{roExtraIsize, "extra_isize"},
		{0x80, "has_snapshot"},
		{roQuota, "quota"},
		{roBigalloc, "bigalloc"},
		{roMetadataCsum, "metadata_csum"},
		{0x800, "replica"},
		{0x1000, "read-only"},
		{0x2000, "project"},
		{0x4000, "shared_blocks"},
		{0x8000, "verity"},
	}
)

// superblock holds the validated fields of the ext2/3/4 superblock.
type superblock struct {
	inodesCount    uint32
	blocksCount    int64 // effective: the declared count clamped to the image
	declaredBlocks int64
	firstDataBlock uint32
	blockSize      int
	blocksPerGroup uint32
	inodesPerGroup uint32
	state          uint16
	revLevel       uint32
	firstIno       uint32
	inodeSize      int
	compat         uint32
	incompat       uint32
	roCompat       uint32
	uuid           [16]byte
	label          string
	descSize       int
	firstMetaBG    uint32
	backupBGs      [2]uint32
	reservedGDT    uint16 // s_reserved_gdt_blocks: descriptor blocks reserved for online resize
	checksumType   uint8
	csumSeed       uint32 // metadata_csum seed: s_checksum_seed with CSUM_SEED, else crc32c(~0, uuid)
	groups         int64  // block groups described by the declared block count
	itableBlocks   int64  // blocks in one group's inode table
}

// logicalSuperBlock is the block that holds the primary superblock: 1 for 1 KiB
// blocks (the superblock is at byte 1024), else 0. The contiguous descriptor
// table starts in the next block, as in the kernel, whatever first_data_block
// says.
func (sb *superblock) logicalSuperBlock() uint32 {
	if sb.blockSize == 1024 {
		return 1
	}
	return 0
}

func (sb *superblock) hasIncompat(f uint32) bool { return sb.incompat&f != 0 }
func (sb *superblock) hasRoCompat(f uint32) bool { return sb.roCompat&f != 0 }
func (sb *superblock) hasCompat(f uint32) bool   { return sb.compat&f != 0 }
func (sb *superblock) metadataCsum() bool        { return sb.hasRoCompat(roMetadataCsum) }
func (sb *superblock) gdtCsum() bool {
	return sb.hasRoCompat(roGDTCsum) && !sb.metadataCsum()
}

func corrupt(structure string, off int64, format string, a ...any) error {
	return &filesys.CorruptError{Structure: structure, Offset: off, Reason: fmt.Sprintf(format, a...)}
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// rawCRC32C is the kernel's crc32c_le: the CRC-32C register processed from
// seed with no pre- or post-inversion (standard CRC-32C is ^rawCRC32C(~0, p)).
// Calls chain: rawCRC32C(rawCRC32C(s, a), b) == rawCRC32C(s, a||b).
func rawCRC32C(seed uint32, p []byte) uint32 {
	return ^crc32.Update(^seed, castagnoli, p)
}

// crc16 is the kernel's crc16 (reflected polynomial 0xA001, no final xor).
func crc16(crc uint16, p []byte) uint16 {
	for _, c := range p {
		crc ^= uint16(c)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// probeSuper reports whether b (at least 0x3C bytes of a superblock) carries
// the ext magic and a sane block size.
func probeSuper(b []byte) bool {
	return len(b) >= 0x3C &&
		binary.LittleEndian.Uint16(b[0x38:]) == superMagic &&
		binary.LittleEndian.Uint32(b[0x18:]) <= maxLogBlockSize
}

// parseSuper decodes and validates the superblock b (superSize bytes) of an
// image of imgSize bytes. Problems that do not prevent reading (checksum
// mismatch, truncated image, dirty state) are returned as warnings.
func parseSuper(b []byte, imgSize int64) (*superblock, []string, error) {
	const st = "ext4 superblock"
	le := binary.LittleEndian
	var warns []string
	if len(b) < superSize {
		return nil, nil, corrupt(st, superOffset, "short superblock")
	}
	if m := le.Uint16(b[0x38:]); m != superMagic {
		return nil, nil, corrupt(st, superOffset+0x38, "bad magic %#04x, want %#04x", m, superMagic)
	}
	sb := &superblock{
		inodesCount:    le.Uint32(b[0x0:]),
		firstDataBlock: le.Uint32(b[0x14:]),
		blocksPerGroup: le.Uint32(b[0x20:]),
		inodesPerGroup: le.Uint32(b[0x28:]),
		state:          le.Uint16(b[0x3A:]),
		revLevel:       le.Uint32(b[0x4C:]),
		firstIno:       goodOldFirstIno,
		inodeSize:      goodOldInodeSz,
	}
	lbs := le.Uint32(b[0x18:])
	if lbs > maxLogBlockSize {
		return nil, nil, corrupt(st, superOffset+0x18, "log_block_size %d exceeds %d", lbs, maxLogBlockSize)
	}
	sb.blockSize = 1024 << lbs

	// Features: revision 0 has none (and no s_first_ino / s_inode_size).
	if sb.revLevel >= 1 {
		sb.compat = le.Uint32(b[0x5C:])
		sb.incompat = le.Uint32(b[0x60:])
		sb.roCompat = le.Uint32(b[0x64:])
		sb.firstIno = le.Uint32(b[0x54:])
		sb.inodeSize = int(le.Uint16(b[0x58:]))
	}
	if sb.hasIncompat(incompatJournalDev) {
		return nil, nil, fmt.Errorf("ext4: external journal device, not a filesystem: %w", filesys.ErrUnsupported)
	}
	if unknown := sb.incompat &^ (incompatKnown | incompatJournalDev); unknown != 0 {
		return nil, nil, fmt.Errorf("ext4: unknown incompatible feature bits %#x: %w", unknown, filesys.ErrUnsupported)
	}
	if sb.hasRoCompat(roBigalloc) {
		return nil, nil, fmt.Errorf("ext4: bigalloc (cluster allocation, ro_compat %#x): %w", roBigalloc, filesys.ErrUnsupported)
	}

	// Geometry. Every number is validated before it sizes anything.
	maxPerGroup := uint32(8 * sb.blockSize) // one bitmap block
	if sb.blocksPerGroup == 0 || sb.blocksPerGroup > maxPerGroup {
		return nil, nil, corrupt(st, superOffset+0x20, "blocks_per_group %d outside 1..%d", sb.blocksPerGroup, maxPerGroup)
	}
	if sb.inodesPerGroup == 0 || sb.inodesPerGroup > maxPerGroup {
		return nil, nil, corrupt(st, superOffset+0x28, "inodes_per_group %d outside 1..%d", sb.inodesPerGroup, maxPerGroup)
	}
	if sb.inodesCount == 0 {
		return nil, nil, corrupt(st, superOffset, "inodes_count is 0")
	}
	if sb.inodeSize < goodOldInodeSz || sb.inodeSize > sb.blockSize || bits.OnesCount(uint(sb.inodeSize)) != 1 {
		return nil, nil, corrupt(st, superOffset+0x58, "inode_size %d is not a power of two in %d..%d", sb.inodeSize, goodOldInodeSz, sb.blockSize)
	}
	if sb.firstIno < goodOldFirstIno || sb.firstIno > sb.inodesCount {
		return nil, nil, corrupt(st, superOffset+0x54, "first_ino %d outside %d..%d", sb.firstIno, goodOldFirstIno, sb.inodesCount)
	}
	itBytes := int64(sb.inodesPerGroup) * int64(sb.inodeSize) // <= 8192*65536, cannot overflow
	sb.itableBlocks = (itBytes + int64(sb.blockSize) - 1) / int64(sb.blockSize)
	if sb.itableBlocks >= int64(sb.blocksPerGroup) {
		return nil, nil, corrupt(st, superOffset+0x28, "inode table of %d blocks does not fit in a group of %d blocks", sb.itableBlocks, sb.blocksPerGroup)
	}

	sb.descSize = 32
	declared := uint64(le.Uint32(b[0x4:]))
	if sb.hasIncompat(incompat64Bit) {
		declared |= uint64(le.Uint32(b[0x150:])) << 32
		ds := int(le.Uint16(b[0xFE:]))
		if ds < 64 || ds > 1024 || bits.OnesCount(uint(ds)) != 1 {
			return nil, nil, corrupt(st, superOffset+0xFE, "desc_size %d is not a power of two in 64..1024", ds)
		}
		sb.descSize = ds
	}
	if declared > math.MaxInt64 {
		return nil, nil, corrupt(st, superOffset+0x4, "blocks_count %d exceeds the addressable range", declared)
	}
	sb.declaredBlocks = int64(declared)
	if sb.declaredBlocks <= int64(sb.firstDataBlock) {
		return nil, nil, corrupt(st, superOffset+0x4, "blocks_count %d does not exceed first_data_block %d", declared, sb.firstDataBlock)
	}
	span, bpg := sb.declaredBlocks-int64(sb.firstDataBlock), int64(sb.blocksPerGroup)
	groups := span / bpg // span > 0 and the sum below cannot overflow
	if span%bpg != 0 {
		groups++
	}
	table, ok := filesys.MulOK(groups, int64(sb.descSize))
	if !ok || table > maxGDTBytes {
		return nil, nil, corrupt(st, superOffset+0x4, "%d block groups of %d-byte descriptors exceed the %d-byte table limit", groups, sb.descSize, maxGDTBytes)
	}
	// A table larger than the image is not an error: a head capture or a
	// truncated image still opens, and loadGroups marks the groups whose
	// descriptors are missing as unreadable.
	sb.groups = groups

	// Clamp to what the image actually holds.
	sb.blocksCount = sb.declaredBlocks
	if have := imgSize / int64(sb.blockSize); have < sb.blocksCount {
		sb.blocksCount = have
		warns = append(warns, fmt.Sprintf("truncated image: the superblock declares %d blocks of %d bytes but the image holds %d", sb.declaredBlocks, sb.blockSize, have))
	}
	if sb.blocksCount <= int64(sb.firstDataBlock) {
		return nil, nil, corrupt(st, superOffset+0x14, "the image holds %d blocks, no more than first_data_block %d", sb.blocksCount, sb.firstDataBlock)
	}
	if want, ok := filesys.MulOK(groups, int64(sb.inodesPerGroup)); !ok || want != int64(sb.inodesCount) {
		warns = append(warns, fmt.Sprintf("inode count %d does not match %d block groups of %d inodes", sb.inodesCount, groups, sb.inodesPerGroup))
	}

	copy(sb.uuid[:], b[0x68:0x78])
	sb.label = cString(b[0x78:0x88])
	sb.reservedGDT = le.Uint16(b[0xCE:])
	sb.firstMetaBG = le.Uint32(b[0x104:])
	sb.backupBGs = [2]uint32{le.Uint32(b[0x24C:]), le.Uint32(b[0x250:])}
	sb.checksumType = b[0x175]
	sb.csumSeed = rawCRC32C(0xFFFFFFFF, sb.uuid[:])
	if sb.hasIncompat(incompatCsumSeed) {
		sb.csumSeed = le.Uint32(b[0x270:])
	}

	if sb.metadataCsum() {
		if sb.checksumType != csumTypeCRC32C {
			warns = append(warns, fmt.Sprintf("unsupported superblock checksum type %d", sb.checksumType))
		} else if stored, want := le.Uint32(b[0x3FC:]), rawCRC32C(0xFFFFFFFF, b[:0x3FC]); stored != want {
			warns = append(warns, fmt.Sprintf("superblock checksum mismatch: stored %#08x, computed %#08x", stored, want))
		}
	}
	if sb.revLevel > 1 {
		warns = append(warns, fmt.Sprintf("unknown revision level %d", sb.revLevel))
	}
	if want := sb.logicalSuperBlock(); sb.firstDataBlock != want {
		warns = append(warns, fmt.Sprintf("first_data_block is %d, expected %d for %d-byte blocks; the descriptor table is read from block %d", sb.firstDataBlock, want, sb.blockSize, want+1))
	}
	if sb.hasIncompat(incompatRecover) {
		warns = append(warns, "the journal needs recovery and has not been replayed: the filesystem may be inconsistent")
	}
	if sb.state&0x2 != 0 {
		warns = append(warns, "the superblock records filesystem errors")
	}
	return sb, warns, nil
}

// cString returns b up to the first NUL.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Feature sets of the older generations. A filesystem is ext2 or ext3 only when
// every set bit is in the generation's whitelist (so a feature this reader has
// never heard of makes it ext4, not ext2).
const (
	ext2Compat   = compatDirPrealloc | compatImagic | compatExtAttr | compatResizeInode | compatDirIndex
	ext2Incompat = incompatFiletype
	ext2Ro       = roSparseSuper | roLargeFile | roBtreeDir

	ext3Compat   = compatHasJournal | compatExtAttr | compatResizeInode | compatDirIndex | compatDirPrealloc
	ext3Incompat = incompatFiletype | incompatRecover | incompatMetaBG
	ext3Ro       = ext2Ro
)

// fsType names the filesystem generation from its feature set: ext2 without a
// journal and with only ext2 features, ext3 with a journal and only ext3
// features, ext4 otherwise.
func (sb *superblock) fsType() string {
	switch {
	case !sb.hasCompat(compatHasJournal) &&
		sb.compat&^ext2Compat == 0 && sb.incompat&^ext2Incompat == 0 && sb.roCompat&^ext2Ro == 0:
		return "ext2"
	case sb.hasCompat(compatHasJournal) &&
		sb.compat&^ext3Compat == 0 && sb.incompat&^ext3Incompat == 0 && sb.roCompat&^ext3Ro == 0:
		return "ext3"
	default:
		return "ext4"
	}
}

func (sb *superblock) featureNames() []string {
	var out []string
	add := func(word uint32, names []featureName, label string) {
		for _, n := range names {
			if word&n.bit != 0 {
				out = append(out, n.name)
				word &^= n.bit
			}
		}
		if word != 0 {
			out = append(out, fmt.Sprintf("%s:%#x", label, word))
		}
	}
	add(sb.compat, compatNames, "compat")
	add(sb.incompat, incompatNames, "incompat")
	add(sb.roCompat, roCompatNames, "ro_compat")
	return out
}

func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
