package f2fs

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"unicode/utf16"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Fixed geometry. This reader supports only the layout every real F2FS volume
// uses: 4 KiB blocks and 512 blocks (2 MiB) per segment.
const (
	blockSize    = 4096
	blockShift   = 12
	segShift     = 9
	blocksPerSeg = 1 << segShift

	// The superblock is 3072 bytes at byte 1024 of block 0, with a backup at
	// byte 1024 of block 1. (f2fs_super_block ends exactly at the end of the
	// 4 KiB block: 1024 + 3072 = 4096.)
	superOffset = 1024
	superSize   = 3072
	backupBlock = 1
	minImage    = 2 * blockSize // both superblock copies must fit

	superMagic = 0xF2F52010

	// maxCPPayload bounds sb.cp_payload (extra checkpoint blocks that hold
	// bitmaps): a pack is at most one segment, so the payload plus the header,
	// summaries and the trailing copy must fit in 512 blocks.
	maxCPPayload = blocksPerSeg - 2

	// maxBlockAddr: F2FS block addresses are 32-bit.
	maxBlockAddr = 1 << 32
)

// Superblock field offsets, relative to the start of the superblock
// (byte 1024 of the block). Derived from struct f2fs_super_block in the
// kernel's include/linux/f2fs_fs.h:
//
//	magic u32 @0, major_ver u16 @4, minor_ver u16 @6, log_sectorsize @8,
//	log_sectors_per_block @12, log_blocksize @16, log_blocks_per_seg @20,
//	segs_per_sec @24, secs_per_zone @28, checksum_offset @32, block_count u64
//	@36, section_count @44, segment_count @48, segment_count_{ckpt,sit,nat,
//	ssa,main} @52..68, segment0_blkaddr @72, {cp,sit,nat,ssa,main}_blkaddr
//	@76..92, root_ino @96, node_ino @100, meta_ino @104, uuid[16] @108,
//	volume_name[512] u16 @124 (to 1148), extension_count u32 @1148,
//	extension_list[64][8] @1152 (to 1664), cp_payload u32 @1664,
//	version[256] @1668, init_version[256] @1924, feature u32 @2180 (the field
//	after init_version), ... crc u32 @3068 (the last field; sizeof = 3072; it is reached through
//
// checksum_offset, which mkfs sets to 3068).
//
// The feature offset follows from that order: 124 + 2*512 (volume_name) = 1148;
// + 4 (extension_count) + 64*8 (extension_list) = 1664; + 4 (cp_payload) = 1668;
// + 2*256 (version, init_version) = 2180.
const (
	sbMagic          = 0
	sbMajorVer       = 4
	sbLogSectorSize  = 8
	sbLogSecPerBlock = 12
	sbLogBlockSize   = 16
	sbLogBlocksPSeg  = 20
	sbSegsPerSec     = 24
	sbSecsPerZone    = 28
	sbChecksumOffset = 32
	sbBlockCount     = 36
	sbSectionCount   = 44
	sbSegmentCount   = 48
	sbSegCkpt        = 52
	sbSegSIT         = 56
	sbSegNAT         = 60
	sbSegSSA         = 64
	sbSegMain        = 68
	sbSeg0Addr       = 72
	sbCPAddr         = 76
	sbSITAddr        = 80
	sbNATAddr        = 84
	sbSSAAddr        = 88
	sbMainAddr       = 92
	sbRootIno        = 96
	sbNodeIno        = 100
	sbMetaIno        = 104
	sbUUID           = 108
	sbVolumeName     = 124
	sbCPPayload      = 1664
	sbFeature        = 2180
)

// Feature bits (sb.feature).
const (
	featEncrypt       = 0x1
	featBlkzoned      = 0x2
	featAtomicWrite   = 0x4
	featExtraAttr     = 0x8
	featPrjQuota      = 0x10
	featInodeChksum   = 0x20
	featFlexInlineXat = 0x40
	featQuotaIno      = 0x80
	featInodeCrtime   = 0x100
	featLostFound     = 0x200
	featVerity        = 0x400
	featSBChksum      = 0x800
	featCasefold      = 0x1000
	featCompression   = 0x2000
	featRO            = 0x4000
)

// featureNames follow the mkfs spelling of the -O option.
var featureNames = []struct {
	bit  uint32
	name string
}{
	{featEncrypt, "encrypt"},
	{featBlkzoned, "blkzoned"},
	{featAtomicWrite, "atomic_write"},
	{featExtraAttr, "extra_attr"},
	{featPrjQuota, "project_quota"},
	{featInodeChksum, "inode_checksum"},
	{featFlexInlineXat, "flexible_inline_xattr"},
	{featQuotaIno, "quota"},
	{featInodeCrtime, "inode_crtime"},
	{featLostFound, "lost_found"},
	{featVerity, "verity"},
	{featSBChksum, "sb_checksum"},
	{featCasefold, "casefold"},
	{featCompression, "compression"},
	{featRO, "ro"},
}

