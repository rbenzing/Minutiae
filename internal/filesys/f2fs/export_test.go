package f2fs

// Test-only accessors, so the external f2fs_test package can check internals
// without widening the public API.

// Geometry is a read-only copy of the superblock geometry.
type Geometry struct {
	BlockCount                               uint64
	SegmentCount                             uint32
	SegCkpt, SegSIT, SegNAT, SegSSA, SegMain uint32
	Seg0, CP, SIT, NAT, SSA, Main            uint32
	RootIno, NodeIno, MetaIno                uint32
	CPPayload                                uint32
	Feature                                  uint32
	Blocks                                   int64
}

// Geometry returns the decoded superblock geometry.
func (f *FS) Geometry() Geometry {
	sb := f.sb
	return Geometry{
		BlockCount: sb.blockCount, SegmentCount: sb.segmentCount,
		SegCkpt: sb.segCkpt, SegSIT: sb.segSIT, SegNAT: sb.segNAT, SegSSA: sb.segSSA, SegMain: sb.segMain,
		Seg0: sb.seg0Addr, CP: sb.cpAddr, SIT: sb.sitAddr, NAT: sb.natAddr, SSA: sb.ssaAddr, Main: sb.mainAddr,
		RootIno: sb.rootIno, NodeIno: sb.nodeIno, MetaIno: sb.metaIno,
		CPPayload: sb.cpPayload, Feature: sb.feature, Blocks: sb.blocks,
	}
}

// Checkpoint is a read-only copy of the selected checkpoint pack.
type Checkpoint struct {
	Pack                    int
	Addr                    uint32
	Version                 uint64
	Flags                   uint32
	PackBlocks, StartSum    uint32
	UserBlocks, ValidBlocks uint64
	FreeSegs                uint32
	NextFreeNid             uint32
	SITBitmap, NATBitmap    []byte
}

// Checkpoint returns the selected checkpoint pack.
func (f *FS) Checkpoint() Checkpoint {
	cp := f.cp
	return Checkpoint{
		Pack: cp.pack, Addr: cp.addr, Version: cp.ver, Flags: cp.flags,
		PackBlocks: cp.packBlocks, StartSum: cp.startSum,
		UserBlocks: cp.userBlocks, ValidBlocks: cp.validBlocks,
		FreeSegs: cp.freeSegs, NextFreeNid: cp.nextFreeNid,
		SITBitmap: cp.sitBitmap, NATBitmap: cp.natBitmap,
	}
}

// RawCRC32 exposes the kernel-style crc32_le (no inversion).
var RawCRC32 = rawCRC32

// TestBit exposes the version-bitmap bit test (MSB-first within each byte).
var TestBit = testBit

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }
