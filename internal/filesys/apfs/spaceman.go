package apfs

import (
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Space manager layout (Format reference of the 2F plan; verified against real
// containers: the real fixtures' spaceman, CIB and bitmap blocks).
//
// spaceman_phys_t: sm_block_size u32 @32, sm_blocks_per_chunk u32 @36,
// sm_chunks_per_cib u32 @40, sm_cibs_per_cab u32 @44, sm_dev[2] @48 (48 bytes
// each: sm_block_count u64 @0, sm_chunk_count u64 @8, sm_cib_count u32 @16,
// sm_cab_count u32 @20, sm_free_count u64 @24, sm_addr_offset u32 @32),
// sm_ip_block_count u64 @152, sm_ip_bm_block_count u32 @164, sm_ip_bm_base u64
// @168, sm_ip_base u64 @176.
const (
	typeSpacemanCAB = 0x6
	typeSpacemanCIB = 0x7

	smBlockSize      = 32
	smBlocksPerChunk = 36
	smChunksPerCIB   = 40
	smCibsPerCAB     = 44
	smDev0           = 48
	smDev1           = 96
	smDevSize        = 48
	smIPBlockCount   = 152
	smIPBmBlockCount = 164
	smIPBmBase       = 168
	smIPBase         = 176
	smMinLen         = 184 // bytes needed through sm_ip_base

	// spaceman_device_t
	sdBlockCount = 0
	sdChunkCount = 8
	sdCibCount   = 16
	sdCabCount   = 20
	sdAddrOffset = 32

	// chunk_info_block_t / cib_addr_block_t: header, then the records.
	cibIndexOff   = 32
	cibCountOff   = 36
	cibRecsOff    = 40
	chunkInfoSize = 32 // chunk_info_t: ci_xid, ci_addr, ci_block_count, ci_free_count, ci_bitmap_addr
	ciAddrOff     = 8
	ciBlocksOff   = 16
	ciFreeOff     = 20
	ciBitmapOff   = 24
	ciCountMask   = 0x000fffff // CI_COUNT_MASK; the other 12 bits are reserved
)

// spaceman is the validated geometry of the main device.
type spaceman struct {
	blocks     uint64 // sm_dev[0].sm_block_count
	chunks     uint64
	cibs, cabs uint64
	cpc, cpcab uint64 // chunks per CIB, CIBs per CAB
	addrs      []byte // the address array (CABs when cabs > 0, else CIBs)
	obj        [2]uint64
	// Ranges that are never free whatever a bitmap says.
	ipBase, ipBlocks, ipBmBase, ipBmBlocks uint64
}

// readFresh reads the object of size bytes at block paddr straight from the
// image (not through the metadata cache): free-space metadata is checked as it
// lies now. Bounds failures are *filesys.CorruptError, read failures are
// returned wrapped as they are (a short read matches isShort).
func (f *FS) readFresh(paddr uint64, size int) ([]byte, error) {
	if size <= 0 || size > maxObjectBytes || size%f.bs != 0 {
		return nil, corrupt("object", -1, "object at block %d has unusable size %d (block size %d)", paddr, size, f.bs)
	}
	nblk := uint64(size / f.bs)
	if paddr >= f.blocks || nblk > f.blocks-paddr {
		return nil, corrupt("object", -1, "object at block %d (%d blocks) lies outside the container of %d blocks", paddr, nblk, f.blocks)
	}
	buf := make([]byte, size)
	if err := readFull(f.data, buf, int64(paddr)*int64(f.bs)); err != nil {
		return nil, fmt.Errorf("apfs: read block %d: %w", paddr, err)
	}
	return buf, nil
}

// readFreshObject is readFresh plus the checksum, type and (when want != 0)
// header checks. The returned problem is a reason the object cannot be used
// (a warning for the caller); err is a read failure that is not corruption.
func (f *FS) readFreshObject(paddr uint64, size int, want uint32) (buf []byte, problem string, err error) {
	buf, err = f.readFresh(paddr, size)
	switch {
	case err == nil:
	case isShort(err):
		return nil, "is unreadable (truncated image)", nil
	case errors.Is(err, filesys.ErrCorrupt):
		return nil, err.Error(), nil
	default:
		return nil, "", err
	}
	if !checksumOK(buf) {
		return nil, "has a bad checksum", nil
	}
	if h := parseHeader(buf); h.kind() != want {
		return nil, fmt.Sprintf("has object type %#x, want %#x", h.kind(), want), nil
	}
	return buf, "", nil
}

func ceilDiv(a, b uint64) uint64 { return (a + b - 1) / b } // b > 0, a+b-1 does not overflow for the bounded inputs

// loadSpaceman reads and validates the selected checkpoint's space manager. A
// non-empty problem says why it cannot be trusted (the caller reports no free
// space); err is a read failure that is not corruption.
func (f *FS) loadSpaceman() (sm *spaceman, problem string, err error) {
	m, ok := f.cp.ephemeral[f.nx.spacemanOid]
	if !ok || m.typ&typeMask != typeSpaceman {
		return nil, fmt.Sprintf("the checkpoint maps no space manager for oid %d", f.nx.spacemanOid), nil
	}
	buf, why, err := f.readFreshObject(m.paddr, int(m.size), typeSpaceman)
	if err != nil {
		return nil, "", err
	}
	if why != "" {
		return nil, fmt.Sprintf("the space manager at block %d %s", m.paddr, why), nil
	}
	if h := parseHeader(buf); h.oid != m.oid {
		return nil, fmt.Sprintf("the space manager at block %d carries oid %d, want %d", m.paddr, h.oid, m.oid), nil
	}
	if len(buf) < smMinLen {
		return nil, fmt.Sprintf("the space manager object of %d bytes is too short", len(buf)), nil
	}
	bad := func(format string, a ...any) (*spaceman, string, error) {
		return nil, fmt.Sprintf(format, a...), nil
	}
	bs := uint64(f.bs)
	if got := uint64(le.Uint32(buf[smBlockSize:])); got != bs {
		return bad("its block size %d differs from the container's %d", got, bs)
	}
	bpc := uint64(le.Uint32(buf[smBlocksPerChunk:]))
	if bpc != bs*8 {
		return bad("%d blocks per chunk, want %d (one bitmap block per chunk)", bpc, bs*8)
	}
	cpc, cpcab := uint64(le.Uint32(buf[smChunksPerCIB:])), uint64(le.Uint32(buf[smCibsPerCAB:]))
	if cpc == 0 || cpc > (bs-cibRecsOff)/chunkInfoSize {
		return bad("%d chunk-info records per CIB do not fit a block", cpc)
	}
	d1 := buf[smDev1:]
	if le.Uint64(d1[sdBlockCount:]) != 0 || le.Uint64(d1[sdChunkCount:]) != 0 || le.Uint32(d1[sdCibCount:]) != 0 {
		return bad("a second (tier 2) device is present")
	}
	d0 := buf[smDev0:]
	sm = &spaceman{
		blocks: le.Uint64(d0[sdBlockCount:]), chunks: le.Uint64(d0[sdChunkCount:]),
		cibs: uint64(le.Uint32(d0[sdCibCount:])), cabs: uint64(le.Uint32(d0[sdCabCount:])),
		cpc: cpc, cpcab: cpcab,
		ipBlocks: le.Uint64(buf[smIPBlockCount:]), ipBase: le.Uint64(buf[smIPBase:]),
		ipBmBlocks: uint64(le.Uint32(buf[smIPBmBlockCount:])), ipBmBase: le.Uint64(buf[smIPBmBase:]),
	}
	sm.obj = [2]uint64{m.paddr, uint64(m.size) / bs}
	switch {
	case sm.blocks == 0 || sm.blocks > f.blocks:
		return bad("the main device has %d blocks, the container %d", sm.blocks, f.blocks)
	case sm.chunks != ceilDiv(sm.blocks, bpc):
		return bad("%d chunks for %d blocks of %d per chunk", sm.chunks, sm.blocks, bpc)
	case sm.cibs != ceilDiv(sm.chunks, cpc):
		return bad("%d chunk-info blocks for %d chunks of %d per block", sm.cibs, sm.chunks, cpc)
	}
	if sm.cabs > 0 {
		if cpcab == 0 || cpcab > (bs-cibRecsOff)/8 {
			return bad("%d CIB addresses per CAB do not fit a block", cpcab)
		}
		if sm.cabs != ceilDiv(sm.cibs, cpcab) {
			return bad("%d CIB-address blocks for %d CIBs of %d per block", sm.cabs, sm.cibs, cpcab)
		}
	}
	n := sm.cibs
	if sm.cabs > 0 {
		n = sm.cabs
	}
	off := uint64(le.Uint32(d0[sdAddrOffset:]))
	if off < smMinLen || off > uint64(len(buf)) || n > (uint64(len(buf))-off)/8 {
		return bad("the address array (%d entries at offset %d) does not fit the %d-byte object", n, off, len(buf))
	}
	sm.addrs = buf[off : off+8*n]
	for _, r := range [][2]uint64{{sm.ipBase, sm.ipBlocks}, {sm.ipBmBase, sm.ipBmBlocks}} {
		if end, ok := addU64(r[0], r[1]); !ok || end > f.blocks {
			return bad("the internal pool range [%d, +%d) is not inside the container of %d blocks", r[0], r[1], f.blocks)
		}
	}
	return sm, "", nil
}
