package f2fs

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"sync"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// SIT (segment information table) geometry.
//
// Derivation (include/linux/f2fs_fs.h): struct f2fs_sit_entry is packed as
// vblocks le16, valid_map[SIT_VBLOCK_MAP_SIZE = 64] (one bit per block of the
// 512-block segment) and mtime le64 = 74 bytes; SIT_ENTRY_PER_BLOCK =
// F2FS_BLKSIZE / sizeof(struct f2fs_sit_entry) = 4096 / 74 = 55. The low 10
// bits of vblocks are the valid-block count (SIT_VBLOCKS_MASK), the top 6 the
// segment type. A SIT journal entry is {segno le32, struct f2fs_sit_entry} =
// 78 bytes, so the 507-byte journal (2 bytes of count) holds
// SIT_JOURNAL_ENTRIES = (507 - 2) / 78 = 6 of them.
//
// The valid_map bits are numbered most significant first within each byte (the
// kernel's f2fs_test_bit). A SIT entry is indexed by the segment number
// relative to the first segment of the main area, which is also how the
// superblock's segment_count_main counts: entry s describes the blocks
// [main_blkaddr + s*512, main_blkaddr + (s+1)*512).
const (
	sitJournalEntrySize = 4 + sitEntrySize
	sitJournalEntries   = (sumJournalSize - 2) / sitJournalEntrySize // 6
	sitVBlocksMask      = 0x3ff
	sitMapOff           = 2
	sitMapSize          = blocksPerSeg / 8

	// maxUnallocRuns bounds the runs Unallocated reports; more are cut off
	// with a warning.
	maxUnallocRuns = 1 << 24
)

// sitState caches the SIT journal, which is read once: the entries by segment
// number, the last entry for a segment winning (the kernel applies the
// journal in order at mount). A genuine I/O error is not cached, so a later
// call retries.
type sitState struct {
	mu      sync.Mutex
	loaded  bool
	journal map[uint32][]byte
	err     error
}

// loadSITJournal decodes the SIT journal (the cold-data summary's journal). A
// count beyond the in-block capacity is capped, an entry for a segment
// outside the main area ignored, and a segment listed twice reported, all
// with a warning (once, when the journal is loaded).
func (f *FS) loadSITJournal() (map[uint32][]byte, error) {
	s := &f.sit
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return s.journal, s.err
	}
	j, err := f.summaryJournal(curColdData)
	if err != nil {
		if errors.Is(err, filesys.ErrCorrupt) {
			s.loaded, s.err = true, err
		}
		return nil, err
	}
	s.loaded = true
	n := int(binary.LittleEndian.Uint16(j))
	if n > sitJournalEntries {
		f.warn("SIT journal claims %d entries but holds at most %d; the excess is ignored", n, sitJournalEntries)
		n = sitJournalEntries
	}
	m := make(map[uint32][]byte, n)
	for i := range n {
		e := j[2+i*sitJournalEntrySize:]
		segno := binary.LittleEndian.Uint32(e)
		if segno >= f.sb.segMain {
			f.warn("SIT journal entry for segment %d, but the main area has only %d segments; ignored", segno, f.sb.segMain)
			continue
		}
		if _, dup := m[segno]; dup {
			f.warn("SIT journal lists segment %d more than once; the last entry is used", segno)
		}
		m[segno] = e[4 : 4+sitEntrySize : 4+sitEntrySize]
	}
	s.journal = m
	return m, nil
}

// sitBlockOf reads SIT block idx (the copy the SIT version bitmap selects).
func (f *FS) sitBlockOf(idx uint64) ([]byte, error) {
	addr, ok := versionedBlock(f.sb.sitAddr, f.sb.sitPairs(), f.cp.sitBitmap, idx)
	if !ok {
		return nil, corrupt("f2fs SIT", -1, "SIT block %d is outside the SIT area", idx)
	}
	b, err := readBlock(f.r, addr)
	if err != nil {
		return nil, readError("f2fs SIT", int64(addr)*blockSize, err)
	}
	return b, nil
}

