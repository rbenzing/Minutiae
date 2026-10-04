package hfsplus

import (
	"encoding/binary"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Catalog node ids of the special files (TN1150 "Special File CNIDs"); the
// extents-overflow file (3) is cnidExtents, in fork.go.
const (
	cnidCatalog    = 4
	cnidBadBlocks  = 5
	cnidAllocation = 6
	cnidStartup    = 7
	cnidAttributes = 8
)

const (
	// maxUnallocRuns bounds the free runs Unallocated reports; more are cut off
	// with a warning.
	maxUnallocRuns = 1 << 24
	// allocChunk is how much of the allocation file is read at a time.
	allocChunk = 64 << 10
	// maxBadBlockRecords bounds the extents-overflow records read for the
	// bad-block file; more and the bad blocks cannot all be known.
	maxBadBlockRecords = 1 << 16
)

// Unallocated returns the byte runs (from the start of the ReaderAt, so the HFS
// wrapper offset is included) of the allocation blocks the allocation file
// marks free (a clear bit, MSB first), sorted and merged. Free space is only
// reported when the volume's allocation state can be trusted:
//
//   - a journal with pending transactions, or one that cannot be parsed, means
//     the on-disk bitmap may be stale: Unallocated fails with an error wrapping
//     filesys.ErrUnsupported and reports nothing;
//   - a system file whose extents cannot be fully resolved (or an unreadable
//     bad-block record) means some protected blocks are unknown: nothing is
//     reported, with a warning;
//   - blocks the bitmap does not cover (a short allocation file, an unreadable
//     part) are never reported as free, with a warning; bits at or past
//     totalBlocks are ignored whatever their value; blocks the image does not
//     hold are clipped.
//
// Whatever the bitmap says, these are never reported: the boot area and volume
// header (bytes [0, 1536)), the alternate header (the last 1024 bytes of the
// volume), the extents of the allocation, extents-overflow, catalog, attributes
// and startup files (overflow extents included), the bad-block file's extents,
// the journal info block and the journal. The bitmap's own free count is
// compared with the volume header's (a mismatch is a warning), and a volume
// that was not cleanly unmounted warns that the bitmap may be out of date. At
// most maxUnallocRuns runs are reported (a warning says when the rest is cut off).
func (f *FS) Unallocated() ([]filesys.Run, error) {
	switch f.journal {
	case journalPending:
		return nil, fmt.Errorf("%w: the journal has pending transactions that were never replayed, so the allocation bitmap on disk may be stale; no free space is reported", filesys.ErrUnsupported)
	case journalUnknown:
		return nil, fmt.Errorf("%w: the journal state cannot be determined, so the allocation bitmap on disk cannot be vouched for; no free space is reported", filesys.ErrUnsupported)
	}
	vh := f.vh
	bs := int64(vh.blockSize)
	total := int64(vh.totalBlocks)
	avail := min(total, (f.size-f.base)/bs) // whole blocks the image holds
	if vh.attributes&attrUnmounted == 0 {
		f.warn("the volume was not cleanly unmounted: the allocation bitmap may be out of date")
	}

	prot, bitmap, ok := f.protectedBlocks()
	if !ok {
		return nil, nil // the warning is recorded
	}

	// The part of the allocation file that holds bits for blocks below total.
	need := (uint64(total) + 7) / 8
	have := min(vh.allocation.logicalSize, need)
	if have < need {
		f.warn("the allocation file holds %d bytes, covering only %d of %d blocks; the blocks beyond are not reported as free", vh.allocation.logicalSize, have*8, total)
	}

	var (
		runs      []filesys.Run
		capped    bool
		pi        int // first protected range that may still matter (emit is called in ascending order)
		runStart  = int64(-1)
		clearBits int64 // clear bits among the first bitsDone blocks
	)
	runCap := maxUnallocRuns
	if f.unallocCap > 0 {
		runCap = f.unallocCap
	}
	// emit appends the blocks [s, e) that the image holds and no protected range covers.
	emit := func(s, e int64) {
		e = min(e, avail)
		for s < e {
			for pi < len(prot) && prot[pi].Offset+prot[pi].Length <= s {
				pi++
			}
			if pi < len(prot) && prot[pi].Offset <= s {
				s = prot[pi].Offset + prot[pi].Length
				continue
			}
			stop := e
			if pi < len(prot) && prot[pi].Offset < e {
				stop = prot[pi].Offset
			}
			if len(runs) >= runCap {
				f.warn("unallocated space: more than %d free runs, the rest is not reported", runCap)
				capped = true
				return
			}
			runs = append(runs, filesys.Run{Offset: f.base + s*bs, Length: (stop - s) * bs}) // inside the image: cannot overflow
			s = stop
		}
	}
	closeRun := func(end int64) {
		if runStart >= 0 {
			emit(runStart, end)
			runStart = -1
		}
	}

	var bitsDone int64 // blocks whose bit was read
	buf := make([]byte, allocChunk)
	for read := uint64(0); read < have && !capped; {
		n := min(uint64(len(buf)), have-read)
		if err := readFull(bitmap, buf[:n], int64(read)); err != nil { // read < have <= 2^29
			f.warn("allocation file: unreadable at byte %d (%v); the blocks from there on are not reported as free", read, err)
			break
		}
		for i := uint64(0); i < n && !capped; i++ {
			first := int64(read+i) * 8
			if first >= total {
				break
			}
			nb := min(8, total-first)
			switch b := buf[i]; {
			case b == 0xFF:
				closeRun(first)
			case b == 0 && nb == 8:
				if runStart < 0 {
					runStart = first
				}
				clearBits += 8
			default:
				for k := int64(0); k < nb && !capped; k++ {
					if b&(0x80>>k) != 0 {
						closeRun(first + k)
					} else {
						if runStart < 0 {
							runStart = first + k
						}
						clearBits++
					}
				}
			}
		}
		read += n
		bitsDone = min(int64(read)*8, total)
	}
	if !capped {
		closeRun(bitsDone)
	}
	if !capped && bitsDone == total && clearBits != int64(vh.freeBlocks) {
		f.warn("the volume header says %d free blocks but the allocation file has %d clear bits", vh.freeBlocks, clearBits)
	}
	return filesys.MergeRuns(runs), nil
}

// protectedBlocks returns, as sorted merged ranges of allocation blocks (Run
// offsets and lengths are block numbers and counts here), everything that is
// never free space, and the allocation file's fork map. ok is false, with a
// warning, when some of it cannot be determined.
func (f *FS) protectedBlocks() (prot []filesys.Run, bitmap *forkMap, ok bool) {
	vh := f.vh
	bs := int64(vh.blockSize)
	total := int64(vh.totalBlocks)
	add := func(start, count int64) {
		if start < 0 || count <= 0 || start >= total {
			return
		}
		prot = append(prot, filesys.Run{Offset: start, Length: min(count, total-start)})
	}
	add(0, (vhOffset+vhSize+bs-1)/bs) // boot area and volume header
	altStart := (vh.volumeBytes() - altFromEnd) / bs
	add(altStart, total-altStart) // alternate header: the last 1024 bytes of the volume
	if jb := int64(vh.journalInfoBlock); jb != 0 {
		add(jb, 1)
	}
	if f.jrnlSize > 0 {
		first := f.jrnlOff / bs
		add(first, (f.jrnlOff+f.jrnlSize+bs-1)/bs-first)
	}

	for _, sf := range []struct {
		name string
		id   uint32
		fd   forkData
	}{
		{"allocation file", cnidAllocation, vh.allocation},
		{"extents-overflow file", cnidExtents, vh.extents},
		{"catalog file", cnidCatalog, vh.catalog},
		{"attributes file", cnidAttributes, vh.attributesFile},
		{"startup file", cnidStartup, vh.startup},
	} {
		m, err := f.forkPrefix(sf.id, false, sf.fd)
		if err != nil || !m.complete {
			f.warn("unallocated space: the extents of the %s cannot be fully resolved (%v); no free space is reported", sf.name, errOrIncomplete(err))
			return nil, nil, false
		}
		for _, e := range m.exts {
			add(int64(e.start), int64(e.count))
		}
		if sf.id == cnidAllocation {
			bitmap = m
		}
	}

	if vh.extents.totalBlocks > 0 {
		if !f.addBadBlocks(add) {
			return nil, nil, false
		}
	}
	return filesys.MergeRuns(prot), bitmap, true
}

func errOrIncomplete(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("an extents-overflow record is missing or the extent cap was reached")
}

// addBadBlocks adds the extents of the bad-block file (CNID 5), which has no
// fork in the volume header: its extents are all records of the
// extents-overflow tree. An extent that reaches outside the volume is clipped
// with a warning. false, with a warning, when the records cannot all be read.
func (f *FS) addBadBlocks(add func(start, count int64)) bool {
	tree, err := f.tree(treeExtents)
	if err != nil {
		f.warn("unallocated space: the extents-overflow tree cannot be opened (%v), so the bad-block file is unknown; no free space is reported", err)
		return false
	}
	total := int64(f.vh.totalBlocks)
	cmp := func(key []byte) (int, error) {
		if len(key) != extentsKeyLen {
			return 0, corrupt("extents-overflow tree", -1, "key of %d bytes, not the %d of an extents-overflow key", len(key), extentsKeyLen)
		}
		return compareExtentsKey(key, cnidBadBlocks, forkTypeData, 0), nil
	}
	records := 0
	tooMany := false
	err = tree.scanFrom(cmp, func(rec []byte) (bool, error) {
		key, data, err := splitRecord(rec)
		if err != nil {
			return false, err
		}
		if len(key) != extentsKeyLen {
			return false, corrupt("extents-overflow tree", -1, "key of %d bytes, not the %d of an extents-overflow key", len(key), extentsKeyLen)
		}
		if binary.BigEndian.Uint32(key[2:]) != cnidBadBlocks {
			return false, nil // the records are ordered by file id: the bad-block records are over
		}
		if records++; records > maxBadBlockRecords {
			tooMany = true
			return false, nil
		}
		if len(data) < maxInlineExts*extentRecSize {
			return false, corrupt("extents-overflow tree", -1, "bad-block record of %d bytes, too short for %d extents", len(data), maxInlineExts)
		}
		for i := 0; i < maxInlineExts; i++ {
			start := int64(binary.BigEndian.Uint32(data[i*extentRecSize:]))
			count := int64(binary.BigEndian.Uint32(data[i*extentRecSize+4:]))
			if count == 0 {
				break
			}
			if start+count > total {
				f.warn("unallocated space: bad block extent %d+%d reaches outside the %d-block volume; the part inside is kept out of the free space", start, count, total)
			}
			add(start, count)
		}
		return true, nil
	})
	if err == nil && tooMany {
		err = fmt.Errorf("more than %d bad-block records", maxBadBlockRecords)
	}
	if err != nil {
		f.warn("unallocated space: the bad-block file's extents cannot be read (%v); no free space is reported", err)
		return false
	}
	return true
}
