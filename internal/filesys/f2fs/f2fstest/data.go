package f2fstest

import (
	"encoding/binary"
	"slices"
)

// File data encoding for the builder: data blocks, direct, indirect and
// double-indirect node blocks, and an allocator that lays a whole file out.
// Like the rest of the package it shares no code with the reader.

// Address values of a data slot (include/linux/f2fs_fs.h).
const (
	NullAddr = 0          // NULL_ADDR: a hole
	NewAddr  = 0xFFFFFFFF // NEW_ADDR: allocated but never written, reads as zeros
)

// AddrsPerBlock is the number of 32-bit slots in a direct node (data
// addresses) or an indirect node (node ids): (4096 - 24) / 4.
const AddrsPerBlock = 1018

// DataBlock is a file data block to place in the image at an absolute block
// address (it is written only when the address lies inside the image).
type DataBlock struct {
	Addr uint32
	Data []byte // at most 4096 bytes, zero-padded
}

func writeData(img []byte, o Options) {
	for _, d := range o.Data {
		if len(d.Data) > BlockSize {
			panic("f2fstest: data block longer than 4096 bytes")
		}
		if off := int(d.Addr) * BlockSize; off+BlockSize <= len(img) {
			copy(img[off:off+BlockSize], d.Data)
		}
	}
}

// pointerNode encodes a direct or an indirect node: 1018 words, then the
// footer. nid and ino go into the footer.
func pointerNode(nid, ino uint32, words []uint32) []byte {
	if len(words) > AddrsPerBlock {
		panic("f2fstest: more than 1018 entries in a node block")
	}
	b := make([]byte, BlockSize)
	for i, w := range words {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	binary.LittleEndian.PutUint32(b[4072:], nid)
	binary.LittleEndian.PutUint32(b[4076:], ino)
	return b
}

// DirectNodeBlock encodes a direct node holding data addresses.
func DirectNodeBlock(nid, ino uint32, addrs []uint32) []byte { return pointerNode(nid, ino, addrs) }

// IndirectNodeBlock encodes an indirect (or double-indirect) node holding node
// ids.
func IndirectNodeBlock(nid, ino uint32, nids []uint32) []byte { return pointerNode(nid, ino, nids) }

// Alloc hands out node ids and main-area block addresses for File.
type Alloc struct {
	NextNID  uint32
	NextAddr uint32
	// Stride is the distance between consecutive data block addresses
	// (default 1: consecutive blocks are physically adjacent).
	Stride uint32
	limit  uint32
}

// NewAlloc starts allocating node ids at firstNID and addresses at the first
// block of the main area of the image o describes.
func NewAlloc(o Options, firstNID uint32) *Alloc {
	l := Geometry(o)
	return &Alloc{NextNID: firstNID, NextAddr: l.Main, limit: l.BlockCount}
}

// NID returns a fresh node id.
func (a *Alloc) NID() uint32 { n := a.NextNID; a.NextNID++; return n }

// Addr returns a fresh block address (advancing by Stride).
func (a *Alloc) Addr() uint32 {
	n := a.NextAddr
	if n >= a.limit {
		panic("f2fstest: main area exhausted; raise Options.Segments")
	}
	a.NextAddr += max(a.Stride, 1)
	return n
}

// FileData is the mapping of a file's logical blocks. A block in neither
// field is a NULL_ADDR hole.
type FileData struct {
	Blocks map[int64][]byte // logical block -> content (at most 4096 bytes, zero-padded)
	New    []int64          // logical blocks mapped to NEW_ADDR
}

// span is the number of logical blocks a node of the given level covers: 1018
// for a direct node (level 1), 1018^2 for an indirect, 1018^3 for a
// double-indirect one.
func span(level int) int64 {
	n := int64(1)
	for range level {
		n *= AddrsPerBlock
	}
	return n
}

// File lays out the file in: its data blocks and the node blocks that map
// them (direct nodes in i_nid[0..1], indirect in i_nid[2..3], double indirect
// in i_nid[4], as many as needed). It returns the inode node (first) with
// Addrs and NIDs filled in, every node block and the data blocks; append them
// to Options.Nodes and Options.Data. in.NID must be set; Size is the caller's.
// Only an inode without inline xattr space is supported.
func (a *Alloc) File(o Options, in Inode, d FileData) ([]Node, []DataBlock) {
	if in.Inline&InlineXattr != 0 {
		panic("f2fstest: Alloc.File does not support inline xattr space")
	}
	extra := 0
	if in.Extra {
		extra = 36
		if in.ExtraIsize != 0 {
			extra = int(in.ExtraIsize)
		}
	}
	slots := int64(923 - extra/4)

	var present []int64 // sorted logical blocks that have a non-NULL address
	for k := range d.Blocks {
		present = append(present, k)
	}
	present = append(present, d.New...)
	slices.Sort(present)
	present = slices.Compact(present)
	has := func(lo, hi int64) bool { // any present block in [lo, hi)?
		i, _ := slices.BinarySearch(present, lo)
		return i < len(present) && present[i] < hi
	}
	isNew := map[int64]bool{}
	for _, k := range d.New {
		isNew[k] = true
	}

	var nodes []Node
	var data []DataBlock
	addrOf := func(idx int64) uint32 {
		if c, ok := d.Blocks[idx]; ok {
			addr := a.Addr()
			data = append(data, DataBlock{Addr: addr, Data: c})
			return addr
		}
		if isNew[idx] {
			return NewAddr
		}
		return NullAddr
	}
	// build lays out the node of the given level that starts at logical block
	// base and returns its nid (0 when no block in its span is present).
	var build func(level int, base int64) uint32
	build = func(level int, base int64) uint32 {
		if !has(base, base+span(level)) {
			return 0
		}
		words := make([]uint32, AddrsPerBlock)
		for j := range words {
			lo := base + int64(j)*span(level-1)
			if level == 1 {
				words[j] = addrOf(lo)
			} else {
				words[j] = build(level-1, lo)
			}
		}
		nid := a.NID()
		blk := pointerNode(nid, in.NID, words)
		nodes = append(nodes, Node{NID: nid, Addr: a.Addr(), Block: blk})
		return nid
	}

	addrs := make([]uint32, 0, slots)
	for i := range slots {
		addrs = append(addrs, addrOf(i))
	}
	base := slots
	for i, level := range []int{1, 1, 2, 2, 3} {
		in.NIDs[i] = build(level, base)
		base += span(level)
	}
	in.Addrs = addrs
	inode := Node{NID: in.NID, Addr: a.Addr(), Block: InodeBlock(o, in)}
	return append([]Node{inode}, nodes...), data
}
