package f2fs

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The footer index: the blocks of the node segments that the SIT calls free,
// read once in one sequential pass, whose footer looks like a node of this
// volume. A deleted file's inode and node blocks are among them (the kernel
// frees the blocks but does not erase them). Recovery looks an inode number up
// here; the index decides nothing about identity (recover_inode.go does).
//
// What "not found" means: not found among the scanned node-segment blocks. A
// block past a current node segment's cursor, and a segment that is typed data
// now, are not scanned (3D-R25). The classification comes from the 3D probe
// (gate G3): a freed node segment keeps its node type (3, 4 or 5) at vblocks
// 0, and the summary of a closed node segment names the node (gate G2); the
// summary of a current segment is not on disk.
//
// Memory (3D-R22): one flat slice of 24-byte entries, sorted after the pass,
// and one 8-byte record per distinct nid; no map. At the cap that is at most
// 48 MiB and 16 MiB, plus the 2 MiB segment buffer.

const (
	maxCopiesPerNID = 8       // spec 14
	maxNodeIndex    = 1 << 21 // entries (2,097,152); past it every lookup is ErrLimit (3D-R5, 3D-R22)

	// maxSkippedNamed bounds the skipped segments the index names in warnings
	// and in the undecided error.
	maxSkippedNamed = 8
)

// Segment types (CURSEG_*, from the kernel header; the 3D probe confirmed
// 4 for the warm node log) of the node logs: hot, warm and cold node.
const (
	segTypeHotNode  = 3
	segTypeColdNode = 5
)

// staleNode is one copy of a node block in a free block of a node segment.
type staleNode struct {
	nid, addr, ino, ver, ofs uint32 // ver = uint32(footer.cp_ver), always < verLow(checkpoint) once indexed (3D-R19)
	ssa                      bool   // the SSA entry names the nid
	post                     bool   // build only: ver >= the checkpoint's; counted and removed by the compaction
}

// nidMeta holds the per-nid facts of the index: one record per distinct nid
// with any copy, sorted by nid.
type nidMeta struct {
	nid            uint32
	postCheckpoint uint16 // copies dropped because ver >= the checkpoint (saturating)
	evicted        bool   // the per-nid cap evicted copies of this nid
}

// indexStats counts what the index pass did (exported for tests as NodeIndexStats).
type indexStats struct {
	Builds, SegmentsScanned, SegmentsSkipped, Entries, Evicted, SSADropped, PostCheckpoint int
	BlocksRead                                                                             int64
}

type skippedSeg struct {
	seg    uint32
	reason string
}

type nodeIndex struct {
	nodes []staleNode // sorted by (nid, ver desc), at most maxCopiesPerNID per nid
	meta  []nidMeta   // sorted by nid

	capN       int  // the entry cap in force
	capped     bool // the cap was reached: nodes is incomplete and unsorted
	incomplete string
	skipped    []skippedSeg // the first maxSkippedNamed skipped segments
	stats      indexStats
}

// indexState caches the index of one FS, built on first use. A corrupt or
// unsupported result is cached; a genuine I/O error is not, so a later call
// retries.
type indexState struct {
	mu   sync.Mutex
	done bool
	x    *nodeIndex
	err  error
}

func (f *FS) idxStats() indexStats {
	s := &f.idx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.x == nil {
		return indexStats{}
	}
	return s.x.stats
}

// nodeIndex returns the footer index, building it on first use.
func (f *FS) nodeIndex() (*nodeIndex, error) {
	s := &f.idx
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return s.x, s.err
	}
	x, err := f.buildNodeIndex()
	if err != nil && !errors.Is(err, filesys.ErrCorrupt) && !errors.Is(err, filesys.ErrUnsupported) {
		return nil, err
	}
	s.done, s.x, s.err = true, x, err
	return x, err
}

// skip records a node segment the pass could not scan: every lookup is then
// undecided, because a newer copy may lie in the skipped part.
func (x *nodeIndex) skip(f *FS, seg uint32, reason string) {
	x.stats.SegmentsSkipped++
	if len(x.skipped) < maxSkippedNamed {
		x.skipped = append(x.skipped, skippedSeg{seg, reason})
		f.warn("node index: segment %d is skipped (%s); a lookup of stale nodes cannot be decided", seg, reason)
	}
}

