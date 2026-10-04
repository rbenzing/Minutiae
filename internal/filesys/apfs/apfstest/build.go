// Package apfstest builds synthetic APFS containers for tests. It is a
// test-only helper: it encodes the author's reading of the format (see the
// Format reference of the 2F plan) with its own Fletcher-64, so builder tests
// alone cannot catch a misreading shared with the reader; the real containers
// under apfs/testdata are the independent check.
package apfstest

// Options configures Build. The zero value builds a 4096-block container with
// 4 KiB blocks and three checkpoints.
type Options struct {
	BlockSize   int // default 4096
	Blocks      int // container blocks, default 4096
	UUID        [16]byte
	Xid         uint64 // newest checkpoint xid, default 10
	Checkpoints int    // checkpoints written into the ring, default 3 (xids Xid, Xid-1, ...)
	DescBlocks  int    // descriptor ring blocks, default 16
	DataBlocks  int    // default 16
	CryptoSW    bool
	Volumes     []Volume // volumes, in nx_fs_oid slot order

	// Omap holds the records of the container object map tree; OmapMaxKeys
	// limits the records per node (0: as many as fit) to force a multi-level
	// tree; OmapSnaps are the records of its snapshot tree (none: no tree).
	Omap        []OmapEntry
	OmapMaxKeys int
	OmapSnaps   []OmapSnap

	// MapBlocks is the number of checkpoint-map blocks written before each
	// superblock (default 1). RingStart is the ring index where the OLDEST
	// checkpoint starts; later ones follow it, wrapping at the end of the ring.
	MapBlocks int
	RingStart int
}

func (o Options) norm() Options {
	if o.BlockSize == 0 {
		o.BlockSize = 4096
	}
	if o.Blocks == 0 {
		o.Blocks = 4096
	}
	if o.Xid == 0 {
		o.Xid = 10
	}
	if o.Checkpoints == 0 {
		o.Checkpoints = 3
	}
	if o.DescBlocks == 0 {
		o.DescBlocks = 16
	}
	if o.DataBlocks == 0 {
		o.DataBlocks = 16
	}
	if o.MapBlocks == 0 {
		o.MapBlocks = 1
	}
	return o
}

// Checkpoint is the geometry of one checkpoint in the ring.
type Checkpoint struct {
	Xid       uint64
	Index     int      // ring index of the superblock
	MapIndex  []int    // ring indexes of the mapping blocks, in order
	Super     uint64   // block address of the superblock
	Maps      []uint64 // block addresses of the mapping blocks, in order
	Spaceman  uint64   // block address of this checkpoint's spaceman copy (data area)
	DescIndex int      // nx_xp_desc_index recorded in the superblock
	DescLen   int      // nx_xp_desc_len recorded in the superblock
	DataIndex int      // nx_xp_data_index recorded in the superblock
	DataLen   int      // nx_xp_data_len recorded in the superblock
}

// Geo is where Build puts things. Addresses are block numbers.
type Geo struct {
	BlockSize, Blocks   int
	DescBase, DescCount uint64
	DataBase, DataCount uint64
	SpacemanOid         uint64
	Omap                uint64 // container object map (omap_phys_t)
	// OmapNodes are the blocks of the object map tree, root first; OmapSnapNodes
	// those of its snapshot tree (empty when there are no snapshots). Free is
	// the first block after them: tests may place their own objects from there.
	OmapNodes, OmapSnapNodes []uint64
	Free                     uint64
	// Volumes is where each volume was put, in slot order.
	Volumes []VolumeGeo
	// Checkpoints lists the checkpoints newest first.
	Checkpoints []Checkpoint
}

// layout is everything Build writes besides the checkpoint ring.
type layout struct {
	g    Geo
	tree []Block // container object map tree
	snap []Block // its snapshot tree
	vols [][]Block
	data []Block // file content
}

