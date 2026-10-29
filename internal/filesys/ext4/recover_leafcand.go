package ext4

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Stop reasons that only the leaf chain has (the others are the map walker's, recover_map.go).
const (
	stopLeafGap      = "leaf-gap"      // the next bound leaf does not start where the chain ends, or none is left but the file goes on
	stopLeafConflict = "leaf-conflict" // two bound leaves overlap in logical blocks: which one is current cannot be told
)

// leafUnknownError says the leaf source cannot answer: the free-space pass did not run (filesys.ErrUndecided) or the
// index dropped leaves (filesys.ErrLimit). Recoverable turns it into a warning on the entry's other candidates, or
// returns it when there are none; it never turns it into "no candidate" (3C-R11, 3C-R25).
type leafUnknownError struct {
	kind    error
	warning string
}

func (e *leafUnknownError) Error() string { return e.kind.Error() }
func (e *leafUnknownError) Unwrap() error { return e.kind }

// leafExtent is one extent of a verified leaf.
type leafExtent struct {
	lblk, n, phys int64
	uninit        bool
}

// boundLeaf is a leaf re-read and verified under the inode's seed.
type boundLeaf struct {
	ref  leafRef
	exts []leafExtent
}

// leafCandidates is the ext4-extent-leaf source (3C-R11, amended by 3C-R23 and 3C-R31): the free leaves whose
// checksum binds them to this inode and generation, chained from logical block 0. It needs the index PrepareRecovery
// builds and never builds it itself (3C-R25): without one, or with an index that dropped leaves, the answer is unknown
// and the error is a *leafUnknownError. Superseded leaves verify like current ones, so two leaves that overlap stop
// the chain; nothing is chosen between them.
func (f *FS) leafCandidates(st *staleInode) ([]filesys.Candidate, error) {
	in := st.in
	if !f.sb.metadataCsum() || f.sb.checksumType != csumTypeCRC32C || in.flags&inodeFlagExtents == 0 {
		return nil, nil
	}
	f.leafMu.Lock()
	idx := f.leafIdx
	f.leafMu.Unlock()
	if idx == nil {
		return nil, &leafUnknownError{
			kind:    fmt.Errorf("%w: the stale extent leaves of inode %d were not searched (no free-space pass was run)", filesys.ErrUndecided, in.num),
			warning: "stale extent leaves not searched (no free-space pass in this mode)",
		}
	}
	seed := f.inodeSeed(in)
	key := seedKey(seed, idx.tail)
	if _, keyFull := idx.keyFull[key]; keyFull || idx.full {
		// Leaves were dropped, and any of them may be this inode's newest: a map from the survivors could be an older
		// copy, so nothing is assembled.
		return nil, &leafUnknownError{
			kind:    fmt.Errorf("%w: the leaf index is full, so the stale extent leaves of inode %d may be missing", filesys.ErrLimit, in.num),
			warning: fmt.Sprintf("leaf index full: leaves of inode %d may be missing", in.num),
		}
	}
	refs := idx.byKey[key]
	if len(refs) == 0 {
		return nil, nil
	}
	leaves, err := f.verifyLeaves(refs, seed, idx.tail)
	if err != nil {
		return nil, err
	}
	if len(leaves) == 0 {
		return nil, nil
	}
	return []filesys.Candidate{f.chainLeaves(st, leaves, len(refs))}, nil
}

// verifyLeaves re-reads each indexed leaf and checks its full checksum under seed. A leaf that no longer verifies is
// dropped. The result is sorted by first logical block, then end, then block.
func (f *FS) verifyLeaves(refs []leafRef, seed uint32, tail int) ([]boundLeaf, error) {
	bs := f.sb.blockSize
	le := binary.LittleEndian
	buf := make([]byte, bs)
	var out []boundLeaf
	for _, ref := range refs {
		if err := readFull(f.r, buf, int64(ref.blk)*int64(bs)); err != nil {
			return nil, fmt.Errorf("ext4 stale extent leaf %d: %w", ref.blk, err)
		}
		again, ok := plausibleLeaf(buf, bs, uint64(f.sb.blocksCount))
		if !ok || again.first != ref.first || again.end != ref.end || le.Uint32(buf[tail:]) != rawCRC32C(seed, buf[:tail]) {
			continue
		}
		b := boundLeaf{ref: ref}
		for i := range int(ref.entries) {
			e := buf[extentHeaderLen+i*extentEntryLen:][:extentEntryLen]
			x := leafExtent{lblk: int64(le.Uint32(e)), n: int64(le.Uint16(e[4:])), phys: int64(le.Uint16(e[6:]))<<32 | int64(le.Uint32(e[8:]))}
			if x.n > extentMaxInit {
				x.n -= extentMaxInit
				x.uninit = true
			}
			b.exts = append(b.exts, x)
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b boundLeaf) int {
		return cmp.Or(cmp.Compare(a.ref.first, b.ref.first), cmp.Compare(a.ref.end, b.ref.end), cmp.Compare(a.ref.blk, b.ref.blk))
	})
	return out, nil
}

// chainLeaves chains the verified leaves from logical block 0 and builds the candidate. The chain takes a leaf only
// when it starts exactly where the previous one ended and no other leaf overlaps it; inside a leaf the extents must be
// contiguous, initialized, inside the filesystem and disjoint from every earlier extent. The first thing that is not
// so is the stop, and nothing after it is believed.
func (f *FS) chainLeaves(st *staleInode, leaves []boundLeaf, indexLeaves int) filesys.Candidate {
	in := st.in
	bs := int64(f.sb.blockSize)
	var (
		segs    []mapSeg
		byPhys  []mapSeg // the same extents ordered by physical block, for the overlap test
		chained []uint64
		cursor  int64 // first logical block not covered
		stop    string
		nruns   int
		lastEnd int64
	)
	stopAt := func(reason string, lblk int64) {
		if stop == "" {
			stop = fmt.Sprintf(stopTokenTemplate, reason, lblk)
		}
	}
	for _, l := range leaves {
		lastEnd = max(lastEnd, int64(l.ref.end))
	}
chain:
	for i, l := range leaves {
		first := int64(l.ref.first)
		switch {
		case first > cursor:
			stopAt(stopLeafGap, cursor)
			break chain
		case first < cursor, i+1 < len(leaves) && int64(leaves[i+1].ref.first) < int64(l.ref.end):
			stopAt(stopLeafConflict, first)
			break chain
		}
		chained = append(chained, l.ref.blk)
		for _, x := range l.exts {
			switch {
			case x.lblk != cursor:
				stopAt(stopHole, cursor)
			case x.uninit:
				stopAt(stopUninit, x.lblk)
			case x.phys < 1:
				stopAt(stopMapDamaged, x.lblk)
			}
			if stop != "" {
				break chain
			}
			at := sort.Search(len(byPhys), func(k int) bool { return byPhys[k].phys+byPhys[k].n > x.phys })
			if at < len(byPhys) && byPhys[at].phys < x.phys+x.n {
				stopAt(stopMapOverlap, x.lblk)
				break chain
			}
			adjacent := len(segs) > 0 && segs[len(segs)-1].phys+segs[len(segs)-1].n == x.phys
			if !adjacent {
				if nruns >= f.mapRunsCap {
					stopAt(stopRunsCap, x.lblk)
					break chain
				}
				nruns++
			}
			byPhys = slices.Insert(byPhys, at, mapSeg{x.lblk, x.phys, x.n})
			segs = append(segs, mapSeg{x.lblk, x.phys, x.n})
			cursor += x.n
		}
	}
	wantBlocks := (in.size + bs - 1) / bs
	if stop == "" && in.size > 0 && wantBlocks > cursor {
		stopAt(stopLeafGap, cursor) // the chain ends and no leaf is left, but the inode says the file goes on
	}

	assumptions := []string{"inode-not-reused-since-entry-deleted", "owner-by-crc32c-seed", "leaf-may-be-superseded"}
	var warnings []string
	var size int64
	switch {
	case in.size > 0:
		size = in.size
		assumptions = append(assumptions, "size-from-inode")
		if wantBlocks != cursor {
			assumptions = append(assumptions, "leaf-end-disagrees-with-inode-size")
			warnings = append(warnings, fmt.Sprintf("the inode size (%d bytes) does not end where the chained extent leaves do (%d blocks)", in.size, cursor))
		}
	case stop != "" && lastEnd > cursor:
		size = lastEnd * bs
		assumptions = append(assumptions, "file-end-unknown")
	default:
		size = cursor * bs
		assumptions = append(assumptions, "size-from-extent-leaf-blocks")
	}
	if rootCleared(in.block[:]) {
		assumptions = append(assumptions, "in-inode-map-cleared")
	}

	m := staleMap{stop: stop, notes: warnings}
	for _, s := range segs {
		length := s.n * bs
		if over := m.covered + length - size; over > 0 {
			length -= over // the last block is trimmed to the bytes the file has
		}
		if length <= 0 {
			break
		}
		off := s.phys * bs
		if k := len(m.runs); k > 0 && m.runs[k-1].Offset+m.runs[k-1].Length == off {
			m.runs[k-1].Length += length
		} else {
			m.runs = append(m.runs, filesys.Run{Offset: off, Length: length})
		}
		m.covered += length
	}

	basis := []string{st.rec.idStr, "inode=" + strconv.FormatUint(uint64(in.num), 10), "generation=" + strconv.FormatUint(uint64(in.generation), 10)}
	for i, blk := range chained {
		if i == maxBasisTreeBlocks {
			basis = append(basis, "leaves="+strconv.Itoa(len(chained)))
			break
		}
		basis = append(basis, "leaf="+strconv.FormatUint(blk, 10))
	}
	basis = append(basis, "index-leaves="+strconv.Itoa(indexLeaves))
	return f.candidateFrom(st, MethodExtentLeaf, m, size, basis, assumptions)
}

// rootCleared reports whether the in-inode extent root holds no entries: all zero, or an empty depth-0 header. That is
// what a kernel unlink leaves.
func rootCleared(b []byte) bool {
	le := binary.LittleEndian
	return allZero(b) || (le.Uint16(b) == extentMagic && le.Uint16(b[2:]) == 0 && le.Uint16(b[6:]) == 0 &&
		int(le.Uint16(b[4:])) <= (len(b)-extentHeaderLen)/extentEntryLen)
}
