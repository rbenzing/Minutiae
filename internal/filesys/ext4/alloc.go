package ext4

import (
	"slices"
	"sort"
	"strconv"
	"strings"

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
	n := sb.baseMetaBlocksNoBoot(group)
	if group == 0 && sb.blockSize == 1024 && sb.firstDataBlock == 0 {
		n++ // 1 KiB blocks without first_data_block: group 0 starts at the boot block
	}
	return n
}

func (sb *superblock) baseMetaBlocksNoBoot(group uint64) uint64 {
	var n uint64
	if sb.hasSuper(group) {
		n = 1
	}
	perBlock := uint64(sb.blockSize / sb.descSize)
	if !sb.hasIncompat(incompatMetaBG) || group < uint64(sb.firstMetaBG)*perBlock {
		if n == 0 {
			return 0
		}
		// With META_BG the groups below first_meta_bg still hold the old
		// descriptor table, but only first_meta_bg blocks of it (the kernel's
		// ext4_bg_num_gdb_nometa).
		gdt := (uint64(sb.groups) + perBlock - 1) / perBlock
		if sb.hasIncompat(incompatMetaBG) {
			gdt = uint64(sb.firstMetaBG)
		}
		return n + gdt + uint64(sb.reservedGDT)
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

// maxNamedGroups bounds how many group numbers one warning lists.
const maxNamedGroups = 8

// foreignMetadata returns the sorted, disjoint, coalesced block ranges that the
// descriptors name as bitmaps or inode tables inside [first, total). Each
// location counts on its own: a descriptor flagged bad because one location is
// outside the filesystem still protects its other locations. Only descriptors
// that could not be read at all are ignored.
func foreignMetadata(groups []groupDesc, first, total, itable uint64) []blockRange {
	var rs []blockRange
	for i := range groups {
		d := &groups[i]
		if d.unreadable {
			continue
		}
		for _, r := range [...]blockRange{
			{d.blockBitmap, d.blockBitmap + 1},
			{d.inodeBitmap, d.inodeBitmap + 1},
			{d.inodeTable, d.inodeTable + itable},
		} {
			if r.lo < first || r.lo >= total {
				continue
			}
			r.hi = min(r.hi, total)
			if r.hi > r.lo {
				rs = append(rs, r)
			}
		}
	}
	slices.SortFunc(rs, func(a, b blockRange) int {
		switch {
		case a.lo < b.lo:
			return -1
		case a.lo > b.lo:
			return 1
		}
		return 0
	})
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && r.lo <= out[n-1].hi {
			out[n-1].hi = max(out[n-1].hi, r.hi)
			continue
		}
		out = append(out, r)
	}
	return out
}

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
	// tables there; the kernel's own initialisation would miss them). The
	// ranges are sorted and coalesced once, so marking costs at most the size
	// of a group however many descriptors point into it.
	meta := foreignMetadata(f.groups, uint64(first), uint64(total), uint64(sb.itableBlocks))

	var (
		runs                      []filesys.Run
		groupsSeen                int
		skipped, firstSkipped     int
		untrusted, firstUntrusted int
		noBitmap, firstNoBitmap   int
		firstBitmapErr            error
		badCsum                   int
		badCsumGroups             []int
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
			for i := sort.Search(len(meta), func(i int) bool { return meta[i].hi > uint64(start) }); i < len(meta) && meta[i].lo < uint64(start)+uint64(n); i++ {
				setBlocks(bm, meta[i].lo, meta[i].hi, start, n)
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
					// The bitmap cannot be trusted: skip the group rather than
					// report blocks as free on the strength of damaged data.
					if len(badCsumGroups) < maxNamedGroups {
						badCsumGroups = append(badCsumGroups, g)
					}
					badCsum++
					continue
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
		names := make([]string, len(badCsumGroups))
		for i, g := range badCsumGroups {
			names[i] = strconv.Itoa(g)
		}
		more := ""
		if badCsum > len(badCsumGroups) {
			more = ", ..."
		}
		f.warn("unallocated space: block bitmap checksum mismatch in %d of %d block groups (group %s%s); the groups were skipped, their blocks are not reported as free", badCsum, groupsSeen, strings.Join(names, ", "), more)
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
