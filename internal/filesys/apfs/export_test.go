package apfs

import (
	"bytes"
	"errors"
	"fmt"
)

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

// OmapEntry is one record of an object map tree.
type OmapEntry struct {
	Oid, Xid uint64
	Flags    uint32
	Size     uint32
	Paddr    uint64
}

// OmapSnapshot is one record of an object map's snapshot tree.
type OmapSnapshot struct {
	Xid   uint64
	Flags uint32
	Oid   uint64
}

// omapAt opens the object map at block paddr; 0 means the container's own.
func (f *FS) omapAt(paddr uint64) (*omapView, error) {
	if paddr == 0 {
		return f.cmap, nil
	}
	return f.openOmap(paddr)
}

// OmapEntries returns every record of the object map at paddr (0: the
// container's), in tree order.
func (f *FS) OmapEntries(paddr uint64) ([]OmapEntry, error) {
	o, err := f.omapAt(paddr)
	if err != nil {
		return nil, err
	}
	var out []OmapEntry
	err = o.tree.scan(nil, func(k, v []byte) (bool, error) {
		if len(k) != 16 || len(v) != 16 {
			return false, fmt.Errorf("omap record %d/%d bytes", len(k), len(v))
		}
		out = append(out, OmapEntry{
			Oid: le.Uint64(k), Xid: le.Uint64(k[8:]),
			Flags: le.Uint32(v), Size: le.Uint32(v[4:]), Paddr: le.Uint64(v[8:]),
		})
		return false, nil
	})
	return out, err
}

// OmapLookup is the object map lookup at block paddr (0: the container's).
func (f *FS) OmapLookup(paddr, oid, xid uint64) (uint64, uint32, uint32, error) {
	o, err := f.omapAt(paddr)
	if err != nil {
		return 0, 0, 0, err
	}
	return o.lookup(oid, xid)
}

// OmapSnapshots lists the snapshot tree of the object map at paddr (0: the
// container's).
func (f *FS) OmapSnapshots(paddr uint64) ([]OmapSnapshot, error) {
	o, err := f.omapAt(paddr)
	if err != nil {
		return nil, err
	}
	snaps, err := o.snapshots()
	var out []OmapSnapshot
	for _, s := range snaps {
		out = append(out, OmapSnapshot{Xid: s.xid, Flags: s.flags, Oid: s.oid})
	}
	return out, err
}

// OpenOmapErr opens the object map at paddr and returns only the error.
func (f *FS) OpenOmapErr(paddr uint64) error {
	_, err := f.openOmap(paddr)
	return err
}

// TreeRec is one scanned record (copied).
type TreeRec struct{ Key, Val []byte }

// ScanTree opens the tree rooted at rootOid with tree type typ (storage bits
// decide physical or virtual) and scans it. A virtual tree is resolved through
// the container object map when useOmap is set, else through none. prefix nil
// visits everything; limit > 0 stops after that many records.
func (f *FS) ScanTree(rootOid uint64, typ uint32, useOmap bool, prefix func(key []byte) int, limit int) ([]TreeRec, error) {
	var o *omapView
	if useOmap {
		o = f.cmap
	}
	t, err := f.openTree(rootOid, typ, o)
	if err != nil {
		return nil, err
	}
	var out []TreeRec
	err = t.scan(prefix, func(k, v []byte) (bool, error) {
		out = append(out, TreeRec{Key: append([]byte(nil), k...), Val: append([]byte(nil), v...)})
		return limit > 0 && len(out) >= limit, nil
	})
	return out, err
}

// ScanVolumeTree is ScanTree through the object map at block omapPaddr.
func (f *FS) ScanVolumeTree(omapPaddr, rootOid uint64, typ uint32) ([]TreeRec, error) {
	o, err := f.openOmap(omapPaddr)
	if err != nil {
		return nil, err
	}
	t, err := f.openTree(rootOid, typ, o)
	if err != nil {
		return nil, err
	}
	var out []TreeRec
	err = t.scan(nil, func(k, v []byte) (bool, error) {
		out = append(out, TreeRec{Key: append([]byte(nil), k...), Val: append([]byte(nil), v...)})
		return false, nil
	})
	return out, err
}

// SetNodeBudget sets the per-scan node budget.
func (f *FS) SetNodeBudget(n int) { f.nodeBudget = n }

// ParseEntryID exposes parseEntryID; kind is "root", "node" or "snaps".
func ParseEntryID(id string) (kind string, vol int, view, ino uint64, ok bool) {
	k, vol, view, ino, ok := parseEntryID(id)
	return map[idKind]string{idNone: "", idRoot: "root", idNode: "node", idSnaps: "snaps"}[k], vol, view, ino, ok
}

// NodeID and SnapsID build entry IDs.
func NodeID(vol int, view, ino uint64) string { return nodeID(vol, view, ino) }

// SetDirBudget sets the bytes of directory records this FS may still scan.
func (f *FS) SetDirBudget(n int64) { f.dirBudget.Store(n) }