// layout computes where everything goes. Volumes follow the container object
// map; their superblocks are entered in the container object map as virtual
// oids.
func (o Options) layout() layout {
	o = o.norm()
	if o.Checkpoints*(o.MapBlocks+1) > o.DescBlocks {
		panic("apfstest: checkpoints do not fit the descriptor ring")
	}
	if o.Checkpoints > o.DataBlocks {
		panic("apfstest: checkpoints do not fit the data ring")
	}
	if uint64(o.Checkpoints) > o.Xid {
		panic("apfstest: Xid smaller than the number of checkpoints")
	}
	if o.RingStart < 0 || o.RingStart >= o.DescBlocks {
		panic("apfstest: RingStart outside the ring")
	}
	if len(o.Volumes) > maxVolumes {
		panic("apfstest: too many volumes")
	}
	g := Geo{
		BlockSize: o.BlockSize, Blocks: o.Blocks,
		DescBase: 1, DescCount: uint64(o.DescBlocks),
		SpacemanOid: oidSpaceman,
	}
	g.DataBase = g.DescBase + g.DescCount
	g.DataCount = uint64(o.DataBlocks)
	g.Omap = g.DataBase + g.DataCount
	if g.Omap >= uint64(o.Blocks) {
		panic("apfstest: container too small for its areas")
	}
	// The number of omap nodes depends on the number of records, not on the
	// block addresses in them: size the tree with placeholders, place the volumes
	// after it, then pack it again with the real addresses.
	entries := append([]OmapEntry(nil), o.Omap...)
	for i := range o.Volumes {
		entries = append(entries, OmapEntry{Oid: VolumeOid(i), Xid: volXid})
	}
	tree, snap := o.omapBlocks(g.Omap+1, entries)
	next := g.Omap + 1 + uint64(len(tree)+len(snap))
	var l layout
	// File data goes after the last volume; where the volumes end does not depend
	// on it, so lay them out once to find the end and again with the data placed.
	volumesEnd := next // advances over the volumes in the first pass
	for i, v := range o.Volumes {
		_, blocks := o.buildVolume(i, v, volumesEnd, &dataAlloc{bs: o.BlockSize})
		volumesEnd += uint64(len(blocks))
	}
	da := &dataAlloc{bs: o.BlockSize, next: volumesEnd}
	for i, v := range o.Volumes {
		vg, blocks := o.buildVolume(i, v, next, da)
		next += uint64(len(blocks))
		g.Volumes = append(g.Volumes, vg)
		l.vols = append(l.vols, blocks)
		entries[len(o.Omap)+i].Paddr = vg.Super
	}
	if next != volumesEnd {
		panic("apfstest: volume sizes changed between layout passes")
	}
	l.data = da.blocks
	next = da.next
	tree, snap = o.omapBlocks(g.Omap+1, entries)
	for _, b := range tree {
		g.OmapNodes = append(g.OmapNodes, b.Addr)
	}
	for _, b := range snap {
		g.OmapSnapNodes = append(g.OmapSnapNodes, b.Addr)
	}
	g.Free = next
	if g.Free >= uint64(o.Blocks) {
		panic("apfstest: container too small for its object map and volumes")
	}
	g.Checkpoints = make([]Checkpoint, o.Checkpoints)
	for t := range o.Checkpoints { // t = 0 is the oldest
		cp := Checkpoint{Xid: o.Xid - uint64(o.Checkpoints-1-t)}
		start := (o.RingStart + t*(o.MapBlocks+1)) % o.DescBlocks
		for m := range o.MapBlocks {
			idx := (start + m) % o.DescBlocks
			cp.MapIndex = append(cp.MapIndex, idx)
			cp.Maps = append(cp.Maps, g.DescBase+uint64(idx))
		}
		cp.Index = (start + o.MapBlocks) % o.DescBlocks
		cp.Super = g.DescBase + uint64(cp.Index)
		cp.DescIndex = start
		cp.DescLen = o.MapBlocks + 1
		cp.Spaceman = g.DataBase + uint64(t)
		cp.DataIndex, cp.DataLen = t, 1 // this checkpoint's spaceman copy is its only data block
		g.Checkpoints[o.Checkpoints-1-t] = cp
	}
	l.g = g
	l.tree, l.snap = tree, snap
	return l
}

// Geometry computes the layout Build uses for o.
func Geometry(o Options) Geo { return o.layout().g }

// Build returns a container image. Block 0 is a copy of the OLDEST
// checkpoint's superblock (stale on purpose: a reader must not trust it); the
// descriptor ring holds, per checkpoint, its mapping block(s) and superblock;
// the data area holds one spaceman copy per checkpoint; the container object
// map follows the data area, then the volumes.
func Build(o Options) []byte {
	o = o.norm()
	l := o.layout()
	g := l.g
	bs := o.BlockSize
	img := make([]byte, o.Blocks*bs)
	blk := func(n uint64) []byte { return img[int(n)*bs : (int(n)+1)*bs] }

	for _, cp := range g.Checkpoints {
		sb := blk(cp.Super)
		writeSuperblock(sb, o, g, cp)
		sealBlock(sb)
		for i, m := range cp.Maps {
			b := blk(m)
			writeMap(b, g, cp, m, i == 0, i == len(cp.Maps)-1)
			sealBlock(b)
		}
		sp := blk(cp.Spaceman)
		writeSpaceman(sp, o, cp.Xid)
		sealBlock(sp)
	}
	oldest := g.Checkpoints[len(g.Checkpoints)-1]
	om := blk(g.Omap)
	var snapRoot uint64
	if len(l.snap) > 0 {
		snapRoot = l.snap[0].Addr
	}
	writeOmap(om, g.Omap, oldest.Xid, l.tree[0].Addr, snapRoot, len(o.OmapSnaps))
	sealBlock(om)
	Place(img, bs, l.tree)
	Place(img, bs, l.snap)
	Place(img, bs, l.data)
	for _, blocks := range l.vols {
		Place(img, bs, blocks)
	}
	copy(blk(0), blk(oldest.Super))
	return img
}

// omapBlocks packs the container object map tree (records entries) and its
// snapshot tree, the first node at block first.
func (o Options) omapBlocks(first uint64, entries []OmapEntry) (tree, snap []Block) {
	next := first
	alloc := func() (uint64, uint64) {
		a := next
		next++
		return a, a
	}
	xid := o.Xid - uint64(o.Checkpoints-1) // the oldest checkpoint's xid, like the omap block
	tree = PackTree(OmapTreeSpec(o.BlockSize, xid, o.OmapMaxKeys), OmapRecs(entries, o.BlockSize), alloc)
	if len(o.OmapSnaps) > 0 {
		spec := TreeSpec{
			BlockSize: o.BlockSize, Fixed: true, KeySize: 8, ValSize: 16,
			BTFlags: 0x10, Storage: StoragePhysical, Xid: xid, MaxKeys: o.OmapMaxKeys,
		}
		snap = PackTree(spec, snapRecs(o.OmapSnaps), alloc)
	}
	return tree, snap
}