// buildNodeIndex runs the one pass. The error is a genuine I/O error, or
// ErrUnsupported for a checkpoint version it cannot compare against.
func (f *FS) buildNodeIndex() (*nodeIndex, error) {
	verLow, ok := f.cp.verLow()
	if !ok {
		return nil, fmt.Errorf("f2fs node index: checkpoint_ver %#x has a non-zero high half, footer versions cannot be compared: %w", f.cp.ver, filesys.ErrUnsupported)
	}
	sb := f.sb
	x := &nodeIndex{capN: maxNodeIndex}
	if f.indexCap > 0 && f.indexCap < x.capN {
		x.capN = f.indexCap
	}
	limit := min(sb.mainEnd(), uint64(sb.blocks)) // 3D-R21: nothing past the clamped end
	natCap := sb.natCapacity()

	// The current segments of the checkpoint (main-area-relative numbers, raw:
	// an out-of-range value matches no segment).
	curNodeOff := func(seg uint32) (uint16, bool) {
		for _, c := range f.cp.curNode {
			if c.seg == seg {
				return c.off, true
			}
		}
		return 0, false
	}
	isCurData := func(seg uint32) bool {
		for _, c := range f.cp.curData {
			if c.seg == seg {
				return true
			}
		}
		return false
	}

	buf := make([]byte, blocksPerSeg*blockSize)
	var ioErr error
	_, err := f.forEachSIT(func(seg uint32, raw []byte) bool {
		if raw == nil {
			x.skip(f, seg, "its SIT block is unreadable, so its type is unknown")
			return true
		}
		vb := binary.LittleEndian.Uint16(raw)
		if typ := vb >> 10; typ < segTypeHotNode || typ > segTypeColdNode || isCurData(seg) {
			return true
		}
		vblocks := int(vb & sitVBlocksMask)
		vmap := raw[sitMapOff : sitMapOff+sitMapSize]
		set := 0
		for _, c := range vmap {
			set += bits.OnesCount8(c)
		}
		if vblocks > blocksPerSeg || vblocks > set {
			x.skip(f, seg, "its SIT entry is inconsistent")
			return true
		}
		if set == blocksPerSeg {
			return true // every block is live: reached through the NAT
		}
		base := uint64(sb.mainAddr) + uint64(seg)<<segShift // < limit: forEachSIT stops there
		n := int(min(uint64(blocksPerSeg), limit-base))
		off, cur := curNodeOff(seg)
		if cur {
			n = min(n, int(off)) // what lies at or past the cursor is an earlier use's
		}
		if n == 0 {
			return true
		}
		if err := readFull(f.data, buf[:n*blockSize], int64(base)*blockSize); err != nil {
			if isIOError(err) {
				ioErr = fmt.Errorf("f2fs node index: read of segment %d failed: %w", seg, err)
				return false
			}
			x.skip(f, seg, "it cannot be read in full")
			return true
		}
		x.stats.SegmentsScanned++
		x.stats.BlocksRead += int64(n)

		// The summary of a current segment is not on disk (probe, G2).
		var ssa []byte
		if !cur {
			b, err := f.ssaBlock(seg)
			switch {
			case err == nil:
				ssa = b
			case errors.Is(err, filesys.ErrCorrupt):
				f.warn("node index: the summary block of segment %d cannot be read (%v); its copies are not cross-checked", seg, err)
			default:
				ioErr = err
				return false
			}
		}
		for i := range n {
			if sitValid(vmap, i) {
				continue
			}
			ft := parseFooter(buf[i*blockSize : (i+1)*blockSize])
			// Free space holds anything: a footer that is no plausible node of this
			// volume is not indexed and not damage.
			if ft.nid < 4 || uint64(ft.nid) >= natCap || ft.ino < 4 || uint64(ft.ino) >= natCap {
				continue
			}
			ssaOK := false
			if ssa != nil {
				if ssaEntryNID(ssa, i) != ft.nid {
					x.stats.SSADropped++
					continue
				}
				ssaOK = true
			}
			if len(x.nodes) >= x.capN {
				x.capped = true
				return false
			}
			ver := uint32(ft.cpVer) // low 32 bits only: the kernel ORs a CRC into the high half (probe Q5)
			x.nodes = append(x.nodes, staleNode{
				nid: ft.nid, addr: uint32(base) + uint32(i), ino: ft.ino, ver: ver, ofs: ft.ofs(),
				ssa: ssaOK, post: ver >= verLow,
			})
		}
		return true
	})
	if ioErr != nil {
		return nil, ioErr
	}
	if err != nil {
		if !errors.Is(err, filesys.ErrCorrupt) {
			return nil, err
		}
		x.incomplete = fmt.Sprintf("the SIT journal cannot be read (%v), so any segment may be overridden", err)
		f.warn("node index: %s; a lookup of stale nodes cannot be decided", x.incomplete)
	}
	x.stats.Builds = 1
	if x.capped {
		x.nodes = nil // unsorted and incomplete: never served
		return x, nil
	}
	x.finish()
	return x, nil
}

