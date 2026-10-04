package apfstest

import (
	"sort"
	"strings"
)

const (
	maxVolumes = 100

	// volXid is the transaction id of every volume object the builder writes: the
	// entries of the container and volume object maps carry it, so it is at or
	// below every checkpoint xid a test builds.
	volXid = 1

	volumeOidBase = 1100  // virtual oid of the volume superblock of slot 0
	fsOidBase     = 65536 // first virtual oid of a volume's fs-tree nodes
	fsOidStride   = 65536 // fs-tree node oids per volume

	typeVolume   = 0xd // OBJECT_TYPE_FS
	rootTreeType = 2   // OBJECT_TYPE_BTREE, virtual

	// apfs_fs_flags and incompatible features.
	fsUnencrypted      = 0x1
	incompatCaseInsens = 0x1
	incompatNormInsens = 0x8
	incompatSealed     = 0x20
	featHardlinkMaps   = 0x2

	volNameOff = 704
)

// VolumeOid is the virtual oid the builder gives the superblock of the volume
// in slot i (nx_fs_oid[i]).
func VolumeOid(i int) uint64 { return uint64(volumeOidBase + i) }

// Volume describes one volume of a container.
type Volume struct {
	Name            string
	RawName         []byte // apfs_volname bytes when set (overrides Name)
	UUID            [16]byte
	Role            uint16
	Encrypted       bool // APFS_FS_UNENCRYPTED clear
	CaseInsensitive bool
	NormInsensitive bool
	HashedKeys      bool // directory records use the hashed key form
	Sealed          bool // SEALED_VOLUME and hashed index nodes
	Files           []File

	// Incompat adds incompatible-feature bits; NumSnapshots, LastModTime and
	// TreeMaxKeys set apfs_num_snapshots, apfs_last_mod_time and the records per
	// fs-tree node (0: as many as fit; a small value forces a multi-level tree).
	Incompat     uint64
	NumSnapshots uint64
	LastModTime  uint64
	TreeMaxKeys  int

	// Extra adds records to the fs tree; Reorder, when set, rearranges the
	// sorted record list (to build out-of-order trees).
	Extra   []FSRecord
	Reorder func([]FSRecord) []FSRecord
}

// Times are the four inode times in nanoseconds since 1970; zero means the
// builder's default clock.
type Times struct{ Create, Modify, Change, Access uint64 }

// DefaultTime is the clock the builder uses for times left at zero.
const DefaultTime = 1_700_000_000_000_000_000

// Xattr is an extended attribute; Stream selects the data-stream form (the
// builder writes a stream descriptor and no data).
type Xattr struct {
	Name   string
	Value  []byte
	Stream bool
}

// File describes one object of a volume. Missing parent directories are
// created.
type File struct {
	Path    string // "/dir/name" inside the volume
	RawName []byte // on-disk bytes of the last name component, when set
	Data    []byte // content: sets the dstream size (extents are written by the data task)
	Size    int64  // dstream size when Data is nil
	Dir     bool
	Symlink string // target; makes the file a symlink
	// SymlinkStream stores the symlink target as a data-stream xattr.
	SymlinkStream bool
	Mode          uint32 // permission bits, or a full mode when type bits are set
	UID, GID      uint32
	Times         Times
	Xattrs        []Xattr

	LinkTo      string // Path of the file this name is a hard link to
	LinkSibling bool   // the directory record names a sibling id, mapped to the inode

	CompressedFlag   bool   // UF_COMPRESSED in bsd_flags
	UncompressedSize uint64 // with INODE_HAS_UNCOMPRESSED_SIZE when non-zero
	CryptoID         uint64 // default_crypto_id of the dstream
	PrivateID        uint64 // private_id of the inode (default: its number)

	// Hostile or special cases.
	Ino         uint64 // explicit inode number
	NoInode     bool   // write the directory record only
	BadHash     bool   // wrong name hash in the directory record
	DrecType    uint16 // directory record type (0: derived from the mode)
	DrecFileID  uint64 // file_id of the directory record (0: the inode)
	Nlink       int32  // nlink / nchildren override (0: counted)
	ProtClass   uint32
	BsdFlags    uint32
	InternalFlg uint64
}

// VolumeGeo is where a volume's objects are. Addresses are block numbers.
type VolumeGeo struct {
	Slot         int
	Oid          uint64 // virtual oid of the superblock (nx_fs_oid)
	Super        uint64 // block of the superblock
	Omap         uint64 // omap_phys_t block of the volume object map
	OmapNodes    []uint64
	ExtentRef    uint64
	SnapMeta     uint64
	FsNodes      []uint64 // fs-tree node blocks, root first
	FsOids       []uint64 // their virtual oids, root first
	First, End   uint64   // the volume's blocks are [First, End)
	Inodes       map[string]uint64
	NextObjectID uint64
}

