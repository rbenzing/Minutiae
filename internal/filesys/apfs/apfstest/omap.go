package apfstest

import "sort"

// OmapValDeleted is OMAP_VAL_DELETED.
const OmapValDeleted = 1

// OmapEntry is one record of an object map tree: key (Oid, Xid), value
// {Flags, Size, Paddr}. A zero Size on a live entry defaults to one block.
type OmapEntry struct {
	Oid, Xid uint64
	Flags    uint32
	Size     uint32
	Paddr    uint64
}

// OmapSnap is one record of the snapshot tree: key Xid, value {Flags, 0, Oid}.
type OmapSnap struct {
	Xid   uint64
	Flags uint32
	Oid   uint64
}

// OmapRecs encodes entries as fixed 16/16 records sorted by (oid, xid).
func OmapRecs(entries []OmapEntry, blockSize int) []Rec {
	es := append([]OmapEntry(nil), entries...)
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Oid != es[j].Oid {
			return es[i].Oid < es[j].Oid
		}
		return es[i].Xid < es[j].Xid
	})
	recs := make([]Rec, len(es))
	for i, e := range es {
		if e.Size == 0 && e.Flags&OmapValDeleted == 0 {
			e.Size = uint32(blockSize)
		}
		k := make([]byte, 16)
		le.PutUint64(k[0:], e.Oid)
		le.PutUint64(k[8:], e.Xid)
		v := make([]byte, 16)
		le.PutUint32(v[0:], e.Flags)
		le.PutUint32(v[4:], e.Size)
		le.PutUint64(v[8:], e.Paddr)
		recs[i] = Rec{Key: k, Val: v}
	}
	return recs
}

// OmapTreeSpec is the shape of an object-map tree as the real containers have
// it: physical, fixed 16/16 entries, bt_flags PHYSICAL.
func OmapTreeSpec(blockSize int, xid uint64, maxKeys int) TreeSpec {
	return TreeSpec{
		BlockSize: blockSize, Fixed: true, KeySize: 16, ValSize: 16,
		BTFlags: 0x10, Storage: StoragePhysical, Xid: xid, MaxKeys: maxKeys,
	}
}

// snapRecs encodes snapshot-tree records sorted by xid.
func snapRecs(snaps []OmapSnap) []Rec {
	ss := append([]OmapSnap(nil), snaps...)
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Xid < ss[j].Xid })
	recs := make([]Rec, len(ss))
	for i, s := range ss {
		k := make([]byte, 8)
		le.PutUint64(k, s.Xid)
		v := make([]byte, 16)
		le.PutUint32(v[0:], s.Flags)
		le.PutUint64(v[8:], s.Oid)
		recs[i] = Rec{Key: k, Val: v}
	}
	return recs
}

// OmapPhys writes an omap_phys_t into b (an object of its own block).
func OmapPhys(b []byte, addr, xid uint64, flags uint32, snapCount uint32, treeOid, snapTreeOid uint64) {
	putObjHeader(b, addr, xid, flagPhysical|typeOmap, 0)
	le.PutUint32(b[32:], flags)
	le.PutUint32(b[36:], snapCount)
	le.PutUint32(b[40:], flagPhysical|typeBTree)
	le.PutUint32(b[44:], flagPhysical|typeBTree)
	le.PutUint64(b[48:], treeOid)
	le.PutUint64(b[56:], snapTreeOid)
}