// Unallocated returns the byte runs of the main-area blocks whose bit in the
// segment's SIT valid map is clear, sorted and merged.
//
// A segment's entry is the SIT journal's (it is newer than the last SIT
// flush), else the one in the SIT block the version bitmap selects. The valid
// map is authoritative: a block is reported free only when its bit is clear,
// even if the entry's valid-block count disagrees (that is reported as a
// warning); an entry whose count exceeds the 512 blocks of a segment cannot be
// interpreted and its segment is skipped. A SIT block that cannot be read
// skips its segments, with a warning, and an unreadable journal reports
// nothing, because any segment might be overridden by it. Only blocks inside
// the main area and inside the image are reported.
//
// The current segments of the checkpoint need no special handling: the
// checkpoint flushes their SIT entries (to the journal or to the SIT), so
// every block written so far has its bit set; the blocks beyond a write cursor
// (and the holes behind it) have a clear bit, hold no live data, and are
// unallocated. At most maxUnallocRuns runs are reported (a warning says when
// the rest is cut off).
func (f *FS) Unallocated() ([]filesys.Run, error) {
	sb := f.sb
	journal, err := f.loadSITJournal()
	if err != nil {
		f.warn("SIT journal: cannot be read (%v); no free space is reported", err)
		return nil, nil
	}
	limit := min(sb.mainEnd(), uint64(sb.blocks)) // first block that is not reported
	maxRuns := f.unallocCap
	if maxRuns <= 0 {
		maxRuns = maxUnallocRuns
	}

	var (
		runs   []filesys.Run
		capped bool
		blk    []byte // SIT block curIdx, nil when unreadable
		curIdx = ^uint64(0)
	)
	// emit appends the free blocks [from, to), merging with the previous run.
	emit := func(from, to uint64) {
		off, end := int64(from)*blockSize, int64(to)*blockSize // < 2^32 blocks: no overflow
		if n := len(runs); n > 0 && runs[n-1].Offset+runs[n-1].Length == off {
			runs[n-1].Length = end - runs[n-1].Offset
			return
		}
		if len(runs) >= maxRuns {
			f.warn("unallocated space: more than %d free runs, the rest is not reported", maxRuns)
			capped = true
			return
		}
		runs = append(runs, filesys.Run{Offset: off, Length: end - off})
	}

	for seg := uint32(0); seg < sb.segMain && !capped; seg++ {
		base := uint64(sb.mainAddr) + uint64(seg)<<segShift
		if base >= limit {
			break
		}
		raw, ok := journal[seg]
		if !ok {
			if idx := uint64(seg / sitEntriesPerBlock); idx != curIdx {
				curIdx = idx
				b, err := f.sitBlockOf(idx)
				if err != nil {
					f.warn("SIT block %d is unreadable (%v); its segments are not reported as free", idx, err)
					b = nil
				}
				blk = b
			}
			if blk == nil {
				continue
			}
			off := int(seg%sitEntriesPerBlock) * sitEntrySize
			raw = blk[off : off+sitEntrySize]
		}

		vblocks := int(binary.LittleEndian.Uint16(raw) & sitVBlocksMask)
		if vblocks > blocksPerSeg {
			f.warn("SIT entry of segment %d claims %d valid blocks but a segment holds %d; the segment is not reported as free", seg, vblocks, blocksPerSeg)
			continue
		}
		vmap := raw[sitMapOff : sitMapOff+sitMapSize]
		set := 0
		for _, c := range vmap {
			set += bits.OnesCount8(c)
		}
		if set != vblocks {
			f.warn("SIT entry of segment %d: valid-block count %d disagrees with its valid map (%d bits set); the valid map is used", seg, vblocks, set)
		}
		if set == 0 {
			emit(base, min(base+blocksPerSeg, limit))
			continue
		}
		if set == blocksPerSeg {
			continue
		}
		n := min(uint64(blocksPerSeg), limit-base) // blocks of the segment inside the image
		start := ^uint64(0)                        // first block of the open free run
		for i := uint64(0); i < n && !capped; i++ {
			if vmap[i>>3]&(0x80>>(i&7)) != 0 {
				if start != ^uint64(0) {
					emit(start, base+i)
					start = ^uint64(0)
				}
			} else if start == ^uint64(0) {
				start = base + i
			}
		}
		if start != ^uint64(0) && !capped {
			emit(start, base+n)
		}
	}
	return filesys.MergeRuns(runs), nil
}

// FS implements the whole filesystem interface (Unallocated completes it).
var _ filesys.FileSystem = (*FS)(nil)