// superblock holds the validated fields of one superblock copy.
type superblock struct {
	feature      uint32
	blockCount   uint64 // as declared
	segmentCount uint32
	sectionCount uint32
	segsPerSec   uint32
	secsPerZone  uint32

	segCkpt, segSIT, segNAT, segSSA, segMain uint32

	seg0Addr, cpAddr, sitAddr, natAddr, ssaAddr, mainAddr uint32

	rootIno, nodeIno, metaIno uint32
	cpPayload                 uint32
	uuid                      [16]byte
	label                     string

	// blocks is the number of blocks the image actually holds, at most
	// blockCount.
	blocks int64
}

func (sb *superblock) has(f uint32) bool { return sb.feature&f != 0 }

// natPairs and sitPairs are the number of segment pairs (two copies each) in
// the NAT and SIT areas.
func (sb *superblock) natPairs() uint32 { return sb.segNAT / 2 }
func (sb *superblock) sitPairs() uint32 { return sb.segSIT / 2 }

func corrupt(structure string, off int64, format string, a ...any) error {
	return &filesys.CorruptError{Structure: structure, Offset: off, Reason: fmt.Sprintf(format, a...)}
}

// rawCRC32 is the kernel's crc32_le as used by f2fs_crc32: the CRC-32 (IEEE,
// reflected) register run from seed with NO initial or final inversion
// (standard CRC-32 is ^rawCRC32(^0, p)). Calls chain:
// rawCRC32(rawCRC32(s, a), b) == rawCRC32(s, a||b).
//
// Derivation: the kernel defines f2fs_crc32(sbi, addr, len) as
// crc32_le(F2FS_SUPER_MAGIC, addr, len), and __f2fs_crc32(sbi, crc, addr, len)
// as crc32_le(crc, addr, len); crc32_le is the table-driven reflected
// algorithm with no inversion. Go's crc32.Update inverts on entry and exit,
// so the inversions are undone around it.
func rawCRC32(seed uint32, p []byte) uint32 {
	return ^crc32.Update(^seed, crc32.IEEETable, p)
}

