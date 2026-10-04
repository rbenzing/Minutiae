package apfs

import (
	"cmp"
	"fmt"
	"math/bits"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// maxUnallocRuns bounds the runs Unallocated reports; more are cut off with a
// warning.
const maxUnallocRuns = 1 << 24

// span is a half-open range of blocks.
type span struct{ start, end uint64 }

// chunkInfo is one chunk_info_t that passed the structural checks.
type chunkInfo struct {
	index        uint64 // chunk number (chunk k covers blocks [k*blocks_per_chunk, ...))
	blocks, free uint64
	bitmap       uint64 // block of the bitmap, 0 for none
	cib          uint64 // the CIB it came from (for messages)
	start, end   uint64 // the blocks it covers
}

// Unallocated reports the free space of the container's main device as byte
// runs, from the space manager of the selected checkpoint: per chunk, the
// chunk-info record and its bitmap block (bit i of byte i/8, mask 1<<(i%8), set =
// allocated; verified against the real fixtures). A bitmap-less chunk (address
// 0) is free only when its record says every block is. Free space is never
// guessed: a space manager, CIB, CAB or chunk whose checksum, counts, address
// or bitmap disagree is skipped with a warning (its blocks are not reported),
// and block 0, the checkpoint areas, the internal pool, every bitmap, CIB, CAB
// and space-manager block and the blocked-out range are never reported whatever
// a bitmap says. The result is bounded by the container and by the image, and
// holds at most 1<<24 runs (a warning says when the rest is cut off). A read
// failure that is not corruption is returned as an error.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	sm, problem, err := f.loadSpaceman()
	if err != nil {
		return nil, fmt.Errorf("apfs: unallocated: space manager: %w", err)
	}
	if problem != "" {
		f.warn("free space: %s; no free space is reported", problem)
		return nil, nil
	}
	bs := uint64(f.bs)
	limit := min(f.blocks, sm.blocks, uint64(f.size)/bs) // first block never reported: the container, the device and the image end
	maxRuns := f.unallocCap
	if maxRuns <= 0 {
		maxRuns = maxUnallocRuns
	}

	// Never free, whatever a bitmap says.
	var excl []span
	addExcl := func(start, count uint64) {
		if end, ok := addU64(start, count); ok && count > 0 {
			excl = append(excl, span{start, min(end, f.blocks)})
		}
	}
	addExcl(0, 1)
	addExcl(f.nx.descBase, uint64(f.nx.descBlocks))
	addExcl(f.nx.dataBase, uint64(f.nx.dataBlocks))
	addExcl(sm.ipBase, sm.ipBlocks)
	addExcl(sm.ipBmBase, sm.ipBmBlocks)
	addExcl(f.nx.blockedStart, f.nx.blockedCount)
	addExcl(sm.obj[0], sm.obj[1])

	// A device smaller than the container is noted; the bitmaps stay authoritative.
	if sm.blocks < f.blocks {
		f.warn("free space: the space manager's device has %d blocks but the container has %d; the blocks past the device are not reported", sm.blocks, f.blocks)
	}
	infos, err := f.readChunkInfos(sm, limit, addExcl)
	if err != nil {
		return nil, err
	}

	// A bitmap block shared by two chunks cannot be told apart: both are skipped.
	users := map[uint64]int{}
	for _, ci := range infos {
		if ci.bitmap != 0 {
			users[ci.bitmap]++
		}
	}

	var (
		free   []span
		capped bool
	)
	emit := func(s span) {
		if capped || s.start >= s.end {
			return
		}
		if n := len(free); n > 0 && free[n-1].end == s.start {
			free[n-1].end = s.end
			return
		}
		if len(free) >= maxRuns {
			f.warn("free space: more than %d free runs, the rest is not reported", maxRuns)
			capped = true
			return
		}
		free = append(free, s)
	}
	var (
		sum     uint64 // of the ci_free_count of the chunks whose bitmaps were used
		skipped bool   // a chunk was left out (its own warning says why)
	)
	for _, ci := range infos {
		if capped {
			skipped = true
			break
		}
		switch {
		case ci.bitmap == 0:
			if ci.free != ci.blocks {
				f.warn("free space: chunk %d (CIB at block %d) has no bitmap block but records %d of %d blocks allocated; the chunk is skipped", ci.index, ci.cib, ci.blocks-ci.free, ci.blocks)
				skipped = true
				continue
			}
			emit(span{ci.start, min(ci.end, limit)})
			sum += ci.free
		case users[ci.bitmap] > 1:
			f.warn("free space: chunk %d (CIB at block %d) shares its bitmap block %d with another chunk; the chunk is skipped", ci.index, ci.cib, ci.bitmap)
			skipped = true
		default:
			used, err := f.chunkFree(ci, limit, emit)
			if err != nil {
				return nil, err
			}
			if used {
				sum += ci.free
			} else {
				skipped = true
			}
		}
	}
	// The space manager's own totals are cross-checked with a warning only (the
	// bitmaps stay authoritative, so the runs never change), and only when every
	// chunk was read and used: a skipped chunk has its own warning.
	if !skipped && uint64(len(infos)) == sm.chunks && sum != sm.free {
		f.warn("free space: the space manager records %d free blocks but its chunk records add up to %d; the bitmaps are used", sm.free, sum)
	}

	runs := subtract(free, excl, f.bs)
	if len(runs) > maxRuns {
		runs = runs[:maxRuns]
		if !capped {
			f.warn("free space: more than %d free runs, the rest is not reported", maxRuns)
		}
	}
	return filesys.MergeRuns(runs), nil
}

