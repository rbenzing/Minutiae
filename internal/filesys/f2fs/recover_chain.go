package f2fs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The stale map walker (3D Task 5): the block map of a deleted file, read
// from a stale inode block and the stale node blocks the footer index finds,
// in file order. It is the stale twin of chain.walk (data.go) with three
// differences: a node is not reached through the NAT but through a nodeSource
// that must prove it belongs to the inode; a hole, a missing node or any
// doubt ends the map (a trusted prefix, never a zero fill); and the result is
// a stop reason, not an error, because a prefix is a finding.

// nodeSource gives the node block that holds nid in the node tree of the stale
// inode owner. ofs is the offset the slot requires (nodeOffset, 3D-R26). stop
// is "" or one of node-missing, node-owner, node-not-node, node-ofs, node-tie,
// node-evicted (the walk stops there); err is an I/O or index error (the
// index is capped or incomplete: ErrLimit, ErrUndecided).
type nodeSource interface {
	nodeFor(nid, owner, ofs uint32) (b []byte, addr uint32, stop string, err error)
}

// nodeOffset is the kernel's node-path numbering (ofs_of_node), from memory of
// fs/f2fs/node.c and confirmed by the 3D probe (Q7): i_nid[0] = 1 and i_nid[1]
// = 2 (direct nodes), i_nid[2] = 3 (indirect; its k-th direct child is 4+k),
// i_nid[3] = 4+1018 = 1022 (its k-th child is 1023+k), i_nid[4] = 1022+1018+1
// = 2041 (double indirect; its i-th indirect child is 2042 + i*1019, whose
// k-th direct child is 2043 + i*1019 + k).
//
// slot is the i_nid index. For slots 0 and 1 only the node itself exists. For
// slots 2 and 3, i is a child index (-1 for the node itself). For slot 4, i is
// the indirect child (-1 for the node itself) and k the direct child of that
// indirect child (-1 for the indirect child itself). An argument outside that
// shape gives 0, which no footer offset of a tree node equals.
func nodeOffset(slot int, i, k int) uint32 {
	const (
		dir1, dir2 = 1, 2
		ind1       = 3
		ind2       = ind1 + 1 + addrsPerBlock // 1022
		dind       = ind2 + 1 + addrsPerBlock // 2041
		indSpan    = 1 + addrsPerBlock        // nodes in an indirect node's subtree: 1019
	)
	switch {
	case slot == 0 && i < 0:
		return dir1
	case slot == 1 && i < 0:
		return dir2
	case (slot == 2 || slot == 3) && k < 0:
		base := uint32(ind1)
		if slot == 3 {
			base = ind2
		}
		if i < 0 {
			return base
		}
		return base + 1 + uint32(i)
	case slot == 4 && i < 0:
		return dind
	case slot == 4 && k < 0:
		return dind + 1 + uint32(i)*indSpan
	case slot == 4:
		return dind + 1 + uint32(i)*indSpan + 1 + uint32(k)
	}
	return 0
}

type staleResult struct {
	runs    []filesys.Run // fs-relative, file order, adjacent runs merged, last block trimmed to the size
	covered int64
	stop    string   // "" when covered == size, else "<reason>@<n>"
	nodes   []uint32 // addresses of the node blocks used, in walk order
}

// staleMap walks the block map of the stale inode in with nodes from src. The
// runs are the trusted prefix; stop names why it ends before the size. The
// error is an I/O or index error from src (returned alone) or a size that
// cannot be (negative). Nothing is allocated from the size, and the work is
// bounded by maxFileRuns runs and node blocks.
func (f *FS) staleMap(in *inode, src nodeSource) (staleResult, error) {
	return f.staleMapLimit(in, src, maxFileRuns)
}

func (f *FS) staleMapLimit(in *inode, src nodeSource, limit int) (staleResult, error) {
	return f.staleMapFan(in, src, limit, addrsPerBlock)
}

