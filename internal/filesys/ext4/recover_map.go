package ext4

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Stop reasons of a stale map (the text before the "@<lblk>" of a stale-chain-stop). A stale map is believed up to its
// first stop and no further; nothing after a stop is guessed, and no hole is filled (3C-R3).
const (
	stopHole         = "hole"               // a logical range the map does not map (a sparse range, or a map that ends early)
	stopUninit       = "uninit"             // an uninitialized (preallocated) extent: its blocks read as zeros, whatever they hold
	stopMapBlockFree = "map-block-not-free" // a map block (index, leaf, indirect) is allocated now: it holds another file's pointers
	stopMapBlockCsum = "map-block-checksum" // an extent block fails its checksum under (inode, generation)
	stopMapDamaged   = "map-damaged"        // a header, count, range or order that no map of this inode can have had
	stopMapOverlap   = "map-overlap"        // a block named twice, or a data extent over a map block or another extent
	stopRunsCap      = "runs-cap"           // more runs than the cap

	stopTokenTemplate = "%s@%d"
)

// staleMap is what a deleted inode's own map still yields.
type staleMap struct {
	runs    []filesys.Run // fs-relative, file order, physically adjacent runs merged, no holes
	covered int64         // bytes the runs cover
	stop    string        // "" or "<reason>@<lblk>"
	tree    []uint64      // map blocks read, in order
	notes   []string
	cleared bool // the map holds nothing at all (a kernel unlink clears it): no claim, and no stop to report
}

// errMapStop unwinds a walk at its stop; it is never returned to a caller.
var errMapStop = errors.New("stale map stopped")

type mapSeg struct{ lblk, phys, n int64 }

type mapWalker struct {
	f       *FS
	in      *inode
	free    *freeSet
	bs      int64
	limit   int64 // bytes to cover
	nb      int64 // blocks to cover
	next    int64 // first logical block not covered yet
	segs    []mapSeg
	nruns   int
	tree    []uint64
	seen    map[uint64]struct{}
	stop    string
	seed    uint32
	csum    bool
	cleared bool
}

func (w *mapWalker) work(n int64) { w.f.mapWork.Add(n) }

func (w *mapWalker) stopAt(reason string, lblk int64) error {
	if w.stop == "" {
		w.stop = fmt.Sprintf(stopTokenTemplate, reason, lblk)
	}
	return errMapStop
}

func (w *mapWalker) freeSet() (*freeSet, error) {
	if w.free == nil {
		s, err := w.f.freeSet()
		if err != nil {
			return nil, err
		}
		w.free = s
	}
	return w.free, nil
}

// extendsLast reports whether a segment at physical block phys continues the last of segs on disk (and so, as every
// segment ends at the next logical block, logically too).
func extendsLast(segs []mapSeg, phys int64) bool {
	last := len(segs) - 1
	return last >= 0 && segs[last].phys+segs[last].n == phys
}

// pushSeg appends n blocks at logical block lblk and physical block phys, or extends the last segment when they
// continue it. Both stale-map sources build their lists through it, so the lists have the same shape.
func pushSeg(segs []mapSeg, lblk, phys, n int64) []mapSeg {
	if extendsLast(segs, phys) {
		segs[len(segs)-1].n += n
		return segs
	}
	return append(segs, mapSeg{lblk, phys, n})
}

