package apfstest

// Storage flags of the objects a tree is made of (the high bits of o_type).
const (
	StorageVirtual   = 0
	StoragePhysical  = flagPhysical
	StorageEphemeral = flagEphemeral
)

// B-tree constants: the builder's own copy of the on-disk values.
const (
	typeBTree     = 2
	typeBTreeNode = 3

	btnRoot  = 0x1
	btnLeaf  = 0x2
	btnFixed = 0x4

	// BTFlagHashed is btree_info_t's bt_flags HASHED bit.
	BTFlagHashed = 0x80

	nodeHeaderSize = 56 // btn_data
	infoSize       = 40 // btree_info_t at the end of a root node
	hashLen        = 32 // the hash after a child oid in a HASHED tree

	// InvalidOff is v.off of a ghost entry (BTOFF_INVALID).
	InvalidOff = 0xFFFF
)

// Rec is one record of a tree. A Ghost keeps its key but has no value
// (v.off = 0xFFFF).
type Rec struct {
	Key, Val []byte
	Ghost    bool
}

// Block is one packed, sealed tree node ready to be placed in an image.
type Block struct {
	Addr, Oid uint64
	Data      []byte
}

// Alloc hands out the location of the next node: its block address and its
// object id (equal for physical trees; a virtual tree's oid is mapped to the
// address by an object map the caller maintains).
type Alloc func() (addr, oid uint64)

// TreeSpec describes the shape of a tree to pack.
type TreeSpec struct {
	BlockSize int
	// Fixed selects the FIXED_KV_SIZE table of contents (kvoff_t entries); the
	// records' keys and values must then be KeySize and ValSize long.
	Fixed            bool
	KeySize, ValSize int
	BTFlags          uint32 // btree_info_t.bt_flags
	Storage          uint32 // storage bits of the node objects
	Xid              uint64
	// MaxKeys limits the records per node, forcing a multi-level tree (0: as
	// many as fit).
	MaxKeys int
	// TocSlack adds empty ToC entries to every node's table space.
	TocSlack int
	// Hashed makes index values carry a 32-byte hash after the child oid and
	// sets BTFlagHashed.
	Hashed bool
}

type item struct {
	key, val []byte
	ghost    bool
}

func (s TreeSpec) entrySize() int {
	if s.Fixed {
		return 4
	}
	return 8
}

func (s TreeSpec) indexValLen() int {
	if s.Hashed {
		return 8 + hashLen
	}
	return 8
}

// size is the space the items need in one node, reserving the root's info.
func (s TreeSpec) size(items []item) int {
	n := nodeHeaderSize + infoSize + (len(items)+s.TocSlack)*s.entrySize()
	for _, it := range items {
		n += len(it.key)
		if !it.ghost {
			n += len(it.val)
		}
	}
	return n
}

// group splits items into nodes.
func (s TreeSpec) group(items []item) [][]item {
	if s.MaxKeys == 1 && len(items) > 1 {
		panic("apfstest: MaxKeys 1 can never build a tree with more than one record")
	}
	if len(items) == 0 {
		return [][]item{nil}
	}
	var out [][]item
	start := 0
	for start < len(items) {
		end := start + 1
		if s.size(items[start:end]) > s.BlockSize {
			panic("apfstest: a record does not fit a node")
		}
		for end < len(items) && (s.MaxKeys == 0 || end-start < s.MaxKeys) && s.size(items[start:end+1]) <= s.BlockSize {
			end++
		}
		out = append(out, items[start:end])
		start = end
	}
	return out
}

// PackTree packs recs (which must already be in key order) into a tree and
// returns its nodes, the root first. Every node is one block, sealed with the
// builder's own checksum.
func PackTree(spec TreeSpec, recs []Rec, alloc Alloc) []Block {
	if spec.Hashed {
		spec.BTFlags |= BTFlagHashed
	}
	items := make([]item, len(recs))
	var longestKey, longestVal int
	for i, r := range recs {
		if spec.Fixed && (len(r.Key) != spec.KeySize || (!r.Ghost && len(r.Val) != spec.ValSize)) {
			panic("apfstest: record does not have the fixed key/value size")
		}
		items[i] = item{key: r.Key, val: r.Val, ghost: r.Ghost}
		longestKey = max(longestKey, len(r.Key))
		longestVal = max(longestVal, len(r.Val))
	}
	keyCount := len(recs)

	var lower []Block // every non-root node, in allocation order
	level := uint16(0)
	for {
		groups := spec.group(items)
		if len(groups) == 1 {
			addr, oid := alloc()
			root := Block{Addr: addr, Oid: oid}
			root.Data = spec.buildNode(groups[0], level, true, oid, &infoValues{
				keyCount: uint64(keyCount), nodeCount: uint64(len(lower) + 1),
				longestKey: longestKey, longestVal: longestVal,
			})
			return append([]Block{root}, lower...)
		}
		next := make([]item, 0, len(groups))
		for _, g := range groups {
			addr, oid := alloc()
			b := Block{Addr: addr, Oid: oid, Data: spec.buildNode(g, level, false, oid, nil)}
			lower = append(lower, b)
			val := make([]byte, spec.indexValLen())
			le.PutUint64(val, oid)
			next = append(next, item{key: g[0].key, val: val})
		}
		items = next
		level++
	}
}

