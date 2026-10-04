package apfs

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// omap_phys_t layout and flags (Apple File System Reference).
const (
	omFlagsOff        = 32
	omSnapCountOff    = 36
	omTreeTypeOff     = 40
	omSnapTreeTypeOff = 44
	omTreeOidOff      = 48
	omSnapTreeOidOff  = 56
	omMostRecentSnap  = 64
	omapPhysSize      = 72 // through om_most_recent_snap

	omapManuallyManaged = 0x1
	omapEncrypting      = 0x2
	omapDecrypting      = 0x4
	omapKeyrolling      = 0x8
	omapCryptoGen       = 0x10
	omapKnownFlags      = omapManuallyManaged | omapEncrypting | omapDecrypting | omapKeyrolling | omapCryptoGen

	// omap_val_t flags.
	omapValDeleted   = 0x1
	omapValSaved     = 0x2
	omapValEncrypted = 0x4
	omapValNoHeader  = 0x8

	omapKeySize = 16 // omap_key_t {oid, xid}
	omapValSize = 16 // omap_val_t {flags, size, paddr}

	// omap_snapshot_t flags (DELETED 1, REVERTED 2) are reported as stored;
	// the snapshot layer interprets them.
	omapSnapKeySize = 8  // the snapshot xid
	omapSnapValSize = 16 // omap_snapshot_t {flags, pad, oid}

	// maxOmapSnapshots bounds the snapshot-tree records one omap may list.
	maxOmapSnapshots = 1 << 16
)

// omapView is an object map: the physical tree of (oid, xid) to location
// records, read as of a transaction id.
type omapView struct {
	f    *FS
	tree *tree
	xid  uint64 // lookups use the largest record xid <= this

	flags        uint32
	snapCount    uint32
	snapTreeType uint32
	snapTreeOid  uint64
	mostRecent   uint64
}

// omapSnap is one record of the snapshot tree.
type omapSnap struct {
	xid   uint64
	flags uint32
	oid   uint64
}

// openOmap reads the omap_phys_t at block paddr and opens its tree. The view
// is as of the selected checkpoint's xid (the xid field; a volume view sets
// its own).
func (f *FS) openOmap(paddr uint64) (*omapView, error) {
	buf, h, err := f.readObjectRaw(paddr, f.bs)
	if err != nil {
		return nil, err
	}
	if !checksumOK(buf) {
		f.warn("object map at block %d has a bad checksum; it is read with every bound checked", paddr)
	}
	if h.kind() != typeOmap {
		return nil, corrupt("object map", int64(paddr)*int64(f.bs), "block %d has object type %#x, want an object map", paddr, h.kind())
	}
	o := &omapView{
		f:            f,
		xid:          f.nx.xid,
		flags:        le.Uint32(buf[omFlagsOff:]),
		snapCount:    le.Uint32(buf[omSnapCountOff:]),
		snapTreeType: le.Uint32(buf[omSnapTreeTypeOff:]),
		snapTreeOid:  le.Uint64(buf[omSnapTreeOidOff:]),
		mostRecent:   le.Uint64(buf[omMostRecentSnap:]),
	}
	if u := o.flags &^ omapKnownFlags; u != 0 {
		f.warn("object map at block %d has unknown flag bits %#x", paddr, u)
	}
	treeType, treeOid := le.Uint32(buf[omTreeTypeOff:]), le.Uint64(buf[omTreeOidOff:])
	if treeOid == 0 {
		return nil, corrupt("object map", int64(paddr)*int64(f.bs), "object map at block %d has no tree", paddr)
	}
	// The tree that maps virtual oids cannot itself be virtual: whether its
	// root is physical follows the type's storage bits.
	t, err := f.openTree(treeOid, treeType, nil)
	if err != nil {
		return nil, err
	}
	if t.info.keySize != omapKeySize || t.info.valSize != omapValSize {
		return nil, corrupt("object map", int64(paddr)*int64(f.bs), "object map tree has key/value sizes %d/%d, want %d/%d", t.info.keySize, t.info.valSize, omapKeySize, omapValSize)
	}
	o.tree = t
	return o, nil
}

// resolve is lookup at the view's xid.
func (o *omapView) resolve(oid uint64) (paddr uint64, size, flags uint32, err error) {
	return o.lookup(oid, o.xid)
}

// lookup finds the record of oid with the largest xid not above xid (never a
// later one). A record flagged DELETED, or no record at all, is
// filesys.ErrNotFound. The result is the object's block, size and flags.
func (o *omapView) lookup(oid, xid uint64) (paddr uint64, size, flags uint32, err error) {
	var (
		found   bool
		bestXid uint64
		bestVal []byte
	)
	// Records sort by (oid, xid): everything of an oid below it is "before", the
	// oid's records up to xid are the range, later xids and larger oids are after.
	cmp := func(key []byte) int {
		koid := le.Uint64(key)
		switch {
		case koid < oid:
			return -1
		case koid > oid:
			return 1
		}
		if len(key) >= 16 && le.Uint64(key[8:]) > xid {
			return 1
		}
		return 0
	}
	err = o.tree.scan(cmp, func(key, val []byte) (bool, error) {
		if len(key) != omapKeySize || len(val) != omapValSize {
			return false, corrupt("object map", -1, "record of %d-byte key and %d-byte value in an object map", len(key), len(val))
		}
		if kx := le.Uint64(key[8:]); !found || kx >= bestXid {
			found, bestXid, bestVal = true, kx, val
		}
		return false, nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	if !found {
		return 0, 0, 0, fmt.Errorf("apfs: object %d at xid %d: %w", oid, xid, filesys.ErrNotFound)
	}
	flags = le.Uint32(bestVal)
	if flags&omapValDeleted != 0 {
		return 0, 0, 0, fmt.Errorf("apfs: object %d at xid %d was deleted: %w", oid, xid, filesys.ErrNotFound)
	}
	return le.Uint64(bestVal[8:]), le.Uint32(bestVal[4:]), flags, nil
}

// snapshots lists the records of the snapshot tree in xid order. An omap
// without a snapshot tree has none.
func (o *omapView) snapshots() ([]omapSnap, error) {
	if o.snapTreeOid == 0 {
		return nil, nil
	}
	t, err := o.f.openTree(o.snapTreeOid, o.snapTreeType, nil)
	if err != nil {
		return nil, err
	}
	var out []omapSnap
	err = t.scan(nil, func(key, val []byte) (bool, error) {
		if len(key) != omapSnapKeySize || len(val) != omapSnapValSize {
			return false, corrupt("object map snapshot tree", -1, "record of %d-byte key and %d-byte value", len(key), len(val))
		}
		s := omapSnap{xid: le.Uint64(key), flags: le.Uint32(val), oid: le.Uint64(val[8:])}
		if n := len(out); n > 0 && s.xid <= out[n-1].xid {
			return false, corrupt("object map snapshot tree", -1, "snapshot xid %d does not follow %d", s.xid, out[n-1].xid)
		}
		if len(out) >= maxOmapSnapshots {
			return false, corrupt("object map snapshot tree", -1, "more than %d snapshots", maxOmapSnapshots)
		}
		out = append(out, s)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
