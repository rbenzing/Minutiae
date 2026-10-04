package hfsplustest

import (
	"encoding/binary"
	"fmt"
	"sort"
	"unicode"
	"unicode/utf16"
)

// rec is one B-tree leaf record: key (with its keyLength prefix, even length)
// and data (even length). parent and name are the catalog sort key, kept so
// the builder orders records itself rather than decoding its own bytes.
type rec struct {
	key, data []byte
	parent    uint32
	name      []uint16
}

func (r rec) bytes() []byte {
	out := append([]byte(nil), r.key...)
	if len(out)%2 != 0 {
		out = append(out, 0)
	}
	out = append(out, r.data...)
	if len(out)%2 != 0 {
		out = append(out, 0)
	}
	return out
}

// treeSpec describes a B-tree to lay out.
type treeSpec struct {
	nodeSize   int
	blockSize  int // the fork is rounded up to whole blocks
	recs       []rec
	maxKey     uint16
	keyCompare byte
	attrs      uint32
	// fixedIndex selects the index record layout of a tree without
	// kBTVariableIndexKeysMask: every index key is maxKey bytes (the key padded
	// with zeros) and the child pointer follows it. 1 writes maxKey in the
	// record's keyLength field (as Apple's writers do), 2 leaves the real key
	// length there. 0 is the variable layout.
	fixedIndex int
	// stale, when it fits, is written into the free space of every leaf after
	// its last record (not counted by numRecords): leftover bytes of a removed
	// record.
	stale []byte
}

// builtTree is a laid-out B-tree: the whole fork's bytes and where things are.
type builtTree struct {
	data       []byte // totalNodes*nodeSize bytes, node 0 first
	nodeSize   int
	totalNodes int
	depth      int
	root       uint32
	firstLeaf  uint32
	lastLeaf   uint32
	levels     [][]uint32 // node numbers per level; levels[0] are the leaves
}

