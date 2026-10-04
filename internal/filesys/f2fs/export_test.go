package f2fs

import "github.com/rbenzing/minutiae/internal/filesys"

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

// NATLookup exposes natLookup.
func (f *FS) NATLookup(nid uint32) (uint32, error) { return f.natLookup(nid) }

// Node exposes node.
func (f *FS) Node(nid uint32) ([]byte, error) { return f.node(nid) }

// ParseNodeID exposes parseNodeID.
func ParseNodeID(id string) (uint32, error) { return parseNodeID(id) }

// InodeView is a read-only copy of a parsed inode.
type InodeView struct {
	Mode                        uint16
	Inline                      uint8
	UID, GID, Links             uint32
	Size                        int64
	Blocks                      uint64
	Times                       filesys.Times
	Generation, XattrNID, PIno  uint32
	Flags                       uint32
	Name                        string
	NameBad                     bool
	DirLevel                    uint8
	Ext                         [3]uint32
	NIDs                        [5]uint32
	ExtraIsize, XattrWords      int
	ProjID                      uint32
	AddrStart, AddrSlots        int
	ChecksumChecked, ChecksumOK bool
	Compressed, Encrypted       bool
	NSInvalid, TimeInvalid      bool
	Raw                         []byte
}

// Inode parses inode ino and returns it with its directory entry (named
// "name", with the given raw name).
func (f *FS) Inode(ino uint32) (InodeView, filesys.Entry, error) {
	in, err := f.inode(ino)
	if err != nil {
		return InodeView{}, filesys.Entry{}, err
	}
	v := InodeView{
		Mode: in.mode, Inline: in.inline, UID: in.uid, GID: in.gid, Links: in.links,
		Size: in.size, Blocks: in.blocks, Times: in.times,
		Generation: in.generation, XattrNID: in.xattrNID, PIno: in.pino, Flags: in.flags,
		Name: string(in.name), NameBad: in.nameBad, DirLevel: in.dirLevel, Ext: in.ext, NIDs: in.nids,
		ExtraIsize: in.extraIsize, XattrWords: in.xattrWords, ProjID: in.projID,
		AddrStart: in.addrStart, AddrSlots: in.addrSlots,
		ChecksumChecked: in.csumChecked, ChecksumOK: in.csumOK,
		Compressed: in.compressed, Encrypted: in.encrypted,
		NSInvalid: in.nsInvalid, TimeInvalid: in.timeBad, Raw: in.raw,
	}
	return v, toEntry("name", []byte("raw"), in), nil
}

// SetDirBudget sets the bytes of directory blocks the instance may still read.
func (f *FS) SetDirBudget(n int64) {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	f.dirBudget, f.dirBudgetTotal = n, n
}

// SetUnallocatedRunCap lowers the number of runs Unallocated reports.
func (f *FS) SetUnallocatedRunCap(n int) { f.unallocCap = n }

// SetDirEntryCap lowers the number of entries one directory scan yields.
func (f *FS) SetDirEntryCap(n int) { f.dirCap = n }
