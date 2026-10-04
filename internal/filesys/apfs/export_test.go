package apfs

import "errors"

// Test-only accessors, so the external apfs_test package can check internals
// without widening the public API.

// Warn records a warning, as the reader does when it meets a problem.
func (f *FS) Warn(format string, a ...any) { f.warn(format, a...) }

// NXFields is the part of the selected container superblock the tests compare.
type NXFields struct {
	Xid                    uint64
	BlockSize              uint32
	BlockCount             uint64
	Features, RoCompat     uint64
	Incompat, Flags        uint64
	UUID                   [16]byte
	DescBlocks, DataBlocks uint32
	DescBase, DataBase     uint64
	DescIndex, DescLen     uint32
	SpacemanOid, OmapOid   uint64
	MaxFS                  uint32
	NextXid, NextOid       uint64
	DescNext, DataNext     uint32
	DataIndex, DataLen     uint32
	ReaperOid              uint64
	FsOids                 []uint64 // nx_fs_oid, all 100 slots
	BlockedStart           uint64
	BlockedCount           uint64
	EvictOid               uint64
}

// NX returns the fields of the selected checkpoint superblock.
func (f *FS) NX() NXFields {
	n := f.nx
	return NXFields{
		Xid: n.xid, BlockSize: n.blockSize, BlockCount: n.blockCount,
		Features: n.features, RoCompat: n.roCompat, Incompat: n.incompat, Flags: n.flags,
		UUID: n.uuid, DescBlocks: n.descBlocks, DataBlocks: n.dataBlocks,
		DescBase: n.descBase, DataBase: n.dataBase, DescIndex: n.descIndex, DescLen: n.descLen,
		SpacemanOid: n.spacemanOid, OmapOid: n.omapOid, MaxFS: n.maxFS, NextXid: n.nextXid,
		NextOid: n.nextOid, DescNext: n.descNext, DataNext: n.dataNext,
		DataIndex: n.dataIndex, DataLen: n.dataLen, ReaperOid: n.reaperOid,
		FsOids: n.fsOid[:], BlockedStart: n.blockedStart, BlockedCount: n.blockedCount, EvictOid: n.evictOid,
	}
}

// SuperblockIndex is the ring index of the selected checkpoint superblock.
func (f *FS) SuperblockIndex() int { return f.cp.index }

// Ephemeral returns the checkpoint-map entry of an ephemeral object.
func (f *FS) Ephemeral(oid uint64) (paddr uint64, size, typ uint32, ok bool) {
	m, ok := f.cp.ephemeral[oid]
	return m.paddr, m.size, m.typ, ok
}

// EphemeralCount is the number of ephemeral objects the checkpoint maps.
func (f *FS) EphemeralCount() int { return len(f.cp.ephemeral) }

// ReadObject exposes readObject; want is an object type or AnyType.
func (f *FS) ReadObject(paddr uint64, size int, want uint32) ([]byte, uint32, uint64, error) {
	b, h, err := f.readObject(paddr, size, want)
	return b, h.typ, h.xid, err
}

// AnyType makes ReadObject accept every object type.
const AnyType = anyType

// IsChecksumError reports whether err is a checksum mismatch, and its address.
func IsChecksumError(err error) (paddr uint64, ok bool) {
	var ce *checksumError
	if errors.As(err, &ce) {
		return ce.paddr, true
	}
	return 0, false
}

// ChecksumOK exposes the object checksum verification.
var ChecksumOK = checksumOK

// MaxDescBlocks is the cap on the checkpoint descriptor and data areas.
const MaxDescBlocks = maxAreaBlocks