// staleMapFan is staleMapLimit with the entries read per node block lowered
// (a test seam: it makes the end of the node tree reachable).
func (f *FS) staleMapFan(in *inode, src nodeSource, limit, fan int) (staleResult, error) {
	if in.size < 0 {
		return staleResult{}, corrupt("f2fs stale inode", -1, "inode %d has a negative size", in.nid)
	}
	if in.size == 0 {
		return staleResult{}, nil
	}
	w := &staleWalker{
		f: f, in: in, src: src, limit: limit, fan: fan,
		end:     min(f.sb.mainEnd(), uint64(f.sb.blocks)),
		visited: map[uint32]struct{}{in.nid: {}},
	}
	w.walk()
	if w.err != nil {
		return staleResult{}, w.err
	}
	if w.stop != "" {
		w.stop = fmt.Sprintf("%s@%d", w.stop, w.stopAt)
	}
	res := staleResult{runs: w.runs, covered: w.covered, stop: w.stop, nodes: w.nodes}
	if cut := repeatedRun(res.runs); cut >= 0 {
		var before int64
		for _, r := range res.runs[:cut] {
			before += r.Length
		}
		res.runs = res.runs[:cut]
		limit := before / blockSize
		keep := 0
		for keep < len(w.starts) && w.starts[keep] < limit {
			keep++
		}
		res.nodes = res.nodes[:keep]
		res.covered = before
		res.stop = fmt.Sprintf("address-repeated@%d", before/blockSize)
	}
	return res, nil
}

type staleWalker struct {
	f       *FS
	in      *inode
	src     nodeSource
	limit   int    // runs, and node blocks, at most
	fan     int    // entries read per node block (addrsPerBlock; lowered by tests)
	end     uint64 // first block address past the main area (clamped to the image)
	runs    []filesys.Run
	covered int64
	lblk    int64
	nodes   []uint32
	starts  []int64 // starts[i] is the logical block where nodes[i] was entered
	visited map[uint32]struct{}
	stop    string // reason, "" while walking
	stopAt  int64  // the number named after the reason (a logical block or a nid)
	err     error
}

func (w *staleWalker) done() bool { return w.stop != "" || w.err != nil || w.covered >= w.in.size }

func (w *staleWalker) halt(reason string, at int64) {
	if w.stop == "" {
		w.stop, w.stopAt = reason, at
	}
}

func (w *staleWalker) walk() {
	raw := w.in.raw
	for i := 0; i < w.in.addrSlots && !w.done(); i++ {
		w.addr(binary.LittleEndian.Uint32(raw[w.in.addrStart+4*i:]))
	}
	for slot, nid := range w.in.nids {
		if w.done() {
			return
		}
		w.node(nid, nodeLevels[slot], slot, -1, nodeOffset(slot, -1, -1))
	}
	if !w.done() {
		// Every slot was read and the size is still not covered: the size is
		// beyond what a node tree can address.
		w.halt("tree-end", w.lblk)
	}
}

// addr maps the next logical block to the data address a.
func (w *staleWalker) addr(a uint32) {
	if a == nullAddr || a == newAddr {
		w.halt("hole", w.lblk)
		return
	}
	if a < w.f.sb.mainAddr || uint64(a) >= w.end {
		w.halt("address-outside-main", w.lblk)
		return
	}
	off := int64(a) * blockSize
	n := min(int64(blockSize), w.in.size-w.covered)
	if k := len(w.runs); k > 0 && w.runs[k-1].Offset+w.runs[k-1].Length == off {
		w.runs[k-1].Length += n
	} else if k >= w.limit {
		w.halt("run-cap", w.lblk)
		return
	} else {
		w.runs = append(w.runs, filesys.Run{Offset: off, Length: n})
	}
	w.covered += n
	w.lblk++
}

// node maps the blocks below node nid of the given level, which sits in
// i_nid[slot] (outer is the indirect index below a double-indirect slot, else
// -1) and must have footer offset ofs.
func (w *staleWalker) node(nid uint32, level, slot, outer int, ofs uint32) {
	if w.done() {
		return
	}
	if nid == 0 {
		w.halt("hole", w.lblk)
		return
	}
	if _, dup := w.visited[nid]; dup {
		w.halt("node-cycle", int64(nid))
		return
	}
	if len(w.nodes) >= w.limit {
		w.halt("run-cap", w.lblk)
		return
	}
	w.visited[nid] = struct{}{}
	b, addr, stop, err := w.src.nodeFor(nid, w.in.nid, ofs)
	if err != nil {
		w.err = err
		return
	}
	if stop != "" {
		w.halt(stop, int64(nid))
		return
	}
	if len(b) != blockSize {
		w.err = corrupt("f2fs stale node", -1, "node %d: the source gave %d bytes, want %d", nid, len(b), blockSize)
		return
	}
	w.starts = append(w.starts, w.lblk)
	w.nodes = append(w.nodes, addr)
	for i := 0; i < w.fan && !w.done(); i++ {
		word := binary.LittleEndian.Uint32(b[4*i:])
		switch {
		case level == 1:
			w.addr(word)
		case level == 2 && slot == 4:
			w.node(word, 1, slot, outer, nodeOffset(slot, outer, i))
		case level == 2:
			w.node(word, 1, slot, -1, nodeOffset(slot, i, -1))
		default: // level 3: the double-indirect node
			w.node(word, 2, slot, i, nodeOffset(slot, i, -1))
		}
	}
}