// addSeg maps n blocks from physical block phys at the next logical block, bounded by the blocks still needed. Blocks
// that continue the previous segment on disk extend it, so the list is bounded by the run cap, not by the block count.
func (w *mapWalker) addSeg(phys, n int64) error {
	n = min(n, w.nb-w.next)
	if n <= 0 {
		return nil
	}
	if !extendsLast(w.segs, phys) {
		if w.nruns >= w.f.mapRunsCap {
			return w.stopAt(stopRunsCap, w.next)
		}
		w.nruns++
	}
	w.segs = pushSeg(w.segs, w.next, phys, n)
	w.next += n
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// mapStale walks the map of in (an extent tree or direct and indirect pointers) from logical block 0 and returns the
// runs that cover the first limit bytes, up to the first stop (3C-R3, 3C-R4). It has its own walker: the live
// walkers warn and apply the live image-cut rule (3C-R5). The error is an I/O failure only; damaged structure is a
// stop. Every bound is explicit: the walk covers at most limit bytes, reads each map block once, reads only blocks
// that are free now, and holds at most f.mapRunsCap runs.
func (f *FS) mapStale(in *inode, limit int64) (staleMap, error) {
	limit = min(limit, f.size)
	if limit <= 0 {
		return staleMap{cleared: true}, nil
	}
	bs := int64(f.sb.blockSize)
	w := &mapWalker{
		f: f, in: in, bs: bs, limit: limit, nb: (limit + bs - 1) / bs, seen: map[uint64]struct{}{},
		seed: f.inodeSeed(in), csum: f.sb.metadataCsum() && f.sb.checksumType == csumTypeCRC32C,
	}
	var err error
	if in.flags&inodeFlagExtents != 0 {
		err = w.extentRoot()
	} else {
		err = w.blockMap()
	}
	if err != nil && !errors.Is(err, errMapStop) {
		return staleMap{}, err
	}
	return w.finish(), nil
}

// readMapBlock reads one map block after the checks every map block passes: it lies in the image, no earlier pointer
// named it, and it is free now. A refusal is a stop at lblk; the error is an I/O failure only.
func (w *mapWalker) readMapBlock(blk uint64, lblk int64) ([]byte, error) {
	if blk == 0 || blk >= uint64(w.f.sb.blocksCount) {
		return nil, w.stopAt(stopMapDamaged, lblk)
	}
	if _, dup := w.seen[blk]; dup {
		return nil, w.stopAt(stopMapOverlap, lblk)
	}
	free, err := w.freeSet()
	if err != nil {
		return nil, err
	}
	if !free.blockFree(blk, w.bs) {
		return nil, w.stopAt(stopMapBlockFree, lblk)
	}
	w.work(1)
	buf := make([]byte, w.bs) // one block: bounded by the block size
	if err := readFull(w.f.r, buf, int64(blk)*w.bs); err != nil {
		return nil, fmt.Errorf("ext4 stale map block %d of inode %d: %w", blk, w.in.num, err)
	}
	w.seen[blk] = struct{}{}
	w.tree = append(w.tree, blk)
	return buf, nil
}

// extentRoot walks the extent tree whose root is in i_block.
func (w *mapWalker) extentRoot() error {
	b := w.in.block[:]
	if allZero(b) {
		w.cleared = true
		return nil
	}
	le := binary.LittleEndian
	if le.Uint16(b) != extentMagic {
		return w.stopAt(stopMapDamaged, 0)
	}
	depth := int(le.Uint16(b[6:]))
	if depth > maxExtentDepth {
		return w.stopAt(stopMapDamaged, 0)
	}
	if rootCleared(b) {
		w.cleared = true // an empty root: what a kernel unlink leaves
		return nil
	}
	return w.node(b, depth)
}

// node walks one extent node (buf holds the whole container) whose header must declare depth wantDepth.
func (w *mapWalker) node(buf []byte, wantDepth int) error {
	le := binary.LittleEndian
	if len(buf) < extentHeaderLen || le.Uint16(buf) != extentMagic {
		return w.stopAt(stopMapDamaged, w.next)
	}
	entries, maxEntries, depth := int(le.Uint16(buf[2:])), int(le.Uint16(buf[4:])), int(le.Uint16(buf[6:]))
	if depth != wantDepth || maxEntries > (len(buf)-extentHeaderLen)/extentEntryLen || entries > maxEntries {
		return w.stopAt(stopMapDamaged, w.next)
	}
	prev := int64(-1)
	for i := range entries {
		if w.next >= w.nb {
			return nil
		}
		w.work(1)
		e := buf[extentHeaderLen+i*extentEntryLen:][:extentEntryLen]
		if depth > 0 {
			lblk := int64(le.Uint32(e))
			if lblk <= prev {
				return w.stopAt(stopMapDamaged, w.next)
			}
			prev = lblk
			switch {
			case lblk > w.next:
				return w.stopAt(stopHole, w.next)
			case lblk < w.next:
				return w.stopAt(stopMapDamaged, w.next)
			}
			child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
			cbuf, err := w.readMapBlock(child, lblk)
			if err != nil {
				return err
			}
			if le.Uint16(cbuf) != extentMagic {
				return w.stopAt(stopMapDamaged, lblk)
			}
			if w.csum {
				tail := extentHeaderLen + int(le.Uint16(cbuf[4:]))*extentEntryLen
				if tail+4 > len(cbuf) {
					return w.stopAt(stopMapDamaged, lblk)
				}
				if le.Uint32(cbuf[tail:]) != rawCRC32C(w.seed, cbuf[:tail]) {
					return w.stopAt(stopMapBlockCsum, lblk)
				}
			}
			if err := w.node(cbuf, depth-1); err != nil {
				return err
			}
			continue
		}

		lblk := int64(le.Uint32(e))
		n := int64(le.Uint16(e[4:]))
		uninit := n > extentMaxInit
		if uninit {
			n -= extentMaxInit
		}
		phys := int64(le.Uint32(e[8:])) | int64(le.Uint16(e[6:]))<<32
		switch {
		case n == 0, lblk < w.next, lblk+n > 1<<32:
			return w.stopAt(stopMapDamaged, w.next)
		case lblk > w.next:
			return w.stopAt(stopHole, w.next)
		case uninit:
			return w.stopAt(stopUninit, lblk)
		case phys < 1 || phys+n > w.f.sb.blocksCount:
			return w.stopAt(stopMapDamaged, lblk)
		}
		if err := w.addSeg(phys, n); err != nil {
			return err
		}
	}
	return nil
}

// blockMap walks the direct and indirect pointers of an ext2/3 style inode.
func (w *mapWalker) blockMap() error {
	b := w.in.block[:]
	if allZero(b) {
		w.cleared = true
		return nil
	}
	ptr := func(i int) uint32 { return binary.LittleEndian.Uint32(b[i*4:]) }
	for i := range 12 {
		if err := w.ptr(ptr(i), 0); err != nil {
			return err
		}
	}
	for i := range 3 { // indirect, double indirect, triple indirect
		if err := w.ptr(ptr(12+i), i+1); err != nil {
			return err
		}
	}
	return nil
}

// ptr handles one block pointer: level 0 is a data block, level n an indirect block whose pointers are of level n-1.
// A zero pointer is a hole, and ends the map (3C-R3).
func (w *mapWalker) ptr(p uint32, level int) error {
	if w.next >= w.nb {
		return nil
	}
	w.work(1)
	switch {
	case p == 0:
		return w.stopAt(stopHole, w.next)
	case level == 0:
		if int64(p) >= w.f.sb.blocksCount {
			return w.stopAt(stopMapDamaged, w.next)
		}
		return w.addSeg(int64(p), 1)
	}
	buf, err := w.readMapBlock(uint64(p), w.next)
	if err != nil {
		return err
	}
	for i := int64(0); i < w.bs/4 && w.next < w.nb; i++ {
		if err := w.ptr(binary.LittleEndian.Uint32(buf[i*4:]), level-1); err != nil {
			return err
		}
	}
	return nil
}

// finish turns the walk into runs. A map that ends before the bytes it must cover stops there as a hole. Data extents
// that overlap each other or one of the map's own blocks end the map at the first of them.
func (w *mapWalker) finish() staleMap {
	m := staleMap{cleared: w.cleared, tree: w.tree}
	if !w.cleared && w.stop == "" && w.next < w.nb {
		w.stop = fmt.Sprintf(stopTokenTemplate, stopHole, w.next)
	}
	if segs, lblk, found := cutAtOverlap(w.segs, w.tree, w.work); found {
		w.stop = fmt.Sprintf(stopTokenTemplate, stopMapOverlap, lblk)
		w.segs = segs
	}
	var covered int64
	for _, s := range w.segs {
		length := s.n * w.bs // <= the image: cannot overflow
		if over := covered + length - w.limit; over > 0 {
			length -= over // the last block is trimmed to the bytes the file has
		}
		if length <= 0 {
			break
		}
		off := s.phys * w.bs
		if k := len(m.runs); k > 0 && m.runs[k-1].Offset+m.runs[k-1].Length == off {
			m.runs[k-1].Length += length
		} else {
			m.runs = append(m.runs, filesys.Run{Offset: off, Length: length})
		}
		covered += length
	}
	m.covered, m.stop = covered, w.stop
	return m
}

// segsOverlap reports whether any two of segs share a block. work (may be nil) counts the work.
func segsOverlap(segs []mapSeg, work func(int64)) bool {
	if work != nil {
		work(int64(len(segs)))
	}
	idx := make([]int, len(segs))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return cmp.Compare(segs[a].phys, segs[b].phys) })
	var end int64
	for k, i := range idx {
		if k > 0 && segs[i].phys < end {
			return true
		}
		end = max(end, segs[i].phys+segs[i].n)
	}
	return false
}

