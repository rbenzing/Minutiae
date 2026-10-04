package f2fs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Checkpoint field offsets (struct f2fs_checkpoint, include/linux/f2fs_fs.h).
const (
	cpVersion        = 0
	cpUserBlocks     = 8
	cpValidBlocks    = 16
	cpRsvdSegs       = 24
	cpOverprovSegs   = 28
	cpFreeSegs       = 32
	cpFlags          = 132
	cpPackBlocks     = 136
	cpStartSum       = 140
	cpValidNodes     = 144
	cpValidInodes    = 148
	cpNextFreeNid    = 152
	cpSITBitmapBytes = 156
	cpNATBitmapBytes = 160
	cpChecksumOffset = 164
	cpBitmap         = 192 // sit_nat_version_bitmap

	// cpMinChksumOffset is the smallest legal checksum_offset (the start of
	// the bitmap area); cpMaxChksumOffset the largest (the last word of the
	// block).
	cpMinChksumOffset = cpBitmap
	cpMaxChksumOffset = blockSize - 4
)

// Checkpoint flags (ckpt_flags).
const (
	cpUmountFlag      = 0x1    // CP_UMOUNT_FLAG
	cpOrphanFlag      = 0x2    // CP_ORPHAN_PRESENT_FLAG
	cpErrorFlag       = 0x8    // CP_ERROR_FLAG
	cpFsckFlag        = 0x10   // CP_FSCK_FLAG
	cpLargeNATBitmapF = 0x400  // CP_LARGE_NAT_BITMAP_FLAG
	cpDisabledFlag    = 0x1000 // CP_DISABLED_FLAG
)

// checkpoint is one validated checkpoint pack.
type checkpoint struct {
	pack  int    // 1 or 2
	addr  uint32 // first block of the pack
	ver   uint64 // checkpoint_ver
	flags uint32

	userBlocks, validBlocks              uint64
	rsvdSegs, overprovSegs, freeSegs     uint32
	validNodes, validInodes, nextFreeNid uint32
	packBlocks                           uint32 // cp_pack_total_block_count
	startSum                             uint32 // pack-relative block of the first data summary
	sitBitmap, natBitmap                 []byte // version bitmaps (which copy of each SIT/NAT block is current)
	sitBitmapBytes, natBitmapBytes       uint32
	pack2Blank                           bool // pack 2 was never written (normal after mkfs)
}

// warnings lists what the checkpoint's flags say about the volume's state.
func (cp *checkpoint) warnings() []string {
	var w []string
	if cp.flags&cpUmountFlag == 0 {
		w = append(w, "last checkpoint is not an unmount checkpoint (data written after it is not reflected)")
	}
	if cp.flags&cpOrphanFlag != 0 {
		w = append(w, "the checkpoint lists orphan inodes (CP_ORPHAN_PRESENT_FLAG): files that were unlinked but still open")
	}
	if cp.flags&cpDisabledFlag != 0 {
		w = append(w, "the checkpoint was written with checkpointing disabled (CP_DISABLED_FLAG): the on-disk state may lag behind the live filesystem")
	}
	if cp.flags&cpErrorFlag != 0 {
		w = append(w, "the checkpoint records filesystem errors (CP_ERROR_FLAG)")
	}
	if cp.flags&cpFsckFlag != 0 {
		w = append(w, "the checkpoint asks for a filesystem check (CP_FSCK_FLAG)")
	}
	return w
}

// testBit reports bit i of a version bitmap. The kernel (f2fs_test_bit) and
// mkfs number the bits most-significant first within each byte.
func testBit(bm []byte, i uint32) bool {
	b := i >> 3
	if uint64(b) >= uint64(len(bm)) {
		return false
	}
	return bm[b]&(0x80>>(i&7)) != 0
}

// cpCRC returns the stored and the computed checksum of a checkpoint header
// block, or ok=false when its checksum_offset is outside the legal range.
//
// Derivation (kernel f2fs_checkpoint_chksum): crc = f2fs_crc32(ckpt,
// checksum_offset), i.e. the raw CRC-32 seeded with F2FS_SUPER_MAGIC over the
// bytes before the checksum word; when checksum_offset < CP_CHKSUM_OFFSET
// (4092), the CRC continues (same register, chained) over the rest of the
// block after the 4-byte checksum word. The checksum is stored at
// checksum_offset.
func cpCRC(blk []byte) (stored, computed uint32, ok bool) {
	off := binary.LittleEndian.Uint32(blk[cpChecksumOffset:])
	if off < cpMinChksumOffset || off > cpMaxChksumOffset {
		return 0, 0, false
	}
	c := rawCRC32(superMagic, blk[:off])
	if off < cpMaxChksumOffset {
		c = rawCRC32(c, blk[off+4:])
	}
	return binary.LittleEndian.Uint32(blk[off:]), c, true
}