// repeatedRun returns the index of the first run, in file order, whose bytes
// overlap an earlier run, or -1. "The first k runs are pairwise disjoint"
// holds for a prefix and then never again, so a binary search on k finds it
// with O(log n) sorts of at most n runs.
// The cut is whole-run: when a run overlaps an earlier one only in its tail, the
// whole run is dropped, so the map drops more than strictly necessary and never
// less (a repeated block is never kept).
func repeatedRun(runs []filesys.Run) int {
	disjoint := func(k int) bool {
		s := slices.Clone(runs[:k])
		slices.SortFunc(s, func(a, b filesys.Run) int { return int(sign(a.Offset - b.Offset)) })
		for i := 1; i < len(s); i++ {
			if s[i-1].Offset+s[i-1].Length > s[i].Offset {
				return false
			}
		}
		return true
	}
	if disjoint(len(runs)) {
		return -1
	}
	lo, hi := 1, len(runs) // runs[:lo] are disjoint, runs[:hi] are not
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if disjoint(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}

func sign(x int64) int64 {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

// indexSource serves node blocks from the footer index: for nid, the copies
// with ino == owner, footer ofs == the required ofs and ver < below. The
// highest version wins; equal highest versions with different bytes give
// node-tie. A copy whose ofs is 0 is an inode (node-not-node); copies that
// exist but none at the required ofs give node-ofs; a nid whose copies belong
// to other inodes gives node-owner; none qualifying while the per-nid cap
// evicted copies gives node-evicted (3D-R19); otherwise none gives
// node-missing.
type indexSource struct {
	f     *FS
	idx   *nodeIndex
	below uint32
}

func (s *indexSource) nodeFor(nid, owner, ofs uint32) ([]byte, uint32, string, error) {
	copies, meta, err := s.idx.lookup(nid)
	if err != nil {
		return nil, 0, "", err
	}
	var older, owned, atOfs []staleNode
	sawInode := false
	for _, c := range copies {
		if c.ver >= s.below {
			continue
		}
		older = append(older, c)
		if c.ino != owner {
			continue
		}
		owned = append(owned, c)
		if c.ofs == 0 {
			sawInode = true
		}
		if c.ofs == ofs {
			atOfs = append(atOfs, c)
		}
	}
	switch {
	case len(atOfs) > 0:
	case meta.evicted:
		return nil, 0, "node-evicted", nil
	case len(older) == 0:
		return nil, 0, "node-missing", nil
	case len(owned) == 0:
		return nil, 0, "node-owner", nil
	case sawInode:
		return nil, 0, "node-not-node", nil
	default:
		return nil, 0, "node-ofs", nil
	}
	// copies are sorted by version, highest first.
	top := atOfs[0].ver
	var first []byte
	var firstAddr uint32
	for _, c := range atOfs {
		if c.ver != top {
			break
		}
		b, err := readBlock(s.f.data, c.addr)
		if err != nil {
			if isIOError(err) {
				return nil, 0, "", fmt.Errorf("f2fs stale node: read of block %d failed: %w", c.addr, err)
			}
			// A block the index recorded cannot be read in full: the answer is unknown, never "missing".
			return nil, 0, "", readError("f2fs stale node", int64(c.addr)*blockSize, err)
		}
		if first == nil {
			first, firstAddr = b, c.addr
		} else if !bytes.Equal(first, b) {
			return nil, 0, "node-tie", nil
		}
	}
	return first, firstAddr, "", nil
}