// readChunkInfos reads the CIBs (through the CAB layer when there is one) that
// cover the chunks below limit, checks them and every chunk-info record, and
// returns the chunks that passed, in chunk order. Every CIB, CAB and bitmap
// address is handed to addExcl. A bad CAB, CIB or record is a warning that skips
// what it governs.
func (f *FS) readChunkInfos(sm *spaceman, limit uint64, addExcl func(start, count uint64)) ([]chunkInfo, error) {
	bs := uint64(f.bs)
	needChunks := min(sm.chunks, ceilDiv(limit, bs*8)) // chunks that start below the image end
	needCibs := ceilDiv(needChunks, sm.cpc)

	// The CIB addresses: straight from the spaceman, or through the CABs.
	cibs := make([]uint64, needCibs)
	okCib := make([]bool, needCibs)
	if sm.cabs == 0 {
		for k := range needCibs {
			cibs[k], okCib[k] = le.Uint64(sm.addrs[8*k:]), true
		}
	} else {
		for j := range ceilDiv(needCibs, sm.cpcab) {
			cabAddr := le.Uint64(sm.addrs[8*j:])
			addExcl(cabAddr, 1)
			buf, why, err := f.readFreshObject(cabAddr, f.bs, typeSpacemanCAB)
			if err != nil {
				return nil, fmt.Errorf("apfs: unallocated: CIB-address block %d: %w", j, err)
			}
			want := min(sm.cpcab, sm.cibs-j*sm.cpcab)
			if why == "" {
				switch {
				case uint64(le.Uint32(buf[cibIndexOff:])) != j:
					why = fmt.Sprintf("has index %d", le.Uint32(buf[cibIndexOff:]))
				case uint64(le.Uint32(buf[cibCountOff:])) != want:
					why = fmt.Sprintf("lists %d CIBs, want %d", le.Uint32(buf[cibCountOff:]), want)
				}
			}
			if why != "" {
				f.warn("free space: the CIB-address block %d at block %d %s; its CIBs and their chunks are skipped", j, cabAddr, why)
				continue
			}
			for i := range want {
				if k := j*sm.cpcab + i; k < needCibs {
					cibs[k], okCib[k] = le.Uint64(buf[cibRecsOff+8*i:]), true
				}
			}
		}
	}

	var out []chunkInfo
	for k := range needCibs {
		if !okCib[k] {
			continue
		}
		addr := cibs[k]
		addExcl(addr, 1)
		buf, why, err := f.readFreshObject(addr, f.bs, typeSpacemanCIB)
		if err != nil {
			return nil, fmt.Errorf("apfs: unallocated: chunk-info block %d: %w", k, err)
		}
		want := min(sm.cpc, sm.chunks-k*sm.cpc)
		if why == "" {
			count := uint64(le.Uint32(buf[cibCountOff:]))
			switch {
			case uint64(le.Uint32(buf[cibIndexOff:])) != k:
				why = fmt.Sprintf("has index %d", le.Uint32(buf[cibIndexOff:]))
			case count > sm.cpc || cibRecsOff+count*chunkInfoSize > bs:
				why = fmt.Sprintf("claims %d chunk-info records (at most %d, and they must fit the block)", count, sm.cpc)
			case count != want:
				why = fmt.Sprintf("holds %d chunk-info records, want %d", count, want)
			}
		}
		if why != "" {
			f.warn("free space: the chunk-info block %d at block %d %s; its chunks are skipped", k, addr, why)
			continue
		}
		for j := range want {
			idx := k*sm.cpc + j
			if idx >= needChunks {
				break
			}
			ci, why := f.checkChunk(sm, idx, buf[cibRecsOff+j*chunkInfoSize:])
			if ci.bitmap != 0 {
				addExcl(ci.bitmap, 1) // a bitmap block is metadata, whether or not the chunk is usable
			}
			if why != "" {
				f.warn("free space: chunk %d (CIB at block %d) %s; the chunk is skipped", idx, addr, why)
				continue
			}
			ci.cib = addr
			out = append(out, ci)
		}
	}
	return out, nil
}