// readBlock reads the 4 KiB block n.
func readBlock(r io.ReaderAt, n uint32) ([]byte, error) {
	b := make([]byte, blockSize)
	if err := readFull(r, b, int64(n)*blockSize); err != nil {
		return nil, err
	}
	return b, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// errUnwritten marks a pack whose first block is all zero: mkfs writes only
// the first pack, so a blank second pack is normal and not worth a warning.
var errUnwritten = errors.New("never written (all zero)")

// readPack reads and validates the checkpoint pack at block addr. The error
// is errUnwritten for a blank pack and otherwise names the first check that
// failed.
func readPack(r io.ReaderAt, sb *superblock, pack int, addr uint32) (*checkpoint, error) {
	le := binary.LittleEndian
	blk, err := readBlock(r, addr)
	if err != nil {
		return nil, readFailed("read of the first block", err)
	}
	if allZero(blk) {
		return nil, errUnwritten
	}
	if stored, want, ok := cpCRC(blk); !ok {
		return nil, fmt.Errorf("checksum_offset %d outside %d..%d", le.Uint32(blk[cpChecksumOffset:]), cpMinChksumOffset, cpMaxChksumOffset)
	} else if stored != want {
		return nil, fmt.Errorf("checksum mismatch in the first block: stored 0x%08x, computed 0x%08x", stored, want)
	}
	u32 := func(off int) uint32 { return le.Uint32(blk[off:]) }
	cp := &checkpoint{
		pack: pack, addr: addr,
		ver:            le.Uint64(blk[cpVersion:]),
		userBlocks:     le.Uint64(blk[cpUserBlocks:]),
		validBlocks:    le.Uint64(blk[cpValidBlocks:]),
		rsvdSegs:       u32(cpRsvdSegs),
		overprovSegs:   u32(cpOverprovSegs),
		freeSegs:       u32(cpFreeSegs),
		flags:          u32(cpFlags),
		packBlocks:     u32(cpPackBlocks),
		startSum:       u32(cpStartSum),
		validNodes:     u32(cpValidNodes),
		validInodes:    u32(cpValidInodes),
		nextFreeNid:    u32(cpNextFreeNid),
		sitBitmapBytes: u32(cpSITBitmapBytes),
		natBitmapBytes: u32(cpNATBitmapBytes),
	}
	if cp.packBlocks < 2 || cp.packBlocks > blocksPerSeg {
		return nil, fmt.Errorf("cp_pack_total_block_count %d outside 2..%d", cp.packBlocks, blocksPerSeg)
	}
	// The pack's last block is a second copy of the header and must agree.
	// addr + packBlocks - 1 stays inside the pack's segment, hence inside the
	// checkpoint area.
	last, err := readBlock(r, addr+cp.packBlocks-1)
	if err != nil {
		return nil, readFailed("read of the last block", err)
	}
	if stored, want, ok := cpCRC(last); !ok {
		return nil, fmt.Errorf("last block: checksum_offset %d outside %d..%d", le.Uint32(last[cpChecksumOffset:]), cpMinChksumOffset, cpMaxChksumOffset)
	} else if stored != want {
		return nil, fmt.Errorf("checksum mismatch in the last block: stored 0x%08x, computed 0x%08x", stored, want)
	}
	if v := le.Uint64(last[cpVersion:]); v != cp.ver {
		return nil, fmt.Errorf("checkpoint_ver %d in the first block but %d in the last", cp.ver, v)
	}

	// Data summaries lie after the header and payload blocks and before the
	// trailing copy.
	if lo := 1 + sb.cpPayload; cp.startSum < lo || cp.startSum >= cp.packBlocks-1 {
		return nil, fmt.Errorf("cp_pack_start_sum %d outside %d..%d", cp.startSum, lo, cp.packBlocks-2)
	}
	if err := cp.loadBitmaps(r, sb, blk); err != nil {
		return nil, err
	}
	return cp, nil
}

// loadBitmaps validates the SIT/NAT version bitmap sizes against the geometry
// and the checkpoint block, and copies the bitmaps out.
//
// Sizes must equal ((segment_count_x/2) << log_blocks_per_seg)/8, one bit per
// block of the first copy of each pair (the kernel rejects any other value).
//
// Placement follows the kernel's __bitmap_ptr, with C = 1 + cp_payload blocks
// starting at the pack:
//
//   - CP_LARGE_NAT_BITMAP_FLAG: after a 4-byte checksum word at 192, the NAT
//     bitmap, then the SIT bitmap, contiguous in C.
//   - otherwise with cp_payload > 0: the NAT bitmap at 192, the SIT bitmap at
//     the start of block 1.
//   - otherwise: the SIT bitmap at 192, the NAT bitmap right after it.
//
// (The plan's format reference lists the SIT bitmap first in the large case;
// the kernel source is the authority and puts the NAT bitmap first there.
// Task 6's real image confirms.) A bitmap must lie inside C and must not
// overlap the checkpoint's own checksum word.
func (cp *checkpoint) loadBitmaps(r io.ReaderAt, sb *superblock, blk []byte) error {
	wantSIT := uint64(sb.sitPairs()) << segShift / 8
	wantNAT := uint64(sb.natPairs()) << segShift / 8
	if uint64(cp.sitBitmapBytes) != wantSIT {
		return fmt.Errorf("sit_ver_bitmap_bytesize %d, the geometry needs %d", cp.sitBitmapBytes, wantSIT)
	}
	if uint64(cp.natBitmapBytes) != wantNAT {
		return fmt.Errorf("nat_ver_bitmap_bytesize %d, the geometry needs %d", cp.natBitmapBytes, wantNAT)
	}
	region := (uint64(sb.cpPayload) + 1) * blockSize
	var sitOff, natOff uint64
	switch {
	case cp.flags&cpLargeNATBitmapF != 0:
		natOff = cpBitmap + 4
		sitOff = natOff + wantNAT
	case sb.cpPayload > 0:
		natOff = cpBitmap
		sitOff = blockSize
	default:
		sitOff = cpBitmap
		natOff = sitOff + wantSIT
	}
	crcOff := uint64(binary.LittleEndian.Uint32(blk[cpChecksumOffset:]))
	for _, b := range []struct {
		name     string
		off, len uint64
	}{{"SIT", sitOff, wantSIT}, {"NAT", natOff, wantNAT}} {
		if b.off+b.len > region {
			return fmt.Errorf("%s version bitmap (%d bytes at %d) does not fit in the %d-byte checkpoint", b.name, b.len, b.off, region)
		}
		if b.off < crcOff+4 && crcOff < b.off+b.len {
			return fmt.Errorf("%s version bitmap (%d bytes at %d) overlaps the checksum word at %d", b.name, b.len, b.off, crcOff)
		}
	}
	read := func(off, n uint64) ([]byte, error) {
		out := make([]byte, n) // n <= 4 MiB: bounded by region (cp_payload <= 510)
		if off+n <= blockSize {
			copy(out, blk[off:])
			return out, nil
		}
		if err := readFull(r, out, int64(cp.addr)*blockSize+int64(off)); err != nil {
			return nil, readFailed("read of the version bitmaps", err)
		}
		return out, nil
	}
	var err error
	if cp.sitBitmap, err = read(sitOff, wantSIT); err != nil {
		return err
	}
	cp.natBitmap, err = read(natOff, wantNAT)
	return err
}

// selectCheckpoint reads both packs and returns the valid one with the higher
// checkpoint_ver (pack 1 on a tie). An invalid pack is skipped with a warning.
// A blank (all-zero) pack 2 is normal (mkfs writes only the first pack): it is
// silent and recorded as the checkpoint's pack2Blank, which Info lists as a
// feature. A blank pack 1 while pack 2 is valid is reported as a possible
// rollback, because pack 1 is always written first. A genuine read failure is
// returned as an I/O error, not as corruption. When neither pack is valid the
// result is a *filesys.CorruptError.
func selectCheckpoint(r io.ReaderAt, sb *superblock) (*checkpoint, []string, error) {
	addrs := [2]uint32{sb.cpAddr, sb.cpAddr + blocksPerSeg} // area has >= 2 segments
	var cps [2]*checkpoint
	var errs [2]error
	var warns []string
	for i := range cps {
		cps[i], errs[i] = readPack(r, sb, i+1, addrs[i])
		var ioe *ioError
		if errors.As(errs[i], &ioe) {
			return nil, nil, fmt.Errorf("f2fs checkpoint pack %d at block %d: %w", i+1, addrs[i], ioe)
		}
		if errs[i] != nil && !errors.Is(errs[i], errUnwritten) {
			warns = append(warns, fmt.Sprintf("checkpoint pack %d at block %d is invalid and was skipped: %v", i+1, addrs[i], errs[i]))
		}
	}
	var chosen *checkpoint
	switch {
	case cps[0] != nil && cps[1] != nil:
		chosen = cps[0]
		if cps[1].ver > cps[0].ver {
			chosen = cps[1]
		}
	case cps[0] != nil:
		chosen = cps[0]
	case cps[1] != nil:
		chosen = cps[1]
	}
	if chosen != nil {
		if errors.Is(errs[0], errUnwritten) {
			warns = append(warns, fmt.Sprintf("checkpoint pack 1 is blank; using pack %d (possible rollback)", chosen.pack))
		}
		chosen.pack2Blank = errors.Is(errs[1], errUnwritten)
		return chosen, warns, nil
	}
	var parts []string
	for i, e := range errs {
		parts = append(parts, fmt.Sprintf("pack %d (block %d): %v", i+1, addrs[i], e))
	}
	return nil, nil, corrupt("f2fs checkpoint", int64(sb.cpAddr)*blockSize, "no valid checkpoint pack: %s", strings.Join(parts, "; "))
}