// parseSuper decodes and validates one superblock copy b (superSize bytes)
// found at absolute byte offset base. A copy that fails any check is an
// error; the caller then tries the other copy.
func parseSuper(b []byte, base int64) (*superblock, error) {
	const st = "f2fs superblock"
	le := binary.LittleEndian
	if len(b) < superSize {
		return nil, corrupt(st, base, "short superblock")
	}
	u32 := func(off int) uint32 { return le.Uint32(b[off:]) }
	if m := u32(sbMagic); m != superMagic {
		return nil, corrupt(st, base+sbMagic, "bad magic %#08x, want %#08x", m, uint32(superMagic))
	}
	if v := le.Uint16(b[sbMajorVer:]); v != 1 {
		return nil, corrupt(st, base+sbMajorVer, "major_ver %d, want 1", v)
	}
	sb := &superblock{
		feature:      u32(sbFeature),
		segmentCount: u32(sbSegmentCount),
		sectionCount: u32(sbSectionCount),
		segsPerSec:   u32(sbSegsPerSec),
		secsPerZone:  u32(sbSecsPerZone),
		segCkpt:      u32(sbSegCkpt),
		segSIT:       u32(sbSegSIT),
		segNAT:       u32(sbSegNAT),
		segSSA:       u32(sbSegSSA),
		segMain:      u32(sbSegMain),
		seg0Addr:     u32(sbSeg0Addr),
		cpAddr:       u32(sbCPAddr),
		sitAddr:      u32(sbSITAddr),
		natAddr:      u32(sbNATAddr),
		ssaAddr:      u32(sbSSAAddr),
		mainAddr:     u32(sbMainAddr),
		rootIno:      u32(sbRootIno),
		nodeIno:      u32(sbNodeIno),
		metaIno:      u32(sbMetaIno),
		cpPayload:    u32(sbCPPayload),
		blockCount:   le.Uint64(b[sbBlockCount:]),
	}

	// Checksum of this copy: f2fs_crc32(F2FS_SUPER_MAGIC, sb[:checksum_offset])
	// stored at checksum_offset (the kernel insists it is offsetof(crc), 3068;
	// any offset inside the structure is accepted here).
	if sb.has(featSBChksum) {
		off := u32(sbChecksumOffset)
		if off > superSize-4 {
			return nil, corrupt(st, base+sbChecksumOffset, "checksum_offset %d lies beyond the %d-byte superblock", off, superSize)
		}
		if stored, want := le.Uint32(b[off:]), rawCRC32(superMagic, b[:off]); stored != want {
			return nil, corrupt(st, base+int64(off), "superblock checksum mismatch: stored %#08x, computed %#08x", stored, want)
		}
	}

	// Fixed geometry.
	if v := u32(sbLogBlockSize); v != blockShift {
		return nil, corrupt(st, base+sbLogBlockSize, "log_blocksize %d, only %d (4 KiB blocks) is supported", v, blockShift)
	}
	if v := u32(sbLogBlocksPSeg); v != segShift {
		return nil, corrupt(st, base+sbLogBlocksPSeg, "log_blocks_per_seg %d, only %d (512 blocks per segment) is supported", v, segShift)
	}
	ls, lspb := u32(sbLogSectorSize), u32(sbLogSecPerBlock)
	if ls < 9 || ls > blockShift || lspb != blockShift-ls {
		return nil, corrupt(st, base+sbLogSectorSize, "log_sectorsize %d and log_sectors_per_block %d are inconsistent with 4 KiB blocks", ls, lspb)
	}
	if sb.segsPerSec == 0 || sb.secsPerZone == 0 || sb.sectionCount == 0 {
		return nil, corrupt(st, base+sbSegsPerSec, "segs_per_sec %d, secs_per_zone %d and section_count %d must all be non-zero", sb.segsPerSec, sb.secsPerZone, sb.sectionCount)
	}
	if sb.blockCount > maxBlockAddr {
		return nil, corrupt(st, base+sbBlockCount, "block_count %d exceeds the 32-bit block address space", sb.blockCount)
	}
	if uint64(sb.segmentCount) > sb.blockCount>>segShift {
		return nil, corrupt(st, base+sbSegmentCount, "segment_count %d exceeds block_count %d / %d", sb.segmentCount, sb.blockCount, blocksPerSeg)
	}
	if sb.segmentCount/sb.segsPerSec < sb.sectionCount {
		return nil, corrupt(st, base+sbSectionCount, "section_count %d does not fit in %d segments of %d per section", sb.sectionCount, sb.segmentCount, sb.segsPerSec)
	}

	// The metadata areas must be in order and must not overlap, each inside
	// block_count. Ends are computed in uint64: 2^32 segments * 512 cannot
	// overflow it.
	if sb.seg0Addr < 2 {
		return nil, corrupt(st, base+sbSeg0Addr, "segment0_blkaddr %d overlaps the superblock copies", sb.seg0Addr)
	}
	prev := uint64(sb.seg0Addr)
	for _, a := range []struct {
		name       string
		off        int
		start, seg uint32
		minSegs    uint32
	}{
		{"checkpoint", sbCPAddr, sb.cpAddr, sb.segCkpt, 2}, // two packs, one segment each
		{"SIT", sbSITAddr, sb.sitAddr, sb.segSIT, 2},       // at least one pair
		{"NAT", sbNATAddr, sb.natAddr, sb.segNAT, 2},
		{"SSA", sbSSAAddr, sb.ssaAddr, sb.segSSA, 1},
		{"main", sbMainAddr, sb.mainAddr, sb.segMain, 1},
	} {
		if a.seg < a.minSegs {
			return nil, corrupt(st, base+int64(a.off), "%s area has %d segments, need at least %d", a.name, a.seg, a.minSegs)
		}
		if uint64(a.start) < prev {
			return nil, corrupt(st, base+int64(a.off), "%s area at block %d overlaps or precedes the previous area (ends at block %d)", a.name, a.start, prev)
		}
		prev = uint64(a.start) + uint64(a.seg)<<segShift
		if prev > sb.blockCount {
			return nil, corrupt(st, base+int64(a.off), "%s area (blocks %d..%d) extends beyond block_count %d", a.name, a.start, prev, sb.blockCount)
		}
	}
	if sb.cpPayload > maxCPPayload {
		return nil, corrupt(st, base+sbCPPayload, "cp_payload %d exceeds %d", sb.cpPayload, maxCPPayload)
	}
	if sb.rootIno == 0 || sb.nodeIno == 0 || sb.metaIno == 0 ||
		sb.rootIno == sb.nodeIno || sb.rootIno == sb.metaIno || sb.nodeIno == sb.metaIno {
		return nil, corrupt(st, base+sbRootIno, "root_ino %d, node_ino %d and meta_ino %d must be distinct and non-zero", sb.rootIno, sb.nodeIno, sb.metaIno)
	}

	copy(sb.uuid[:], b[sbUUID:sbUUID+16])
	sb.label = decodeLabel(b[sbVolumeName : sbVolumeName+1024])
	return sb, nil
}

// decodeLabel decodes the NUL-terminated UTF-16LE volume name; unpaired
// surrogates become U+FFFD.
func decodeLabel(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// featureList names the set feature bits (unknown ones as "feature:0x..").
func (sb *superblock) featureList() []string {
	var out []string
	rest := sb.feature
	for _, n := range featureNames {
		if rest&n.bit != 0 {
			out = append(out, n.name)
			rest &^= n.bit
		}
	}
	if rest != 0 {
		out = append(out, fmt.Sprintf("feature:%#x", rest))
	}
	return out
}

func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