// checkChunk validates the chunk-info record e of chunk idx. The returned
// chunkInfo always carries the bitmap address; a non-empty why rejects the chunk.
func (f *FS) checkChunk(sm *spaceman, idx uint64, e []byte) (ci chunkInfo, why string) {
	bpc := uint64(f.bs) * 8
	addr := le.Uint64(e[ciAddrOff:])
	rawBlocks, rawFree := le.Uint32(e[ciBlocksOff:]), le.Uint32(e[ciFreeOff:])
	ci = chunkInfo{index: idx, bitmap: le.Uint64(e[ciBitmapOff:])}
	start := idx * bpc // idx < chunks <= blocks/bpc + 1 and blocks < 2^63/bs: no overflow
	switch {
	case addr != start:
		return ci, fmt.Sprintf("records address %d, want %d", addr, start)
	case rawBlocks&^ciCountMask != 0 || rawFree&^ciCountMask != 0:
		return ci, fmt.Sprintf("has reserved bits set in its counts (%#x blocks, %#x free)", rawBlocks, rawFree)
	case uint64(rawBlocks) != min(bpc, sm.blocks-start):
		return ci, fmt.Sprintf("covers %d blocks, want %d", rawBlocks, min(bpc, sm.blocks-start))
	case rawFree > rawBlocks:
		return ci, fmt.Sprintf("records %d free of %d blocks", rawFree, rawBlocks)
	}
	ci.blocks, ci.free, ci.start, ci.end = uint64(rawBlocks), uint64(rawFree), start, start+uint64(rawBlocks)
	if ci.bitmap != 0 {
		// The bitmap block must lie in the container and outside the areas the
		// checkpoint owns (the internal pool, where real bitmaps live, is fine).
		inArea := func(base, n uint64) bool { return ci.bitmap >= base && ci.bitmap-base < n }
		switch {
		case ci.bitmap >= f.blocks:
			return ci, fmt.Sprintf("has its bitmap at block %d, outside the container of %d blocks", ci.bitmap, f.blocks)
		case inArea(f.nx.descBase, uint64(f.nx.descBlocks)) || inArea(f.nx.dataBase, uint64(f.nx.dataBlocks)):
			return ci, fmt.Sprintf("has its bitmap at block %d, inside a checkpoint area", ci.bitmap)
		}
	}
	return ci, ""
}