// cutAtOverlap cuts segs at the first block that is also named by an earlier block or is one of the map's own tree
// blocks. It returns the segments before the cut (the segment holding the block is kept, shortened) and the logical
// block of the cut; found is false when nothing overlaps. Both stale-map sources use it, so both cut at the same
// block. The cost is O(segments log segments + tree blocks) in time and memory, never O(blocks): the exact block is
// found arithmetically inside the first offending segment, not by splitting segments into blocks.
func cutAtOverlap(segs []mapSeg, tree []uint64, work func(int64)) ([]mapSeg, int64, bool) {
	m := len(segs)
	// the first segment tk holding a tree block, and the least offset of such a block in it
	tk, toff := m, int64(0)
	if len(tree) > 0 {
		sorted := slices.Clone(tree)
		slices.Sort(sorted)
		if work != nil {
			work(int64(m))
		}
		for k, s := range segs {
			i := sort.Search(len(sorted), func(i int) bool { return int64(sorted[i]) >= s.phys })
			if i < len(sorted) && int64(sorted[i]) < s.phys+s.n {
				tk, toff = k, int64(sorted[i])-s.phys
				break
			}
		}
	}
	// the first segment pk that shares a block with an earlier one: overlap in a prefix implies overlap in every longer
	// one, so the smallest overlapping prefix ends at it
	pk, poff := m, int64(0)
	if m >= 2 && segsOverlap(segs, work) {
		pk = 1 + sort.Search(m-1, func(i int) bool { return segsOverlap(segs[:i+2], work) })
		s := segs[pk]
		poff = s.n
		for _, e := range segs[:pk] {
			if lo, hi := max(e.phys, s.phys), min(e.phys+e.n, s.phys+s.n); lo < hi {
				poff = min(poff, lo-s.phys)
			}
		}
	}
	k, off := tk, toff
	switch {
	case pk < tk:
		k, off = pk, poff
	case pk == tk:
		off = min(toff, poff)
	}
	if k >= m {
		return segs, 0, false
	}
	cut := segs[:k:k]
	lblk := segs[k].lblk + off
	if off > 0 {
		cut = append(cut, mapSeg{segs[k].lblk, segs[k].phys, off})
	}
	return cut, lblk, true
}