// DirBudget is the directory budget left.
func (f *FS) DirBudget() int64 { return f.dirBudget.Load() }

// SetMaxDirEntries sets the per-directory entry cap.
func (f *FS) SetMaxDirEntries(n int) { f.maxDirEntries = n }

// Scans counts the file-system tree scans started so far.
func (f *FS) Scans() int64 { return f.scans.Load() }

// VolumeFields is what the tests compare of a volume.
type VolumeFields struct {
	Slot                      int
	Readable                  bool
	Name                      []byte
	Display                   string
	RawName                   []byte
	UUID                      [16]byte
	Features, RoCompat        uint64
	Incompat, FsFlags         uint64
	Role                      uint16
	FsIndex                   uint32
	OmapOid, RootOid          uint64
	RootType                  uint32
	Snapshots, LastMod        uint64
	Encrypted, Sealed         bool
	CaseInsensitive, NormInsn bool
}

// VolumeFields returns the volume in slot.
func (f *FS) VolumeFields(slot int) (VolumeFields, bool) {
	if slot < 0 || slot >= len(f.slots) || f.slots[slot] == nil {
		return VolumeFields{}, false
	}
	v := f.slots[slot]
	return VolumeFields{
		Slot: v.slot, Readable: v.readable, Name: v.name, Display: v.display, RawName: v.rawName,
		UUID: v.uuid, Features: v.features, RoCompat: v.roCompat, Incompat: v.incompat, FsFlags: v.fsFlags,
		Role: v.role, FsIndex: v.fsIndex, OmapOid: v.omapOid, RootOid: v.rootOid, RootType: v.rootType,
		Snapshots: v.snapshots, LastMod: v.lastMod, Encrypted: v.encrypted, Sealed: v.sealed,
		CaseInsensitive: v.caseInsen, NormInsn: v.normInsen,
	}, true
}

// XField is one decoded extended field.
type XField struct {
	Type, Flags uint8
	Data        []byte
}

// ParseXfields decodes an xf_blob_t and returns the fields and the warnings.
func ParseXfields(b []byte) ([]XField, []string) {
	var warns []string
	fs := parseXfields(b, func(format string, a ...any) { warns = append(warns, fmt.Sprintf(format, a...)) })
	var out []XField
	for _, x := range fs {
		out = append(out, XField{Type: x.typ, Flags: x.flags, Data: x.data})
	}
	return out, warns
}

// InodeFields is a decoded inode.
type InodeFields struct {
	Ino, ParentID, PrivateID    uint64
	Create, Mod, Change, Access uint64
	InternalFlags               uint64
	Links                       int32
	ProtClass, BsdFlags         uint32
	UID, GID                    uint32
	Mode                        uint16
	UncompressedSize            uint64
	HasDstream                  bool
	Size                        int64
	CryptoID                    uint64
	XattrNames                  []string
	XattrCount                  int
	Symlink                     []byte
	SymlinkSet, SymlinkOK       bool
}

// Inode decodes an inode of the volume in slot as of view.
func (f *FS) Inode(slot int, view, ino uint64) (InodeFields, error) {
	if slot < 0 || slot >= len(f.slots) || f.slots[slot] == nil {
		return InodeFields{}, errors.New("no such volume")
	}
	in, err := f.inode(f.slots[slot], view, ino)
	if err != nil {
		return InodeFields{}, err
	}
	return InodeFields{
		Ino: in.ino, ParentID: in.parentID, PrivateID: in.privateID,
		Create: in.create, Mod: in.mod, Change: in.change, Access: in.access,
		InternalFlags: in.internalFlags, Links: in.links, ProtClass: in.protClass, BsdFlags: in.bsdFlags,
		UID: in.uid, GID: in.gid, Mode: in.mode, UncompressedSize: in.uncompressedSize,
		HasDstream: in.hasDstream, Size: in.size, CryptoID: in.cryptoID,
		XattrNames: in.xattrNames, XattrCount: in.xattrCount,
		Symlink: in.symlink, SymlinkSet: in.symlinkSet, SymlinkOK: in.symlinkOK,
	}, nil
}

// DrecInfo is a decoded directory record.
type DrecInfo struct {
	Name   []byte
	FileID uint64
	Flags  uint16
}

// DirRecords lists the directory records of directory dir of the volume in slot
// (the directory inode itself is not required to exist).
func (f *FS) DirRecords(slot int, view, dir uint64) ([]DrecInfo, error) {
	if slot < 0 || slot >= len(f.slots) || f.slots[slot] == nil {
		return nil, errors.New("no such volume")
	}
	var out []DrecInfo
	err := f.scanDir(f.slots[slot], view, dir, func(d *drec) bool {
		out = append(out, DrecInfo{Name: bytes.Clone(d.name), FileID: d.fileID, Flags: d.flags})
		return false
	})
	return out, err
}

// SetNodeReads sets the node reads all scans of this FS may still make.
func (f *FS) SetNodeReads(n int64) { f.nodeReadLimit = n; f.nodeReads.Store(n) }
