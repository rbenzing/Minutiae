package ext4

import (
	"github.com/rbenzing/minutiae/internal/filesys"
)

// bgBlockUninit is the group descriptor flag for a group whose block bitmap was
// never written: every block except the group's own metadata is free.
const bgBlockUninit = 0x2

// blockUninitHonoured reports whether BLOCK_UNINIT means anything: the flag
// only exists on filesystems with group descriptor checksums.
func (sb *superblock) blockUninitHonoured() bool {
	return sb.hasRoCompat(roGDTCsum) || sb.metadataCsum()
}

// baseMetaBlocks is the number of blocks at the start of the group that hold
// the superblock backup, the group descriptor blocks and the reserved GDT
// blocks (the kernel's ext4_num_base_meta_blocks).
func (sb *superblock) baseMetaBlocks(group uint64) uint64 {
	var n uint64
	if sb.hasSuper(group) {
		n = 1
	}
	perBlock := uint64(sb.blockSize / sb.descSize)
	if !sb.hasIncompat(incompatMetaBG) || group < uint64(sb.firstMetaBG)*perBlock {
		if n == 0 {
			return 0
		}
		return n + (uint64(sb.groups)+perBlock-1)/perBlock + uint64(sb.reservedGDT)
	}
	// META_BG: the first, second and last group of each metagroup carry one
	// descriptor block.
	first := group / perBlock * perBlock
	if group == first || group == first+1 || group == first+perBlock-1 {
		n++
	}
	return n
}

// blockRange is a half-open range [lo, hi) of absolute block numbers.
type blockRange struct{ lo, hi uint64 }

