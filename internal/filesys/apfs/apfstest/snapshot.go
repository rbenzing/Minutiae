package apfstest

import (
	"sort"
)

// Snapshot describes one snapshot of a volume. Volume.Files is the live tree
// (the newest state); each snapshot is its own fs-tree, written at its xid:
// the volume object map maps the same virtual root oid to the old tree at that
// xid and to the live tree at the newest xid (the other nodes of a snapshot
// tree have oids of their own). The volume gets a snapshot-metadata tree
// (SNAP_METADATA and SNAP_NAME records), an omap_snapshot_t tree and, per
// snapshot, a volume-superblock copy at the block its metadata names.
type Snapshot struct {
	Name    string
	RawName []byte // stored name bytes when set (overrides Name)
	Files   []File // the tree as of the snapshot
	// CreateTime and ChangeTime are nanoseconds since 1970 (0: DefaultTime).
	CreateTime, ChangeTime uint64
	// Xid is the snapshot's transaction id (0: 2, 3, ... in order; the live tree
	// is then at len(Snapshots)+2).
	Xid uint64

	Deleted        bool // the omap snapshot record is flagged DELETED
	Reverted       bool // ... or REVERTED
	NoOmapSnapshot bool // no record in the volume object map's snapshot tree
	NoSblock       bool // the metadata names no volume-superblock copy (sblock_oid 0)
	BadSblock      bool // the volume-superblock copy fails its checksum
	NoNameRecord   bool // no SNAP_NAME record

	// MutateSblock rewrites the volume-superblock copy before it is sealed (a
	// copy that verifies but says the wrong thing).
	MutateSblock func(b []byte)

	// MutateMeta rewrites the key and value of the SNAP_METADATA record before it
	// is written (hostile records).
	MutateMeta func(key, val []byte) ([]byte, []byte)
}

// SnapGeo is where a snapshot's objects are.
type SnapGeo struct {
	Xid     uint64
	Sblock  uint64   // block of the volume-superblock copy (0: none)
	FsNodes []uint64 // fs-tree node blocks, root first
	FsOids  []uint64 // their virtual oids, root first
}

// Record types of the snapshot-metadata tree (builder's own copy).
const (
	typeSnapMetadata = 1
	typeSnapName     = 11

	snapMetaValFixed = 50 // j_snap_metadata_val_t before the name
	snapNameID       = idMask

	omapSnapDeleted  = 1
	omapSnapReverted = 2
)

// snapTree is one snapshot compiled and packed, its blocks not yet placed.
type snapTree struct {
	s      Snapshot
	xid    uint64
	blocks []Block // root first
	cnt    counts
	nextID uint64
	sblock uint64
}

func (s Snapshot) name() []byte {
	if s.RawName != nil {
		return s.RawName
	}
	return []byte(s.Name)
}

func orDefault(t uint64) uint64 {
	if t == 0 {
		return DefaultTime
	}
	return t
}

// packSnapshots compiles and packs the snapshot trees of v. liveRoot is the
// virtual oid of the live fs-tree's root, shared by every snapshot tree; oidNext
// hands out the oids of the other nodes.
func (v Volume) packSnapshots(spec TreeSpec, da *dataAlloc, liveRoot uint64, oidNext *uint64) []*snapTree {
	var out []*snapTree
	for i, s := range v.Snapshots {
		xid := s.Xid
		if xid == 0 {
			xid = uint64(i) + 2
		}
		sv := v
		sv.Files, sv.Extra, sv.Reorder, sv.Snapshots = s.Files, nil, nil, nil
		recs, _, cnt, nextID, _ := sv.compile(da)
		sspec := spec
		sspec.Xid = xid
		n := len(PackTree(sspec, toRecs(recs), func() (uint64, uint64) { return 0, 0 }))
		call := 0
		blocks := PackTree(sspec, toRecs(recs), func() (uint64, uint64) {
			call++
			if call == n { // the root, allocated last
				return 0, liveRoot
			}
			oid := *oidNext
			*oidNext++
			return 0, oid
		})
		out = append(out, &snapTree{s: s, xid: xid, blocks: blocks, cnt: cnt, nextID: nextID})
	}
	return out
}

// metaRecs encodes the snapshot-metadata tree: SNAP_METADATA records keyed by
// the snapshot xid, then the SNAP_NAME records (key: the name; value: the xid).
func metaRecs(snaps []*snapTree) []Rec {
	type keyed struct {
		id   uint64
		name []byte
		rec  Rec
	}
	var metas, names []keyed
	for _, st := range snaps {
		name := st.s.name()
		k := make([]byte, 8)
		le.PutUint64(k, st.xid&idMask|typeSnapMetadata<<60)
		v := make([]byte, snapMetaValFixed+len(name)+1)
		le.PutUint64(v[8:], st.sblock)
		le.PutUint64(v[16:], orDefault(st.s.CreateTime))
		le.PutUint64(v[24:], orDefault(st.s.ChangeTime))
		le.PutUint16(v[48:], uint16(len(name)+1))
		copy(v[snapMetaValFixed:], name)
		if st.s.MutateMeta != nil {
			k, v = st.s.MutateMeta(k, v)
		}
		metas = append(metas, keyed{id: st.xid, rec: Rec{Key: k, Val: v}})
		if st.s.NoNameRecord {
			continue
		}
		nk := make([]byte, 8+2+len(name)+1)
		le.PutUint64(nk, snapNameID|typeSnapName<<60)
		le.PutUint16(nk[8:], uint16(len(name)+1))
		copy(nk[10:], name)
		nv := make([]byte, 8)
		le.PutUint64(nv, st.xid)
		names = append(names, keyed{name: name, rec: Rec{Key: nk, Val: nv}})
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].id < metas[j].id })
	sort.SliceStable(names, func(i, j int) bool { return string(names[i].name) < string(names[j].name) })
	var recs []Rec
	for _, m := range metas {
		recs = append(recs, m.rec)
	}
	for _, n := range names {
		recs = append(recs, n.rec)
	}
	return recs
}

// omapSnaps are the records of the volume object map's snapshot tree.
func omapSnaps(snaps []*snapTree) []OmapSnap {
	var out []OmapSnap
	for _, st := range snaps {
		if st.s.NoOmapSnapshot {
			continue
		}
		var flags uint32
		if st.s.Deleted {
			flags |= omapSnapDeleted
		}
		if st.s.Reverted {
			flags |= omapSnapReverted
		}
		out = append(out, OmapSnap{Xid: st.xid, Flags: flags, Oid: st.xid + 1000})
	}
	return out
}

// writeSnapSuper writes the volume-superblock copy of a snapshot into b.
func (v Volume) writeSnapSuper(b []byte, vg VolumeGeo, st *snapTree, rootOid uint64) {
	sv := v
	sv.Snapshots = nil
	sv.writeSuper(b, vg, rootOid, st.cnt, st.nextID)
	putObjHeader(b, st.sblock, st.xid, flagPhysical|typeVolume, 0)
	if st.s.MutateSblock != nil {
		st.s.MutateSblock(b)
	}
	sealBlock(b)
	if st.s.BadSblock {
		b[100] ^= 0xff // after sealing: the checksum no longer matches
	}
}
