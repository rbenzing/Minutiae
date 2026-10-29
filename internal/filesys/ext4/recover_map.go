package ext4

import (
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

// addSeg maps n blocks from physical block phys at the next logical block, bounded by the blocks still needed.
func (w *mapWalker) addSeg(phys, n int64) error {
	n = min(n, w.nb-w.next)
	if n <= 0 {
		return nil
	}
	adjacent := len(w.segs) > 0 && w.segs[len(w.segs)-1].phys+w.segs[len(w.segs)-1].n == phys
	if !adjacent {
		if w.nruns >= w.f.mapRunsCap {
			return w.stopAt(stopRunsCap, w.next)
		}
		w.nruns++
	}
	w.segs = append(w.segs, mapSeg{w.next, phys, n})
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
	if depth == 0 && le.Uint16(b[2:]) == 0 && int(le.Uint16(b[4:])) <= (len(b)-extentHeaderLen)/extentEntryLen {
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
	if k := w.firstOverlap(); k < len(w.segs) {
		w.stop = fmt.Sprintf(stopTokenTemplate, stopMapOverlap, w.segs[k].lblk)
		w.segs = w.segs[:k]
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

// overlaps reports whether any two of segs share a block. The work is counted.
func (w *mapWalker) overlaps(segs []mapSeg) bool {
	w.work(int64(len(segs)))
	idx := make([]int, len(segs))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int {
		switch {
		case segs[a].phys < segs[b].phys:
			return -1
		case segs[a].phys > segs[b].phys:
			return 1
		}
		return 0
	})
	var end int64
	for k, i := range idx {
		if k > 0 && segs[i].phys < end {
			return true
		}
		end = max(end, segs[i].phys+segs[i].n)
	}
	return false
}

// firstOverlap returns the index of the first data extent that overlaps an earlier one or one of the map's own blocks
// (len(segs) when none does).
func (w *mapWalker) firstOverlap() int {
	m := len(w.segs)
	first := m
	if len(w.tree) > 0 {
		tree := slices.Clone(w.tree)
		slices.Sort(tree)
		w.work(int64(m))
		for k, s := range w.segs {
			i := sort.Search(len(tree), func(i int) bool { return int64(tree[i]) >= s.phys })
			if i < len(tree) && int64(tree[i]) < s.phys+s.n {
				first = k
				break
			}
		}
	}
	if m < 2 || !w.overlaps(w.segs) {
		return first
	}
	// the smallest prefix that overlaps: overlap in a prefix implies overlap in every longer one
	p := 2 + sort.Search(m-2, func(i int) bool { return w.overlaps(w.segs[:i+2]) })
	return min(first, p-1)
}