// finish sorts the flat slice by (nid, ver desc), keeps at most
// maxCopiesPerNID per nid (the highest versions), drops and counts the
// post-checkpoint copies and builds the per-nid records.
func (x *nodeIndex) finish() {
	slices.SortFunc(x.nodes, func(a, b staleNode) int {
		return cmp.Or(cmp.Compare(a.nid, b.nid), cmp.Compare(b.ver, a.ver), cmp.Compare(a.addr, b.addr))
	})
	groups := 0
	for i := range x.nodes {
		if i == 0 || x.nodes[i].nid != x.nodes[i-1].nid {
			groups++
		}
	}
	x.meta = make([]nidMeta, 0, groups)
	w := 0
	for i := 0; i < len(x.nodes); {
		j := i
		for j < len(x.nodes) && x.nodes[j].nid == x.nodes[i].nid {
			j++
		}
		m := nidMeta{nid: x.nodes[i].nid}
		kept, post := 0, 0
		for k := i; k < j; k++ {
			n := x.nodes[k]
			switch {
			case n.post:
				post++
			case kept < maxCopiesPerNID:
				x.nodes[w] = n
				w++
				kept++
			default:
				m.evicted = true
				x.stats.Evicted++
			}
		}
		x.stats.PostCheckpoint += post
		m.postCheckpoint = uint16(min(post, 1<<16-1))
		x.meta = append(x.meta, m)
		i = j
	}
	x.nodes = x.nodes[:w]
	x.stats.Entries = w
}

// lookup returns a copy of the indexed copies of nid (highest version first)
// and the per-nid facts. It is ErrLimit when the index is capped and
// ErrUndecided when a segment was skipped: in both cases "no copy" would be
// an unknown shown as absent. A nid with no copy gives nil and no error.
func (x *nodeIndex) lookup(nid uint32) ([]staleNode, nidMeta, error) {
	if x.capped {
		return nil, nidMeta{}, fmt.Errorf("f2fs node index: more than %d stale node blocks were found, the index is capped and cannot answer for inode %d: %w", x.capN, nid, filesys.ErrLimit)
	}
	if x.incomplete != "" {
		return nil, nidMeta{}, fmt.Errorf("f2fs node index: %s, so inode %d cannot be looked up: %w", x.incomplete, nid, filesys.ErrUndecided)
	}
	if x.stats.SegmentsSkipped > 0 {
		s := x.skipped[0]
		return nil, nidMeta{}, fmt.Errorf("f2fs node index: %d node segments were skipped (first: segment %d, %s), so inode %d cannot be looked up: %w", x.stats.SegmentsSkipped, s.seg, s.reason, nid, filesys.ErrUndecided)
	}
	i, found := slices.BinarySearchFunc(x.meta, nid, func(m nidMeta, t uint32) int { return cmp.Compare(m.nid, t) })
	if !found {
		return nil, nidMeta{}, nil
	}
	lo, _ := slices.BinarySearchFunc(x.nodes, nid, func(n staleNode, t uint32) int { return cmp.Compare(n.nid, t) })
	hi := lo
	for hi < len(x.nodes) && x.nodes[hi].nid == nid {
		hi++
	}
	return slices.Clone(x.nodes[lo:hi]), x.meta[i], nil
}