// buildVolume lays out volume v of the given slot from block first and
// returns its geometry and its blocks.
func (o Options) buildVolume(slot int, v Volume, first uint64) (VolumeGeo, []Block) {
	bs := o.BlockSize
	recs, inodes, cnt, nextID := v.compile()
	vg := VolumeGeo{Slot: slot, Oid: VolumeOid(slot), First: first, Inodes: inodes, NextObjectID: nextID}

	spec := TreeSpec{
		BlockSize: bs, BTFlags: 0x40, Storage: StorageVirtual, Xid: volXid,
		MaxKeys: v.TreeMaxKeys, Hashed: v.Sealed,
	}
	oidNext := uint64(fsOidBase + slot*fsOidStride)
	fsBlocks := PackTree(spec, toRecs(recs), func() (uint64, uint64) {
		oid := oidNext
		oidNext++
		return 0, oid
	})
	if oidNext > uint64(fsOidBase+(slot+1)*fsOidStride) {
		panic("apfstest: volume fs tree has too many nodes")
	}
	// The volume object map: one record per fs-tree node.
	omapRecs := func(addrs []uint64) []OmapEntry {
		es := make([]OmapEntry, len(fsBlocks))
		for i, b := range fsBlocks {
			es[i] = OmapEntry{Oid: b.Oid, Xid: volXid}
			if addrs != nil {
				es[i].Paddr = addrs[i]
			}
		}
		return es
	}
	omapSpec := OmapTreeSpec(bs, volXid, 0)
	nOmap := len(PackTree(omapSpec, OmapRecs(omapRecs(nil), bs), func() (uint64, uint64) { return 0, 0 }))

	next := first
	take := func() uint64 { a := next; next++; return a }
	vg.Super = take()
	vg.Omap = take()
	omapBase := next
	next += uint64(nOmap)
	vg.ExtentRef = take()
	vg.SnapMeta = take()
	addrs := make([]uint64, len(fsBlocks))
	for i := range fsBlocks {
		addrs[i] = take()
		fsBlocks[i].Addr = addrs[i]
		vg.FsNodes = append(vg.FsNodes, addrs[i])
		vg.FsOids = append(vg.FsOids, fsBlocks[i].Oid)
	}
	vg.End = next
	if next > uint64(o.Blocks) {
		panic("apfstest: container too small for its volumes")
	}

	omapNext := omapBase
	omapTree := PackTree(omapSpec, OmapRecs(omapRecs(addrs), bs), func() (uint64, uint64) {
		a := omapNext
		omapNext++
		return a, a
	})
	for _, b := range omapTree {
		vg.OmapNodes = append(vg.OmapNodes, b.Addr)
	}
	omapPhys := make([]byte, bs)
	OmapPhys(omapPhys, vg.Omap, volXid, 0, 0, omapTree[0].Addr, 0)
	sealBlock(omapPhys)

	emptySpec := TreeSpec{BlockSize: bs, BTFlags: 0x50, Storage: StoragePhysical, Xid: volXid}
	emptyAt := func(addr uint64) Block {
		return PackTree(emptySpec, nil, func() (uint64, uint64) { return addr, addr })[0]
	}

	sb := make([]byte, bs)
	v.writeSuper(sb, vg, fsBlocks[0].Oid, cnt, nextID)
	sealBlock(sb)

	blocks := []Block{
		{Addr: vg.Super, Oid: vg.Oid, Data: sb},
		{Addr: vg.Omap, Oid: vg.Omap, Data: omapPhys},
		emptyAt(vg.ExtentRef), emptyAt(vg.SnapMeta),
	}
	blocks = append(blocks, omapTree...)
	blocks = append(blocks, fsBlocks...)
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Addr < blocks[j].Addr })
	if uint64(len(blocks)) != vg.End-vg.First {
		panic("apfstest: volume blocks are not contiguous")
	}
	return vg, blocks
}

type counts struct{ files, dirs, symlinks, other uint64 }

// writeSuper fills an apfs_superblock_t (offsets from the Format reference).
func (v Volume) writeSuper(b []byte, vg VolumeGeo, rootOid uint64, c counts, nextID uint64) {
	putObjHeader(b, vg.Oid, volXid, typeVolume, 0)
	copy(b[32:], "APSB")
	le.PutUint32(b[36:], uint32(vg.Slot))
	le.PutUint64(b[40:], featHardlinkMaps)
	incompat := v.Incompat
	if v.CaseInsensitive {
		incompat |= incompatCaseInsens
	}
	if v.NormInsensitive {
		incompat |= incompatNormInsens
	}
	if v.Sealed {
		incompat |= incompatSealed
	}
	le.PutUint64(b[56:], incompat)
	le.PutUint32(b[116:], rootTreeType)
	le.PutUint32(b[120:], flagPhysical|typeBTree)
	le.PutUint32(b[124:], flagPhysical|typeBTree)
	le.PutUint64(b[128:], vg.Omap)
	le.PutUint64(b[136:], rootOid)
	le.PutUint64(b[144:], vg.ExtentRef)
	le.PutUint64(b[152:], vg.SnapMeta)
	le.PutUint64(b[176:], nextID)
	le.PutUint64(b[184:], c.files)
	le.PutUint64(b[192:], c.dirs)
	le.PutUint64(b[200:], c.symlinks)
	le.PutUint64(b[208:], c.other)
	le.PutUint64(b[216:], v.NumSnapshots)
	copy(b[240:256], v.UUID[:])
	le.PutUint64(b[256:], v.LastModTime)
	var flags uint64
	if !v.Encrypted {
		flags |= fsUnencrypted
	}
	le.PutUint64(b[264:], flags)
	name := v.RawName
	if name == nil {
		name = []byte(v.Name)
	}
	if len(name) > 255 {
		panic("apfstest: volume name longer than 255 bytes")
	}
	copy(b[volNameOff:volNameOff+255], name)
	le.PutUint16(b[964:], v.Role)
}

func toRecs(recs []FSRecord) []Rec {
	out := make([]Rec, len(recs))
	for i, r := range recs {
		k := make([]byte, 8+len(r.Key))
		le.PutUint64(k, r.ID&idMask|uint64(r.Type)<<60)
		copy(k[8:], r.Key)
		out[i] = Rec{Key: k, Val: r.Val}
	}
	return out
}

// splitPath normalizes "/a//b/" and returns it with its parent and last
// component.
func splitPath(p string) (clean, parent, base string) {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		panic("apfstest: empty file path")
	}
	base = parts[len(parts)-1]
	parent = "/" + strings.Join(parts[:len(parts)-1], "/")
	clean = strings.TrimSuffix(parent, "/") + "/" + base
	return clean, parent, base
}