// pack splits recs into nodes' worth, greedily.
func pack(recs [][]byte, nodeSize int) [][]int {
	var groups [][]int
	var cur []int
	used := 14 + 2 // descriptor and the free-space offset
	for i, r := range recs {
		need := len(r) + 2
		if len(r)+14+4 > nodeSize {
			panic(fmt.Sprintf("hfsplustest: a record of %d bytes does not fit a %d-byte node", len(r), nodeSize))
		}
		if used+need > nodeSize && len(cur) > 0 {
			groups = append(groups, cur)
			cur, used = nil, 14+2
		}
		cur = append(cur, i)
		used += need
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// fillNode writes a node: descriptor, the records, their offsets and the
// free-space offset.
func fillNode(nodeSize int, kind int8, height uint8, recs [][]byte) []byte {
	be := binary.BigEndian
	n := make([]byte, nodeSize)
	n[8] = byte(kind)
	n[9] = height
	be.PutUint16(n[10:], uint16(len(recs)))
	off := 14
	for i, r := range recs {
		be.PutUint16(n[nodeSize-2*(i+1):], uint16(off))
		copy(n[off:], r)
		off += len(r)
	}
	be.PutUint16(n[nodeSize-2*(len(recs)+1):], uint16(off))
	return n
}

// freeOffset returns the free-space offset of a node built by fillNode.
func freeOffset(n []byte) int {
	nrec := int(binary.BigEndian.Uint16(n[10:]))
	return int(binary.BigEndian.Uint16(n[len(n)-2*(nrec+1):]))
}

// buildTree lays a B-tree out: header node 0, the leaves, then index levels up
// to a single root. The map record marks the used nodes.
func buildTree(s treeSpec) *builtTree {
	if s.nodeSize == 0 {
		panic("hfsplustest: node size 0")
	}
	be := binary.BigEndian
	var nodes [][]byte // nodes[0] is the header, filled in last
	nodes = append(nodes, nil)
	t := &builtTree{nodeSize: s.nodeSize}

	// Leaves.
	level := make([][]byte, 0, len(s.recs))
	for _, r := range s.recs {
		level = append(level, r.bytes())
	}
	type child struct {
		num uint32
		key []byte // first key of the child's subtree, with keyLength
	}
	var kids []child
	leafRecs := uint32(len(s.recs))
	groups := pack(level, s.nodeSize)
	var leafNums []uint32
	for _, g := range groups {
		rs := make([][]byte, len(g))
		for i, idx := range g {
			rs[i] = level[idx]
		}
		n := fillNode(s.nodeSize, -1, 1, rs)
		if len(s.stale) > 0 {
			free := freeOffset(n)
			tbl := s.nodeSize - 2*(len(rs)+1)
			if free+len(s.stale) <= tbl {
				copy(n[free:], s.stale)
			}
		}
		num := uint32(len(nodes))
		nodes = append(nodes, n)
		leafNums = append(leafNums, num)
		kids = append(kids, child{num: num, key: s.recs[g[0]].key})
	}
	t.levels = append(t.levels, leafNums)

	// Index levels.
	height := uint8(1)
	for len(kids) > 1 {
		height++
		irecs := make([][]byte, len(kids))
		for i, k := range kids {
			d := make([]byte, 4)
			be.PutUint32(d, k.num)
			key := k.key
			if s.fixedIndex != 0 {
				key = fixedKey(key, int(s.maxKey), s.fixedIndex == 1)
			}
			irecs[i] = rec{key: key, data: d}.bytes()
		}
		var next []child
		var nums []uint32
		for _, g := range pack(irecs, s.nodeSize) {
			rs := make([][]byte, len(g))
			for i, idx := range g {
				rs[i] = irecs[idx]
			}
			n := fillNode(s.nodeSize, 0, height, rs)
			num := uint32(len(nodes))
			nodes = append(nodes, n)
			nums = append(nums, num)
			next = append(next, child{num: num, key: kids[g[0]].key})
		}
		t.levels = append(t.levels, nums)
		kids = next
	}
	// Sibling links within each level.
	for _, lv := range t.levels {
		for i, num := range lv {
			if i+1 < len(lv) {
				be.PutUint32(nodes[num][0:], lv[i+1])
			}
			if i > 0 {
				be.PutUint32(nodes[num][4:], lv[i-1])
			}
		}
	}
	t.depth = len(t.levels)
	if len(s.recs) == 0 {
		t.depth, t.levels = 0, nil
	} else {
		t.root = kids[0].num
		t.firstLeaf, t.lastLeaf = leafNums[0], leafNums[len(leafNums)-1]
	}

	// The fork is a whole number of blocks and of nodes.
	used := len(nodes)
	bytes := (used*s.nodeSize + s.blockSize - 1) / s.blockSize * s.blockSize
	t.totalNodes = bytes / s.nodeSize
	t.data = make([]byte, bytes)
	h := bthdr{
		depth: uint16(t.depth), root: t.root, leafRecords: leafRecs,
		firstLeaf: t.firstLeaf, lastLeaf: t.lastLeaf,
		maxKeyLength: s.maxKey, totalNodes: uint32(t.totalNodes), freeNodes: uint32(t.totalNodes - used),
		clumpSize: uint32(bytes), keyCompare: s.keyCompare, attrs: s.attrs,
	}
	copy(t.data, headerNode(s.nodeSize, h, used))
	for i := 1; i < used; i++ {
		copy(t.data[i*s.nodeSize:], nodes[i])
	}
	return t
}

// foldMode says how a case-folding catalog orders the units that are not
// letters.
type foldMode int

const (
	// foldNulLast: the ordering the reader assumes (unverified against a real
	// image): the other ignorable units (U+200C-200F, U+202A-202E, U+206A-206F,
	// U+FEFF) are skipped, and U+0000 maps to 0xFFFF, so it sorts after every
	// other unit and is not ignorable.
	foldNulLast foldMode = iota
	// foldNulIgnored: U+0000 is skipped like the other ignorable units (an
	// earlier guess; Options.NulIgnorable).
	foldNulIgnored
	// foldRaw: plain lower-casing, nothing skipped (U+0000 sorts first;
	// Options.RawFoldOrder).
	foldRaw
)

// fold lower-cases a name for ordering (independent of the reader's fold).
func fold(u []uint16, mode foldMode) []uint16 {
	out := make([]uint16, 0, len(u))
	for _, c := range u {
		if mode != foldRaw {
			switch {
			case c == 0:
				if mode == foldNulIgnored {
					continue
				}
				out = append(out, 0xFFFF)
				continue
			case c == 0xFEFF, c >= 0x200C && c <= 0x200F, c >= 0x202A && c <= 0x202E, c >= 0x206A && c <= 0x206F:
				continue
			}
		}
		out = append(out, uint16(unicode.ToLower(rune(c))))
	}
	return out
}

func cmpUnits(a, b []uint16) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// sortCatalog orders catalog records by (parent, name) in the given fold mode
// (ignored for a binary, case-sensitive catalog).
func sortCatalog(recs []rec, binary bool, mode foldMode) {
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if a.parent != b.parent {
			return a.parent < b.parent
		}
		an, bn := a.name, b.name
		if !binary {
			an, bn = fold(an, mode), fold(bn, mode)
		}
		return cmpUnits(an, bn) < 0
	})
}

// unitsOf encodes a name as UTF-16 code units.
func unitsOf(name string) []uint16 { return utf16.Encode([]rune(name)) }

// fixedKey pads a key (with its keyLength prefix) to a maxKey-byte key. field
// says whether the keyLength field is rewritten to maxKey or kept.
func fixedKey(key []byte, maxKey int, field bool) []byte {
	out := make([]byte, 2+maxKey)
	copy(out, key)
	if field {
		binary.BigEndian.PutUint16(out, uint16(maxKey))
	}
	return out
}