// chunkFree reads the bitmap of ci and emits its free blocks below limit. A
// bitmap whose clear bits do not match the record is skipped with a warning
// (used is false).
func (f *FS) chunkFree(ci chunkInfo, limit uint64, emit func(span)) (used bool, err error) {
	bm, err := f.readFresh(ci.bitmap, f.bs)
	switch {
	case err == nil:
	case isShort(err):
		f.warn("free space: the bitmap block %d of chunk %d is unreadable (truncated image); the chunk is skipped", ci.bitmap, ci.index)
		return false, nil
	case isNotFoundOrCorrupt(err):
		f.warn("free space: the bitmap block %d of chunk %d cannot be used (%v); the chunk is skipped", ci.bitmap, ci.index, err)
		return false, nil
	default:
		return false, fmt.Errorf("apfs: unallocated: bitmap of chunk %d: %w", ci.index, err)
	}
	if zeros := countZeroBits(bm, ci.blocks); zeros != ci.free {
		f.warn("free space: chunk %d has %d clear bits in its bitmap but records %d free blocks; the chunk is skipped", ci.index, zeros, ci.free)
		return false, nil
	}
	scanZeroRuns(bm, ci.blocks, func(from, to uint64) {
		emit(span{ci.start + from, min(ci.start+to, limit)})
	})
	return true, nil
}

// countZeroBits counts the clear bits among the first n bits of bm (bit i is
// mask 1<<(i%8) of byte i/8); n <= 8*len(bm).
func countZeroBits(bm []byte, n uint64) uint64 {
	full, rest := int(n/8), n%8
	var ones uint64
	for _, b := range bm[:full] {
		ones += uint64(bits.OnesCount8(b))
	}
	if rest != 0 {
		ones += uint64(bits.OnesCount8(bm[full] & (1<<rest - 1)))
	}
	return n - ones
}

// scanZeroRuns calls emit(from, to) for each maximal run of clear bits among the
// first n bits of bm, in order.
func scanZeroRuns(bm []byte, n uint64, emit func(from, to uint64)) {
	var runStart uint64
	inRun := false
	for i := uint64(0); i < n; {
		if i%8 == 0 && n-i >= 8 {
			switch bm[i/8] {
			case 0xFF:
				if inRun {
					emit(runStart, i)
					inRun = false
				}
				i += 8
				continue
			case 0x00:
				if !inRun {
					runStart, inRun = i, true
				}
				i += 8
				continue
			}
		}
		if bm[i/8]>>(i%8)&1 == 0 {
			if !inRun {
				runStart, inRun = i, true
			}
		} else if inRun {
			emit(runStart, i)
			inRun = false
		}
		i++
	}
	if inRun {
		emit(runStart, n)
	}
}

// subtract returns free (sorted, merged block spans) minus excl (any order) as
// byte runs.
func subtract(free, excl []span, bs int) []filesys.Run {
	slices.SortFunc(excl, func(a, b span) int { return cmp.Compare(a.start, b.start) })
	var merged []span
	for _, e := range excl {
		if e.start >= e.end {
			continue
		}
		if n := len(merged); n > 0 && e.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, e.end)
			continue
		}
		merged = append(merged, e)
	}
	var out []filesys.Run
	i := 0
	for _, s := range free {
		cur := s.start
		for i < len(merged) && merged[i].end <= cur {
			i++
		}
		for j := i; cur < s.end; j++ {
			if j >= len(merged) || merged[j].start >= s.end {
				out = append(out, byteRun(cur, s.end, bs))
				break
			}
			if merged[j].start > cur {
				out = append(out, byteRun(cur, merged[j].start, bs))
			}
			cur = max(cur, merged[j].end)
		}
	}
	return out
}

func byteRun(start, end uint64, bs int) filesys.Run {
	return filesys.Run{Offset: int64(start) * int64(bs), Length: int64(end-start) * int64(bs)}
}
