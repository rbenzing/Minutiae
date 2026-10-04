package apfs

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Snapshots (Apple File System Reference; the Format reference of the project
// plan). The volume's snapshot-metadata tree has a SNAP_METADATA record per
// snapshot (key id: the snapshot xid; value: j_snap_metadata_val_t) and a
// SNAP_NAME record per name (key: the name; value: the xid). The record layouts
// below are from memory of the reference and are checked only against the
// builder, not against a real image.
const (
	fsTypeSnapMetadata = 1
	fsTypeSnapName     = 11

	// j_snap_metadata_val_t: extentref_tree_oid u64 @0, sblock_oid u64 @8,
	// create_time u64 @16, change_time u64 @24, inum u64 @32,
	// extentref_tree_type u32 @40, flags u32 @44, name_len u16 @48, name @50.
	snapMetaSblockOff = 8
	snapMetaCreateOff = 16
	snapMetaChangeOff = 24
	snapMetaFlagsOff  = 44
	snapMetaNameLen   = 48
	snapMetaFixed     = 50

	// omap_snapshot_t flags.
	omapSnapDeleted  = 0x1
	omapSnapReverted = 0x2

	snapshotsDirName = ".snapshots"

	maxSnapshots = maxOmapSnapshots // snapshots read per volume
	maxSnapNames = 20               // names an unknown-snapshot error lists
)

// snapshot is one usable snapshot of a volume.
type snapshot struct {
	xid            uint64
	name           []byte // stored name, without the NUL
	display        string // the name shown (unique among the volume's snapshots)
	raw            []byte // the stored name when it differs from display
	sblock         uint64 // block of the volume-superblock copy (0: none)
	create, change uint64 // nanoseconds since 1970
	flags          uint32
	omapMissing    bool // no record in the volume object map's snapshot tree
}

// snapList is the snapshots of one volume, in xid order.
type snapList struct {
	list  []*snapshot
	byXid map[uint64]*snapshot
}

// snapshotList returns the usable snapshots of v (read once, then cached). An
// encrypted volume has none: ErrEncrypted.
func (f *FS) snapshotList(v *volume) (*snapList, error) {
	if v.encrypted {
		return nil, v.encryptedErr()
	}
	if !v.readable {
		return nil, corrupt("volume", int64(v.paddr)*int64(f.bs), "volume %d is unreadable", v.slot)
	}
	v.snapMu.Lock()
	defer v.snapMu.Unlock()
	if v.snaps != nil {
		return v.snaps, nil
	}
	if v.snapErr != nil {
		return nil, v.snapErr
	}
	l, err := f.readSnapshots(v)
	if err != nil {
		// A damaged list stays damaged: remember it, so forged view ids and repeated
		// listings do not rescan it. An I/O failure and the exhaustion of the node
		// budget are not properties of the image and are retried.
		if isNotFoundOrCorrupt(err) && !errors.Is(err, errNodeBudget) {
			v.snapErr = err
		}
		return nil, err
	}
	v.snaps = l
	return l, nil
}

// readSnapshots reads the snapshot-metadata tree of v and cross-checks it with
// the snapshot tree of the volume's object map. A snapshot whose xid is outside
// 1..checkpoint xid, a duplicate xid, a record that does not parse, and a
// snapshot flagged DELETED or REVERTED in the object map are omitted with a
// warning; one missing from the object map's tree is kept, flagged
// omap_snapshot=missing, with a warning.
func (f *FS) readSnapshots(v *volume) (*snapList, error) {
	l := &snapList{byXid: map[uint64]*snapshot{}}
	if v.snapMetaOid == 0 {
		if v.snapshots > 0 {
			f.warn("volume %d reports %d snapshots but has no snapshot metadata tree", v.slot, v.snapshots)
		}
		return l, nil
	}
	o, err := f.volOmap(v)
	if err != nil {
		return nil, err
	}
	t, err := f.openTree(v.snapMetaOid, v.snapMetaType, o)
	if err != nil {
		return nil, fmt.Errorf("apfs: volume %d snapshot metadata tree: %w", v.slot, err)
	}
	var metas []*snapshot
	names := map[string]uint64{}
	err = t.scan(nil, func(key, val []byte) (bool, error) {
		k := le.Uint64(key)
		id, typ := k&jobjIDMask, uint8(k>>60)
		switch typ {
		case fsTypeSnapMetadata:
			if len(metas) >= maxSnapshots {
				f.warn("volume %d has more than %d snapshot metadata records: the rest are not read", v.slot, maxSnapshots)
				return true, nil
			}
			s, why := parseSnapMetadata(id, key, val)
			if why != "" {
				f.warn("volume %d: the snapshot metadata record of xid %d is skipped: %s", v.slot, id, why)
				return false, nil
			}
			metas = append(metas, s)
		case fsTypeSnapName:
			name, ok := snapNameKey(key)
			if !ok || len(val) < 8 {
				f.warn("volume %d: a snapshot name record is malformed and skipped", v.slot)
				return false, nil
			}
			if _, dup := names[string(name)]; !dup {
				names[string(name)] = le.Uint64(val)
			}
		default:
			f.warn("volume %d: a snapshot metadata tree record of type %d is ignored", v.slot, typ)
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("apfs: volume %d snapshot metadata tree: %w", v.slot, err)
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].xid < metas[j].xid })

	osnaps, oerr := o.snapshots()
	if oerr != nil {
		if errors.Is(oerr, errNodeBudget) || !isNotFoundOrCorrupt(oerr) {
			return nil, fmt.Errorf("apfs: volume %d object map snapshot tree: %w", v.slot, oerr)
		}
		f.warn("volume %d: the object map's snapshot tree cannot be read (%v): the snapshots are not cross-checked", v.slot, oerr)
	}
	omapFlags := map[uint64]uint32{}
	for _, s := range osnaps {
		omapFlags[s.xid] = s.flags
	}

	for _, s := range metas {
		switch _, dup := l.byXid[s.xid]; {
		case dup:
			f.warn("volume %d: more than one snapshot with xid %d: the later record is skipped", v.slot, s.xid)
			continue
		case s.xid < 1 || s.xid > f.nx.xid:
			f.warn("volume %d: snapshot %q has xid %d, outside 1..%d (the checkpoint xid): skipped", v.slot, s.name, s.xid, f.nx.xid)
			continue
		}
		if oerr == nil {
			flags, ok := omapFlags[s.xid]
			switch {
			case ok && flags&(omapSnapDeleted|omapSnapReverted) != 0:
				f.warn("volume %d: snapshot %q (xid %d) is flagged %#x (deleted or reverted) in the object map's snapshot tree: omitted", v.slot, s.name, s.xid, flags)
				continue
			case !ok:
				s.omapMissing = true
				f.warn("volume %d: snapshot %q (xid %d) has no record in the object map's snapshot tree: listed as found", v.slot, s.name, s.xid)
			}
		}
		if x, ok := names[string(s.name)]; !ok {
			f.warn("volume %d: snapshot %q (xid %d) has no name record", v.slot, s.name, s.xid)
		} else if x != s.xid {
			f.warn("volume %d: the name record of snapshot %q says xid %d, its metadata xid %d", v.slot, s.name, x, s.xid)
		}
		l.list = append(l.list, s)
		l.byXid[s.xid] = s
	}

	used := map[string]bool{}
	for _, s := range l.list {
		disp, raw := displayName(s.name)
		for used[disp] {
			disp += "~" + strconv.FormatUint(s.xid, 10)
			if raw == nil {
				raw = slices.Clone(s.name)
			}
		}
		used[disp] = true
		s.display, s.raw = disp, raw
	}
	return l, nil
}

// parseSnapMetadata decodes a SNAP_METADATA record; why is set when it cannot
// be used.
func parseSnapMetadata(xid uint64, key, val []byte) (*snapshot, string) {
	if len(key) != 8 {
		return nil, fmt.Sprintf("a key of %d bytes is not a snapshot metadata key", len(key))
	}
	if len(val) < snapMetaFixed {
		return nil, fmt.Sprintf("a value of %d bytes is shorter than the %d fixed bytes", len(val), snapMetaFixed)
	}
	nl := int(le.Uint16(val[snapMetaNameLen:]))
	if nl < 1 || nl > len(val)-snapMetaFixed || val[snapMetaFixed+nl-1] != 0 {
		return nil, "the name length does not fit the value or the name is not NUL-terminated"
	}
	name := val[snapMetaFixed : snapMetaFixed+nl-1]
	if len(name) == 0 || len(name) > maxNameLen || bytes.IndexByte(name, 0) >= 0 {
		return nil, "the name is empty, longer than 255 bytes or holds a NUL"
	}
	return &snapshot{
		xid: xid, name: bytes.Clone(name),
		sblock: le.Uint64(val[snapMetaSblockOff:]),
		create: le.Uint64(val[snapMetaCreateOff:]), change: le.Uint64(val[snapMetaChangeOff:]),
		flags: le.Uint32(val[snapMetaFlagsOff:]),
	}, ""
}

// snapNameKey returns the name of a SNAP_NAME key (header, u16 name_len with
// the NUL, name).
func snapNameKey(key []byte) ([]byte, bool) {
	if len(key) < 8+2+1 {
		return nil, false
	}
	n := int(le.Uint16(key[8:]))
	if n < 1 || 10+n != len(key) || key[len(key)-1] != 0 {
		return nil, false
	}
	return key[10 : len(key)-1], true
}

// volOmap opens (once) the volume's object map.
func (f *FS) volOmap(v *volume) (*omapView, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return f.volOmapLocked(v)
}

// volOmapLocked is volOmap with v.mu held.
func (f *FS) volOmapLocked(v *volume) (*omapView, error) {
	if v.omap != nil {
		return v.omap, nil
	}
	if v.omapOid == 0 {
		return nil, corrupt("volume", int64(v.paddr)*int64(f.bs), "volume %d has no object map", v.slot)
	}
	o, err := f.openOmap(v.omapOid)
	if err != nil {
		return nil, fmt.Errorf("apfs: volume %d object map: %w", v.slot, err)
	}
	v.omap = o
	return o, nil
}

// viewKnown reports whether view names a view of v: 0 is the live tree, any
// other is the xid of a usable snapshot.
func (f *FS) viewKnown(v *volume, view uint64) (bool, error) {
	if view == 0 {
		return true, nil
	}
	l, err := f.snapshotList(v)
	if err != nil {
		return false, err
	}
	return l.byXid[view] != nil, nil
}

// snapTree opens (once) the fs tree of snapshot xid of v: the root oid comes
// from the volume-superblock copy the snapshot's metadata names (type FS, magic
// and checksum are checked; when it cannot be used the live volume's root oid
// is used, with a warning), resolved through the volume object map as of the
// snapshot's xid, like every child node.
func (f *FS) snapTree(v *volume, xid uint64) (*tree, error) {
	l, err := f.snapshotList(v)
	if err != nil {
		return nil, err
	}
	s := l.byXid[xid]
	if s == nil {
		return nil, fmt.Errorf("apfs: volume %d view %d: %w", v.slot, xid, filesys.ErrNotFound)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if t := v.snapTrees[xid]; t != nil {
		return t, nil
	}
	o, err := f.volOmapLocked(v)
	if err != nil {
		return nil, err
	}
	rootOid, rootType, err := f.snapshotRoot(v, s)
	if err != nil {
		return nil, err
	}
	view := *o
	view.xid = xid
	t, err := f.openTree(rootOid, rootType, &view)
	if err != nil {
		return nil, fmt.Errorf("apfs: volume %d snapshot %q (xid %d) file-system tree: %w", v.slot, s.name, xid, err)
	}
	if v.snapTrees == nil {
		v.snapTrees = map[uint64]*tree{}
	}
	v.snapTrees[xid] = t
	return t, nil
}

// snapshotRoot returns the root tree oid and type of snapshot s.
func (f *FS) snapshotRoot(v *volume, s *snapshot) (uint64, uint32, error) {
	fallback := func(format string, a ...any) (uint64, uint32, error) {
		f.warn("volume %d snapshot %q (xid %d): "+format+"; the live root tree oid is used", append([]any{v.slot, s.name, s.xid}, a...)...)
		return v.rootOid, v.rootType, nil
	}
	if s.sblock == 0 {
		return fallback("it names no volume superblock copy")
	}
	buf, h, err := f.readObjectRaw(s.sblock, f.bs)
	if err != nil {
		if !isNotFoundOrCorrupt(err) && !isShort(err) {
			return 0, 0, err
		}
		return fallback("its volume superblock copy at block %d cannot be read (%v)", s.sblock, err)
	}
	switch {
	case !checksumOK(buf):
		return fallback("its volume superblock copy at block %d has a bad checksum", s.sblock)
	case h.kind() != typeFS:
		return fallback("block %d (its volume superblock copy) has object type %#x, want %#x", s.sblock, h.kind(), typeFS)
	case len(buf) < vsSuperblockLen || string(buf[32:36]) != apfsMagic:
		return fallback("block %d (its volume superblock copy) has no %q magic", s.sblock, apfsMagic)
	}
	oid := le.Uint64(buf[vsRootTreeOid:])
	if oid == 0 {
		return fallback("its volume superblock copy has no root tree")
	}
	// The copy verifies, so what it says is checked against what it must be: a
	// valid superblock of another object, volume or time (or a tree that is not an
	// ordinary virtual one) must not steer this view. The snapshot is refused.
	refuse := func(format string, a ...any) (uint64, uint32, error) {
		return 0, 0, corrupt("snapshot volume superblock", int64(s.sblock)*int64(f.bs), "volume %d snapshot %q (xid %d): its volume superblock copy at block %d %s",
			v.slot, s.name, s.xid, s.sblock, fmt.Sprintf(format, a...))
	}
	rootType := le.Uint32(buf[vsRootTreeType:])
	switch {
	case h.oid != s.sblock:
		return refuse("carries oid %d, not its block address", h.oid)
	case h.xid > s.xid:
		return refuse("is from transaction %d, after the snapshot (xid %d)", h.xid, s.xid)
	case le.Uint32(buf[vsFsIndex:]) != v.fsIndex:
		return refuse("belongs to volume index %d, not %d", le.Uint32(buf[vsFsIndex:]), v.fsIndex)
	case rootType&storageMask != 0:
		return refuse("has a root tree of storage type %#x, want a virtual tree", rootType&storageMask)
	}
	return oid, rootType, nil
}

// snapsEntry is the synthetic .snapshots directory of v.
func (f *FS) snapsEntry(v *volume) (filesys.Entry, error) {
	e := filesys.Entry{
		Name: snapshotsDirName, ID: "snaps:" + strconv.Itoa(v.slot), Type: filesys.TypeDir,
		Attrs: []filesys.KV{{Key: "synthetic", Value: "snapshots"}},
	}
	l, err := f.snapshotList(v)
	switch {
	case err == nil:
		e.Attrs = append(e.Attrs, filesys.KV{Key: "snapshots", Value: strconv.Itoa(len(l.list))})
	case errors.Is(err, errNodeBudget) || !isNotFoundOrCorrupt(err):
		return filesys.Entry{}, err
	default:
		f.warn("volume %d: the snapshots cannot be listed: %v", v.slot, err)
		e.Attrs = append(e.Attrs, filesys.KV{Key: "snapshots", Value: "unreadable"})
	}
	return e, nil
}

// snapshotEntry is the root directory of snapshot s: its name, the root
// inode's mode and times (the metadata's create and change times when that
// inode cannot be read) and attributes naming the snapshot.
func (f *FS) snapshotEntry(v *volume, s *snapshot) (filesys.Entry, error) {
	e := filesys.Entry{
		Name: s.display, RawName: slices.Clone(s.raw),
		ID: nodeID(v.slot, s.xid, rootIno), Type: filesys.TypeDir,
	}
	in, err := f.inode(v, s.xid, rootIno)
	switch {
	case err == nil:
		e.Mode, e.UID, e.GID = uint32(in.mode), in.uid, in.gid
		e.Times = inodeTimes(in)
	case errors.Is(err, errNodeBudget) || (!isNotFoundOrCorrupt(err) && !errors.Is(err, filesys.ErrUnsupported)):
		return filesys.Entry{}, err
	default:
		f.warn("volume %d snapshot %q (xid %d): the root inode cannot be read: %v", v.slot, s.name, s.xid, err)
		e.Times.Created, _ = timestamp(s.create)
		e.Times.Changed, _ = timestamp(s.change)
	}
	add := func(k, val string) { e.Attrs = append(e.Attrs, filesys.KV{Key: k, Value: val}) }
	add("snapshot", "true")
	add("snapshot_xid", strconv.FormatUint(s.xid, 10))
	add("snapshot_created", snapshotTime(s.create))
	add("snapshot_changed", snapshotTime(s.change))
	if s.omapMissing {
		add("omap_snapshot", "missing")
	}
	return e, nil
}

// snapshotTime formats nanoseconds since 1970 as RFC 3339 (UTC).
func snapshotTime(ns uint64) string {
	ts, ok := timestamp(ns)
	if !ok {
		return "invalid"
	}
	return ts.T.UTC().Format(time.RFC3339Nano)
}

// listSnapshots lists the snapshot root directories of v (ReadDir of snaps:<n>).
func (f *FS) listSnapshots(v *volume) ([]filesys.Entry, error) {
	l, err := f.snapshotList(v)
	if err != nil {
		return nil, err
	}
	out := make([]filesys.Entry, 0, len(l.list))
	for _, s := range l.list {
		e, err := f.snapshotEntry(v, s)
		if err != nil {
			if errors.Is(err, errNodeBudget) && len(out) > 0 {
				f.warn("volume %d: the node read budget is exhausted; the snapshot listing is partial (%d entries)", v.slot, len(out))
				return out, nil
			}
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// snapshotChild finds the snapshot of v named comp: by display name, then by
// its "~raw~" alias.
func (f *FS) snapshotChild(v *volume, comp string) (filesys.Entry, error) {
	s, err := f.matchSnapshot(v, comp)
	if err != nil {
		return filesys.Entry{}, err
	}
	if s == nil {
		return filesys.Entry{}, errNoChild
	}
	return f.snapshotEntry(v, s)
}

func (f *FS) matchSnapshot(v *volume, comp string) (*snapshot, error) {
	l, err := f.snapshotList(v)
	if err != nil {
		return nil, err
	}
	for _, s := range l.list {
		if s.display == comp {
			return s, nil
		}
	}
	if raw, ok := rawAlias(comp); ok {
		for _, s := range l.list {
			if bytes.Equal(s.name, raw) {
				return s, nil
			}
		}
	}
	return nil, nil
}

// SnapshotPath maps a path inside a volume ("/Data/docs/a.txt"; "/" alone only
// when the container has exactly one volume) to the path of the same file in
// the named snapshot ("/Data/.snapshots/<snapshot>/docs/a.txt"). The snapshot
// is named by its display name or its "~raw~" alias. It implements
// filesys.Snapshotter. The path itself is not looked up.
func (f *FS) SnapshotPath(p, snapshot string) (string, error) {
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			comps = append(comps, c)
		}
	}
	var v *volume
	if len(comps) == 0 {
		if len(f.vols) != 1 {
			return "", fmt.Errorf("apfs: %w: the container has %d volumes", filesys.ErrNeedsVolume, len(f.vols))
		}
		v = f.vols[0]
	} else {
		if v = f.matchVolume(comps[0]); v == nil {
			return "", notFound("no volume %q", comps[0])
		}
		comps = comps[1:]
	}
	s, err := f.matchSnapshot(v, snapshot)
	if err != nil {
		return "", err
	}
	if s == nil {
		l, err := f.snapshotList(v)
		if err != nil {
			return "", err
		}
		names := make([]string, 0, maxSnapNames)
		for _, c := range l.list {
			if len(names) == maxSnapNames {
				break
			}
			names = append(names, strconv.Quote(c.display))
		}
		more := ""
		if n := len(l.list) - len(names); n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		return "", notFound("volume %q has no snapshot %q (snapshots: %s%s)", v.display, snapshot, strings.Join(names, ", "), more)
	}
	out := "/" + v.display + "/" + snapshotsDirName + "/" + s.display
	if len(comps) > 0 {
		out += "/" + strings.Join(comps, "/")
	}
	return out, nil
}

// EntrySnapshot implements filesys.SnapshotViewer: an entry whose ID names a
// snapshot view (n:<volume>:<xid>:<inode>, xid not 0) of a usable snapshot is
// reported with the snapshot's display name and xid. Nothing but the ID is
// trusted; a view that is not a usable snapshot is not one.
func (f *FS) EntrySnapshot(e filesys.Entry) (string, uint64, bool) {
	kind, vol, view, _, ok := parseEntryID(e.ID)
	if !ok || kind != idNode || view == 0 || vol >= len(f.slots) || f.slots[vol] == nil {
		return "", 0, false
	}
	l, err := f.snapshotList(f.slots[vol])
	if err != nil {
		return "", 0, false
	}
	s := l.byXid[view]
	if s == nil {
		return "", 0, false
	}
	return s.display, s.xid, true
}

var _ filesys.SnapshotViewer = (*FS)(nil)
