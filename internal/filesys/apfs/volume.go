package apfs

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Volume superblock (apfs_superblock_t) offsets and flags (Apple File System
// Reference; offsets computed from the field order).
const (
	typeFS    = 0xd // OBJECT_TYPE_FS
	apfsMagic = "APSB"

	vsFsIndex       = 36
	vsFeatures      = 40
	vsRoCompat      = 48
	vsIncompat      = 56
	vsRootTreeType  = 116
	vsOmapOid       = 128
	vsRootTreeOid   = 136
	vsRevertToXid   = 160
	vsNumSnapshots  = 216
	vsUUID          = 240
	vsLastModTime   = 256
	vsFsFlags       = 264
	vsVolname       = 704
	vsVolnameLen    = 256
	vsRole          = 964
	vsRootToXid     = 968
	vsSuperblockLen = 976 // bytes needed through apfs_root_to_xid

	fsFlagUnencrypted = 0x1

	incompatCaseInsensitive = 0x1
	incompatNormInsensitive = 0x8
	incompatSealed          = 0x20
	// knownVolIncompat: CASE_INSENSITIVE, DATALESS_SNAPS, ENC_ROLLED,
	// NORMALIZATION_INSENSITIVE, INCOMPLETE_RESTORE, SEALED_VOLUME.
	knownVolIncompat = 0x3f

	rawPrefix = "~raw~" // display form of a name that cannot be shown as it is
)

// volume is one volume of the container, read from its superblock at Open.
// The object map and the file-system tree are opened lazily and never for an
// encrypted volume.
type volume struct {
	slot  int    // index in nx_fs_oid
	oid   uint64 // virtual oid of the superblock
	paddr uint64 // block of the superblock

	readable bool // the superblock could be read and checked

	name      []byte // apfs_volname without the NUL
	display   string // the name shown (unique among the volumes)
	rawName   []byte // the stored name when it differs from display
	uuid      [16]byte
	features  uint64
	roCompat  uint64
	incompat  uint64
	fsFlags   uint64
	role      uint16
	fsIndex   uint32
	omapOid   uint64
	rootOid   uint64
	rootType  uint32
	snapshots uint64
	lastMod   uint64

	encrypted bool
	sealed    bool
	caseInsen bool
	normInsen bool

	mu   sync.Mutex // guards tree
	tree *tree      // the live fs tree, once opened
}

// folds reports whether name lookups compare case-insensitively (the volume is
// case- or normalization-insensitive).
func (v *volume) folds() bool { return v.caseInsen || v.normInsen }

// encryptedErr is the error for every access below an encrypted volume.
func (v *volume) encryptedErr() error {
	return fmt.Errorf("apfs: volume %d %q: %w", v.slot, v.display, filesys.ErrEncrypted)
}

// displayName returns the Entry.Name for a stored name and the RawName to keep:
// a name that is valid UTF-8, non-empty, without '/' or NUL and not "." or ".."
// is shown as it is; anything else as "~raw~" + base64url(raw).
func displayName(raw []byte) (string, []byte) {
	if plainName(raw) {
		return string(raw), nil
	}
	return rawPrefix + base64.RawURLEncoding.EncodeToString(raw), slices.Clone(raw)
}

func plainName(raw []byte) bool {
	if len(raw) == 0 || !utf8.Valid(raw) || bytes.ContainsAny(raw, "\x00/") {
		return false
	}
	return string(raw) != "." && string(raw) != ".."
}

// rawAlias decodes a "~raw~" display form; ok is false when s is not one in
// canonical form (it must re-encode to exactly s).
func rawAlias(s string) (raw []byte, ok bool) {
	rest, found := strings.CutPrefix(s, rawPrefix)
	if !found {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != rest {
		return nil, false
	}
	return b, true
}

// roleName names a volume role (informational).
func roleName(r uint16) string {
	if r == 0 {
		return "none"
	}
	low := map[uint16]string{1: "system", 2: "user", 4: "recovery", 8: "vm", 0x10: "preboot", 0x20: "installer"}
	if n, ok := low[r]; ok {
		return n
	}
	if r&0x3f == 0 { // n << 6
		hi := map[uint16]string{1: "data", 2: "baseband", 3: "update", 4: "xart", 5: "hardware", 6: "backup", 9: "enterprise", 11: "prelogin"}
		if n, ok := hi[r>>6]; ok {
			return n
		}
	}
	return fmt.Sprintf("%#x", r)
}

// openVolumes reads every populated nx_fs_oid slot. A volume that cannot be
// read stays in the list (unreadable) with a warning.
func (f *FS) openVolumes() {
	n := int(f.nx.maxFS)
	for i := range nxMaxFileSystems {
		oid := f.nx.fsOid[i]
		if oid == 0 {
			continue
		}
		if i >= n {
			f.warn("file system slot %d holds oid %d beyond nx_max_file_systems (%d): ignored", i, oid, n)
			continue
		}
		v := f.readVolume(i, oid)
		f.vols = append(f.vols, v)
		f.slots[i] = v
	}
	// Display names: unique, in slot order.
	used := map[string]bool{}
	for _, v := range f.vols {
		if !v.readable {
			v.display = "~unreadable~" + strconv.Itoa(v.slot)
			used[v.display] = true
			continue
		}
		disp, raw := displayName(v.name)
		for used[disp] {
			disp += "~" + strconv.Itoa(v.slot)
			if raw == nil {
				raw = slices.Clone(v.name)
			}
		}
		used[disp] = true
		v.display, v.rawName = disp, raw
	}
}

// readVolume reads the volume superblock of slot i with virtual oid oid.
func (f *FS) readVolume(slot int, oid uint64) *volume {
	v := &volume{slot: slot, oid: oid}
	fail := func(format string, a ...any) *volume {
		f.warn("volume %d (oid %d) is unreadable: %s", slot, oid, fmt.Sprintf(format, a...))
		return v
	}
	paddr, size, flags, err := f.cmap.resolve(oid)
	if err != nil {
		return fail("%v", err)
	}
	v.paddr = paddr
	if flags&(omapValEncrypted|omapValNoHeader) != 0 {
		return fail("its object map entry has flags %#x", flags)
	}
	if int64(size) != int64(f.bs) {
		return fail("its object map entry maps %d bytes, want one %d-byte block", size, f.bs)
	}
	buf, h, err := f.readObjectRaw(paddr, f.bs)
	if err != nil {
		return fail("%v", err)
	}
	if !checksumOK(buf) {
		f.warn("volume %d superblock at block %d has a bad checksum; it is used with every bound checked", slot, paddr)
	}
	if h.kind() != typeFS {
		return fail("block %d has object type %#x, want a volume superblock", paddr, h.kind())
	}
	if string(buf[32:36]) != apfsMagic || len(buf) < vsSuperblockLen {
		return fail("block %d has no %q magic", paddr, apfsMagic)
	}
	v.readable = true
	v.fsIndex = le.Uint32(buf[vsFsIndex:])
	v.features = le.Uint64(buf[vsFeatures:])
	v.roCompat = le.Uint64(buf[vsRoCompat:])
	v.incompat = le.Uint64(buf[vsIncompat:])
	v.rootType = le.Uint32(buf[vsRootTreeType:])
	v.omapOid = le.Uint64(buf[vsOmapOid:])
	v.rootOid = le.Uint64(buf[vsRootTreeOid:])
	v.snapshots = le.Uint64(buf[vsNumSnapshots:])
	copy(v.uuid[:], buf[vsUUID:vsUUID+16])
	v.lastMod = le.Uint64(buf[vsLastModTime:])
	v.fsFlags = le.Uint64(buf[vsFsFlags:])
	v.role = le.Uint16(buf[vsRole:])
	name := buf[vsVolname : vsVolname+vsVolnameLen]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	} else {
		f.warn("volume %d: the volume name is not NUL-terminated within %d bytes", slot, vsVolnameLen)
	}
	v.name = slices.Clone(name)
	v.encrypted = v.fsFlags&fsFlagUnencrypted == 0
	v.caseInsen = v.incompat&incompatCaseInsensitive != 0
	// A case-insensitive volume is always normalization-insensitive: mkapfs sets
	// CASE_INSENSITIVE alone for its default (insensitive) volume and clears both
	// bits for a normalization-sensitive one (-z). Measured against real containers.
	v.normInsen = v.incompat&(incompatNormInsensitive|incompatCaseInsensitive) != 0
	v.sealed = v.incompat&incompatSealed != 0

	if int64(v.fsIndex) != int64(slot) {
		f.warn("volume in slot %d records apfs_fs_index %d", slot, v.fsIndex)
	}
	if u := v.incompat &^ knownVolIncompat; u != 0 {
		f.warn("volume %d has unknown incompatible feature bits %#x", slot, u)
	}
	if v.sealed {
		f.warn("volume %d is sealed: its integrity hashes are not verified", slot)
	}
	if x := le.Uint64(buf[vsRootToXid:]); x != 0 {
		f.warn("volume %d is rooted at xid %d (a snapshot revert in progress): shown as found", slot, x)
	}
	if x := le.Uint64(buf[vsRevertToXid:]); x != 0 {
		f.warn("volume %d has a pending revert to xid %d: shown as found", slot, x)
	}
	return v
}

// fsTree opens (once) the live file-system tree of v through the volume's
// object map. An encrypted volume is never opened: the error is ErrEncrypted.
func (f *FS) fsTree(v *volume, view uint64) (*tree, error) {
	if v.encrypted {
		return nil, v.encryptedErr()
	}
	if !v.readable {
		return nil, corrupt("volume", int64(v.paddr)*int64(f.bs), "volume %d is unreadable", v.slot)
	}
	if !f.viewKnown(v, view) {
		return nil, fmt.Errorf("apfs: volume %d view %d: %w", v.slot, view, filesys.ErrNotFound)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tree != nil {
		return v.tree, nil
	}
	if v.omapOid == 0 || v.rootOid == 0 {
		return nil, corrupt("volume", int64(v.paddr)*int64(f.bs), "volume %d has no object map or file-system tree", v.slot)
	}
	o, err := f.openOmap(v.omapOid)
	if err != nil {
		return nil, fmt.Errorf("apfs: volume %d object map: %w", v.slot, err)
	}
	t, err := f.openTree(v.rootOid, v.rootType, o)
	if err != nil {
		return nil, fmt.Errorf("apfs: volume %d file-system tree: %w", v.slot, err)
	}
	v.tree = t
	return t, nil
}

// viewKnown reports whether view names a view of v: 0 is the live tree; the
// snapshot views are added with the snapshot layer.
func (f *FS) viewKnown(_ *volume, view uint64) bool { return view == 0 }

// featureLine is the Info.Features line of the volume.
func (v *volume) featureLine() string {
	if !v.readable {
		return fmt.Sprintf("vol[%d] unreadable", v.slot)
	}
	s := fmt.Sprintf("vol[%d] %q: role=%s", v.slot, v.display, roleName(v.role))
	if v.caseInsen {
		s += ", case-insensitive"
	}
	if v.normInsen {
		s += ", normalization-insensitive"
	}
	if v.encrypted {
		s += ", encrypted"
	} else {
		s += ", unencrypted"
	}
	if v.sealed {
		s += ", sealed"
	}
	return s + fmt.Sprintf(", snapshots=%d", v.snapshots)
}

// volumeEntry is the directory entry of the volume's root directory. Times come
// from the root inode when it can be read (never for an encrypted volume),
// else Modified from apfs_last_mod_time.
func (f *FS) volumeEntry(v *volume) filesys.Entry {
	e := filesys.Entry{
		Name: v.display, RawName: slices.Clone(v.rawName),
		ID: nodeID(v.slot, 0, rootIno), Type: filesys.TypeDir,
		Encrypted: v.encrypted,
		Attrs:     []filesys.KV{{Key: "volume", Value: strconv.Itoa(v.slot)}},
	}
	add := func(k, val string) { e.Attrs = append(e.Attrs, filesys.KV{Key: k, Value: val}) }
	if !v.readable {
		add("unreadable", "true")
		return e
	}
	add("uuid", formatUUID(v.uuid))
	add("role", roleName(v.role))
	add("case_insensitive", strconv.FormatBool(v.caseInsen))
	add("normalization_insensitive", strconv.FormatBool(v.normInsen))
	add("encrypted", strconv.FormatBool(v.encrypted))
	add("snapshots", strconv.FormatUint(v.snapshots, 10))
	if v.sealed {
		add("sealed", "true")
	}
	if !v.encrypted {
		if in, err := f.inode(v, 0, rootIno); err == nil {
			e.Mode, e.UID, e.GID = uint32(in.mode), in.uid, in.gid
			e.Times = inodeTimes(in)
			return e
		} else if isNotFoundOrCorrupt(err) {
			f.warn("volume %d: the root inode cannot be read: %v", v.slot, err)
		}
	}
	if v.lastMod != 0 {
		if ts, ok := timestamp(v.lastMod); ok {
			e.Times.Modified = ts
		}
	}
	return e
}

func inodeTimes(in *inode) filesys.Times {
	var t filesys.Times
	t.Modified, _ = timestamp(in.mod)
	t.Accessed, _ = timestamp(in.access)
	t.Changed, _ = timestamp(in.change)
	t.Created, _ = timestamp(in.create)
	return t
}