// PackChain packs recs into one leaf and stacks levels single-child index
// nodes on top of it, so the root has level levels: a tree as deep as a test
// needs without the records a balanced one would take. The root is first.
func PackChain(spec TreeSpec, recs []Rec, levels int, alloc Alloc) []Block {
	if spec.Hashed {
		spec.BTFlags |= BTFlagHashed
	}
	items := make([]item, len(recs))
	for i, r := range recs {
		items[i] = item{key: r.Key, val: r.Val, ghost: r.Ghost}
	}
	var lower []Block
	for level := 0; level <= levels; level++ {
		addr, oid := alloc()
		blk := Block{Addr: addr, Oid: oid}
		if level == levels {
			blk.Data = spec.buildNode(items, uint16(level), true, oid, &infoValues{
				keyCount: uint64(len(recs)), nodeCount: uint64(levels + 1),
			})
			return append([]Block{blk}, lower...)
		}
		blk.Data = spec.buildNode(items, uint16(level), false, oid, nil)
		lower = append(lower, blk)
		val := make([]byte, spec.indexValLen())
		le.PutUint64(val, oid)
		items = []item{{key: items[0].key, val: val}}
	}
	panic("unreachable")
}

type infoValues struct {
	keyCount, nodeCount    uint64
	longestKey, longestVal int
}

// buildNode lays one node out the way the format describes it: the table of
// contents right after the 56-byte header, the keys growing up from the end of
// the table, the values growing down from the end of the node (or from the
// btree_info_t of a root).
func (s TreeSpec) buildNode(items []item, level uint16, root bool, oid uint64, info *infoValues) []byte {
	bs := s.BlockSize
	b := make([]byte, bs)
	typ := uint32(typeBTreeNode)
	flags := uint16(0)
	if root {
		typ = typeBTree
		flags |= btnRoot
	}
	if level == 0 {
		flags |= btnLeaf
	}
	if s.Fixed {
		flags |= btnFixed
	}
	putObjHeader(b, oid, s.Xid, s.Storage|typ, 0)
	le.PutUint16(b[32:], flags)
	le.PutUint16(b[34:], level)
	le.PutUint32(b[36:], uint32(len(items)))

	entry := s.entrySize()
	tocLen := (len(items) + s.TocSlack) * entry
	keyStart := nodeHeaderSize + tocLen
	valEnd := bs
	if root {
		valEnd -= infoSize
	}
	kpos, vsum := 0, 0
	for i, it := range items {
		e := nodeHeaderSize + i*entry
		copy(b[keyStart+kpos:], it.key)
		koff, voff, vlen := kpos, InvalidOff, 0
		kpos += len(it.key)
		if !it.ghost {
			vsum += len(it.val)
			voff, vlen = vsum, len(it.val)
			copy(b[valEnd-vsum:], it.val)
		}
		if s.Fixed {
			le.PutUint16(b[e:], uint16(koff))
			le.PutUint16(b[e+2:], uint16(voff))
		} else {
			le.PutUint16(b[e:], uint16(koff))
			le.PutUint16(b[e+2:], uint16(len(it.key)))
			le.PutUint16(b[e+4:], uint16(voff))
			le.PutUint16(b[e+6:], uint16(vlen))
		}
	}
	le.PutUint16(b[40:], 0)              // table_space.off
	le.PutUint16(b[42:], uint16(tocLen)) // table_space.len
	le.PutUint16(b[44:], uint16(kpos))   // free_space.off (from the key area start)
	le.PutUint16(b[46:], uint16(valEnd-keyStart-kpos-vsum))
	le.PutUint16(b[48:], InvalidOff) // key_free_list
	le.PutUint16(b[50:], 0)
	le.PutUint16(b[52:], InvalidOff) // val_free_list
	le.PutUint16(b[54:], 0)
	if root {
		i := b[bs-infoSize:]
		le.PutUint32(i[0:], s.BTFlags)
		le.PutUint32(i[4:], uint32(bs))
		le.PutUint32(i[8:], uint32(s.KeySize))
		le.PutUint32(i[12:], uint32(s.ValSize))
		le.PutUint32(i[16:], uint32(info.longestKey))
		le.PutUint32(i[20:], uint32(info.longestVal))
		le.PutUint64(i[24:], info.keyCount)
		le.PutUint64(i[32:], info.nodeCount)
	}
	sealBlock(b)
	return b
}

// Place copies packed blocks into img at their addresses.
func Place(img []byte, blockSize int, blocks []Block) {
	for _, b := range blocks {
		copy(img[int(b.Addr)*blockSize:], b.Data)
	}
}

// SealObject recomputes the checksum of one object (a block patched by a test).
func SealObject(b []byte) { sealBlock(b) }