// Unallocated returns the byte runs of the blocks the block bitmaps mark free.
// A BLOCK_UNINIT group (with group descriptor checksums) has no usable bitmap:
// everything except its metadata is free. Descriptors give absolute bitmap
// locations, so flex_bg layouts need no special case. A group whose descriptor
// or bitmap is unusable is skipped and reported through Info().Warnings; its
// blocks are never reported as free. Blocks past the end of the (possibly
// truncated) image are ignored.
func (f *FS) Unallocated() ([]filesys.Run, error) {
	sb := f.sb
	bpg := int64(sb.blocksPerGroup)
	first := int64(sb.firstDataBlock)
	bs := int64(sb.blockSize)
	total := sb.blocksCount
	uninit := func(g int) bool {
		d := &f.groups[g]
		return !d.bad && !d.csumBad && d.flags&bgBlockUninit != 0 && sb.blockUninitHonoured()
	}

	// Metadata of any group that lies inside a BLOCK_UNINIT group is allocated,
	// not only that group's own (flex_bg puts other groups' bitmaps and inode
	// tables there; the kernel's own initialisation would miss them).
	inUninit := map[int][]blockRange{}
	for gi := range f.groups {
		d := &f.groups[gi]
		if d.bad {
			continue
		}
		for _, r := range []blockRange{
			{d.blockBitmap, d.blockBitmap + 1},
			{d.inodeBitmap, d.inodeBitmap + 1},
			{d.inodeTable, d.inodeTable + uint64(sb.itableBlocks)},
		} {
			if r.lo < uint64(first) || r.lo >= uint64(total) {
				continue
			}
			r.hi = min(r.hi, uint64(total))
			for g := (int64(r.lo) - first) / bpg; g <= (int64(r.hi)-1-first)/bpg && g < int64(len(f.groups)); g++ {
				if uninit(int(g)) {
					inUninit[int(g)] = append(inUninit[int(g)], r)
				}
			}
		}
	}

	var (
		runs                      []filesys.Run
		groupsSeen                int
		skipped, firstSkipped     int
		untrusted, firstUntrusted int
		noBitmap, firstNoBitmap   int
		firstBitmapErr            error
		badCsum, firstBadCsum     int
		bm                        = make([]byte, sb.blockSize)
		checkCsum                 = sb.metadataCsum() && sb.checksumType == csumTypeCRC32C
	)
	for g := range f.groups {
		start := first + int64(g)*bpg
		if start >= total {
			break
		}
		n := int(min(bpg, total-start)) // blocks of this group inside the filesystem
		d := &f.groups[g]
		groupsSeen++
		switch {
		case d.bad:
			if skipped == 0 {
				firstSkipped = g
			}
			skipped++
			continue
		case d.flags&bgBlockUninit != 0 && sb.blockUninitHonoured() && d.csumBad:
			// The flag cannot be trusted when the descriptor checksum is wrong.
			if untrusted == 0 {
				firstUntrusted = g
			}
			untrusted++
			continue
		case uninit(g):
			clear(bm)
			setBlocks(bm, uint64(start), uint64(start)+sb.baseMetaBlocks(uint64(g)), start, n)
			for _, r := range inUninit[g] {
				setBlocks(bm, r.lo, r.hi, start, n)
			}
		default:
			off, ok := filesys.MulOK(int64(d.blockBitmap), bs)
			if !ok {
				noBitmap++
				continue
			}
			nb := (sb.blocksPerGroup + 7) / 8 // <= block size: blocks_per_group <= 8*block size
			if err := readFull(f.r, bm[:nb], off); err != nil {
				if noBitmap == 0 {
					firstNoBitmap, firstBitmapErr = g, err
				}
				noBitmap++
				continue
			}
			if checkCsum {
				got := rawCRC32C(sb.csumSeed, bm[:sb.blocksPerGroup/8])
				want := d.bitmapCsum
				if !d.bitmapCsumHi {
					got &= 0xFFFF
				}
				if got != want {
					if badCsum == 0 {
						firstBadCsum = g
					}
					badCsum++
				}
			}
		}
		runs = appendFree(runs, bm, n, uint64(start), bs)
	}

	if skipped > 0 {
		f.warn("unallocated space: %d of %d block groups have unusable descriptors and were skipped, their blocks are not reported as free (first: group %d)", skipped, groupsSeen, firstSkipped)
	}
	if untrusted > 0 {
		f.warn("unallocated space: %d of %d block groups are flagged BLOCK_UNINIT but their descriptor checksum is wrong; skipped, their blocks are not reported as free (first: group %d)", untrusted, groupsSeen, firstUntrusted)
	}
	if noBitmap > 0 {
		reason := "its location overflows"
		if firstBitmapErr != nil {
			reason = firstBitmapErr.Error()
		}
		f.warn("unallocated space: the block bitmap of %d of %d block groups is unreadable and the groups were skipped, their blocks are not reported as free (first: group %d: %s)", noBitmap, groupsSeen, firstNoBitmap, reason)
	}
	if badCsum > 0 {
		f.warn("unallocated space: block bitmap checksum mismatch in %d of %d block groups (first: group %d); the bitmaps were used as stored", badCsum, groupsSeen, firstBadCsum)
	}
	return filesys.MergeRuns(runs), nil
}

// setBlocks marks the blocks [lo, hi) as allocated in bm, the bitmap of the n
// blocks starting at block start. Blocks outside the group are ignored.
func setBlocks(bm []byte, lo, hi uint64, start int64, n int) {
	for b := max(lo, uint64(start)); b < hi && b-uint64(start) < uint64(n); b++ {
		i := b - uint64(start)
		bm[i/8] |= 1 << (i % 8)
	}
}

// appendFree appends the byte runs of the clear bits among the first n bits of
// bm; bit i stands for block base+i. The caller guarantees base+n is inside
// the filesystem, so the byte offsets cannot overflow.
func appendFree(runs []filesys.Run, bm []byte, n int, base uint64, bs int64) []filesys.Run {
	runStart := -1
	flush := func(end int) {
		if runStart >= 0 {
			runs = append(runs, filesys.Run{Offset: int64(base+uint64(runStart)) * bs, Length: int64(end-runStart) * bs})
			runStart = -1
		}
	}
	for i := 0; i < n; {
		if i%8 == 0 && i+8 <= n {
			switch bm[i/8] {
			case 0xFF:
				flush(i)
				i += 8
				continue
			case 0x00:
				if runStart < 0 {
					runStart = i
				}
				i += 8
				continue
			}
		}
		if bm[i/8]&(1<<(i%8)) != 0 {
			flush(i)
		} else if runStart < 0 {
			runStart = i
		}
		i++
	}
	flush(n)
	return runs
}
