package hfsplus

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	forkTypeData     = 0x00
	forkTypeResource = 0xFF

	cnidExtents = 3 // the extents-overflow file: never has overflow records of its own

	maxExtentsPerFork = 1 << 20 // extents (inline + overflow) kept per fork; more means a trusted prefix
)

// forkMap maps fork-relative byte offsets to volume bytes. exts are the fork's
// extents in order (validated to lie inside the volume); complete is false when
// the extent list is only a trusted PREFIX of the fork (an overflow record was
// missing or the extent cap was reached).
type forkMap struct {
	f         *FS
	exts      []extent
	first     []uint64 // first[i] = fork-relative block number of exts[i]'s first block
	blocks    uint64   // blocks covered by exts
	blockSize int64
	logical   uint64 // the fork's logicalSize
	complete  bool
}

// fork resolves a fork's extents (inline and extents-overflow) into a forkMap.
// The error is a *filesys.CorruptError for inconsistent extents or an I/O
// error from the extents-overflow tree; no map is returned with it (use
// forkPrefix for file content, which keeps the trusted prefix).
func (f *FS) fork(fileID uint32, resource bool, fd forkData) (*forkMap, error) {
	m, err := f.forkPrefix(fileID, resource, fd)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// forkPrefix is fork for file content: on an error the map is still returned,
// holding exactly the extents validated before the failure (the trusted
// prefix, see forkExtents), so a file can be opened with the bytes that can be
// trusted. The error says why the fork is not complete.
func (f *FS) forkPrefix(fileID uint32, resource bool, fd forkData) (*forkMap, error) {
	exts, complete, err := f.forkExtents(fileID, resource, fd)
	m := &forkMap{f: f, exts: exts, blockSize: int64(f.vh.blockSize), logical: fd.logicalSize, complete: complete}
	m.first = make([]uint64, len(exts))
	for i, e := range exts {
		m.first[i] = m.blocks
		m.blocks += uint64(e.count) // <= 2^20 extents x 2^32 blocks: cannot overflow
	}
	return m, err
}

// extentInVolume checks that [start, start+count) lies inside the volume.
func (f *FS) extentInVolume(e extent) bool {
	end, ok := filesys.AddOK(int64(e.start), int64(e.count))
	return ok && end <= int64(f.vh.totalBlocks)
}

// forkExtents lists the extents of a fork: the inline ones (up to the first
// empty one), then those of the extents-overflow tree, looked up by (fileID,
// fork type, first covered block) until fd.totalBlocks are covered. complete is
// false when the list is only a trusted prefix: an overflow record is missing
// or malformed (a warning is recorded) or the per-fork extent cap was reached.
// An extent outside the volume, extents beyond fd.totalBlocks and an overflow
// record that makes no progress, repeats or overshoots are a CorruptError. The
// extents-overflow file (CNID 3) never consults the tree: it must hold all its
// blocks inline.
func (f *FS) forkExtents(fileID uint32, resource bool, fd forkData) (exts []extent, complete bool, err error) {
	var covered uint64
	for i, e := range fd.extents {
		if e.count == 0 {
			break
		}
		if !f.extentInVolume(e) {
			return exts, false, corrupt("fork extents", -1, "extent %d of file %d (%d+%d) is outside the %d-block volume", i, fileID, e.start, e.count, f.vh.totalBlocks)
		}
		covered += uint64(e.count) // at most 8 x 2^32: cannot overflow
		if covered > uint64(fd.totalBlocks) {
			return exts, false, corrupt("fork extents", -1, "the inline extents of file %d hold %d blocks, more than its %d", fileID, covered, fd.totalBlocks)
		}
		exts = append(exts, e)
	}
	total := uint64(fd.totalBlocks)
	if covered == total {
		return exts, true, nil
	}
	if fileID == cnidExtents {
		return exts, false, corrupt("fork extents", -1, "the extents-overflow file claims %d blocks but its inline extents hold %d", total, covered)
	}

	forkType := byte(forkTypeData)
	if resource {
		forkType = forkTypeResource
	}
	seen := map[uint32]struct{}{}
	for covered < total {
		if len(exts) >= f.extentCapOrDefault() {
			f.warn("file %d has more than %d extents: only the first %d blocks of its %d are mapped", fileID, f.extentCapOrDefault(), covered, total)
			return exts, false, nil
		}
		start := uint32(covered) // covered < total <= 2^32
		if _, dup := seen[start]; dup {
			return exts, false, corrupt("extents-overflow tree", -1, "file %d fork %#02x: start block %d looked up twice", fileID, forkType, start)
		}
		seen[start] = struct{}{}
		tree, err := f.tree(treeExtents)
		if err != nil {
			return exts, false, err
		}
		data, found, err := tree.overflowRecord(fileID, forkType, start)
		if err != nil {
			return exts, false, err
		}
		if !found {
			f.warn("file %d fork %#02x: no extents-overflow record at block %d, so only %d of its %d blocks are mapped", fileID, forkType, start, covered, total)
			return exts, false, nil
		}
		if len(data) < maxInlineExts*extentRecSize {
			f.warn("file %d fork %#02x: the extents-overflow record at block %d is %d bytes, too short for %d extents; only %d of %d blocks are mapped", fileID, forkType, start, len(data), maxInlineExts, covered, total)
			return exts, false, nil
		}
		// The extents of a bad record are not trusted: on an error the list
		// returned ends before the record.
		before, kept := covered, len(exts)
		for i := 0; i < maxInlineExts; i++ {
			e := extent{start: binary.BigEndian.Uint32(data[i*extentRecSize:]), count: binary.BigEndian.Uint32(data[i*extentRecSize+4:])}
			if e.count == 0 {
				break
			}
			if !f.extentInVolume(e) {
				return exts[:kept], false, corrupt("extents-overflow tree", -1, "file %d fork %#02x: extent %d of the record at block %d (%d+%d) is outside the %d-block volume", fileID, forkType, i, start, e.start, e.count, f.vh.totalBlocks)
			}
			covered += uint64(e.count)
			if covered > total {
				return exts[:kept], false, corrupt("extents-overflow tree", -1, "file %d fork %#02x: the record at block %d runs past the fork's %d blocks", fileID, forkType, start, total)
			}
			exts = append(exts, e)
		}
		if covered == before {
			return exts[:kept], false, corrupt("extents-overflow tree", -1, "file %d fork %#02x: the record at block %d holds no blocks", fileID, forkType, start)
		}
	}
	return exts, true, nil
}

// extentCapOrDefault is the number of extents kept per fork.
func (f *FS) extentCapOrDefault() int {
	if f.extentCap > 0 {
		return f.extentCap
	}
	return maxExtentsPerFork
}

// overflowRecord finds the extents-overflow record keyed exactly (fileID,
// forkType, startBlock) and returns its data (the extent descriptors). found is
// false when there is none. Two records with the same key are a CorruptError.
func (t *btree) overflowRecord(fileID uint32, forkType byte, startBlock uint32) (data []byte, found bool, err error) {
	cmp := func(key []byte) (int, error) {
		if len(key) != extentsKeyLen {
			return 0, corrupt("extents-overflow tree", -1, "key of %d bytes, not the %d of an extents-overflow key", len(key), extentsKeyLen)
		}
		return compareExtentsKey(key, fileID, forkType, startBlock), nil
	}
	recs, err := t.seek(cmp, 2)
	if err != nil {
		return nil, false, err
	}
	if len(recs) == 0 {
		return nil, false, nil
	}
	key, data, err := splitRecord(recs[0])
	if err != nil {
		return nil, false, err
	}
	if len(key) != extentsKeyLen {
		return nil, false, corrupt("extents-overflow tree", -1, "key of %d bytes, not the %d of an extents-overflow key", len(key), extentsKeyLen)
	}
	switch c := compareExtentsKey(key, fileID, forkType, startBlock); {
	case c < 0: // seek returns the first record not less than the key: a smaller one means broken order or links
		return nil, false, corrupt("extents-overflow tree", -1, "records are out of order around file %d fork %#02x block %d", fileID, forkType, startBlock)
	case c > 0:
		return nil, false, nil
	}
	if len(recs) > 1 {
		if k2, _, err := splitRecord(recs[1]); err == nil && len(k2) == extentsKeyLen && compareExtentsKey(k2, fileID, forkType, startBlock) == 0 {
			return nil, false, corrupt("extents-overflow tree", -1, "two records for file %d fork %#02x at block %d", fileID, forkType, startBlock)
		}
	}
	return data, true, nil
}

// extentsKeyLen is the key length of an extents-overflow record: forkType u8,
// pad u8, fileID u32, startBlock u32.
const extentsKeyLen = 10

// compareExtentsKey orders a record key (the bytes after keyLength) against
// (fileID, forkType, startBlock): fileID, then fork type, then start block.
func compareExtentsKey(key []byte, fileID uint32, forkType byte, startBlock uint32) int {
	be := binary.BigEndian
	if id := be.Uint32(key[2:]); id != fileID {
		if id < fileID {
			return -1
		}
		return 1
	}
	if key[0] != forkType {
		if key[0] < forkType {
			return -1
		}
		return 1
	}
	if sb := be.Uint32(key[6:]); sb != startBlock {
		if sb < startBlock {
			return -1
		}
		return 1
	}
	return 0
}

// volumeOffset maps a fork-relative offset to a volume-relative one (for error
// reports); -1 when the offset lies beyond the mapped extents.
func (m *forkMap) volumeOffset(off int64) int64 {
	if off < 0 {
		return -1
	}
	blk := uint64(off / m.blockSize)
	if blk >= m.blocks {
		return -1
	}
	i := sort.Search(len(m.first), func(i int) bool { return m.first[i] > blk }) - 1
	return int64(m.exts[i].start)*m.blockSize + off - int64(m.first[i])*m.blockSize
}

// ReadAt reads fork bytes at a fork-relative offset, splitting a read that
// spans several extents. A read stops at the fork's logical size (io.EOF).
// Bytes inside the logical size but past the mapped extents are an error
// wrapping filesys.ErrCorrupt (never zeros); I/O errors are returned as they are.
func (m *forkMap) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("hfsplus: negative fork offset %d", off)
	}
	var eof error
	if m.logical <= uint64(off) {
		return 0, io.EOF
	}
	if left := m.logical - uint64(off); left < uint64(len(p)) {
		p, eof = p[:left], io.EOF
	}
	n := 0
	for n < len(p) {
		cur, ok := filesys.AddOK(off, int64(n))
		if !ok {
			return n, fmt.Errorf("hfsplus: fork offset overflows")
		}
		blk := uint64(cur / m.blockSize)
		if blk >= m.blocks {
			return n, fmt.Errorf("%w: fork read at offset %d is beyond its %d mapped blocks", filesys.ErrCorrupt, cur, m.blocks)
		}
		// The extent holding blk: the last whose first block is <= blk.
		i := sort.Search(len(m.first), func(i int) bool { return m.first[i] > blk }) - 1
		e := m.exts[i]
		within := cur - int64(m.first[i])*m.blockSize // < count*blockSize
		avail := int64(e.count)*m.blockSize - within  // <= 2^32 x 2^30
		chunk := int(min(int64(len(p)-n), avail))     // fits int: bounded by len(p)
		volOff := int64(e.start)*m.blockSize + within // < totalBlocks*blockSize <= 2^62
		if err := readFull(m.f.r, p[n:n+chunk], volOff); err != nil {
			return n, err
		}
		n += chunk
	}
	return n, eof
}

// Runs returns the byte runs of the first size bytes of the fork (or of all
// the mapped bytes when the fork maps fewer), in fork order, relative to the
// start of the ReaderAt (the HFS wrapper offset is added). Adjacent runs are
// merged by hand in order. The last run is trimmed to size.
func (m *forkMap) Runs(size int64) []filesys.Run {
	var runs []filesys.Run
	remaining := size
	for _, e := range m.exts {
		if remaining <= 0 {
			break
		}
		length := min(int64(e.count)*m.blockSize, remaining)
		off, ok := filesys.AddOK(m.f.base, int64(e.start)*m.blockSize)
		if !ok {
			break
		}
		remaining -= length
		if n := len(runs); n > 0 && runs[n-1].Offset+runs[n-1].Length == off {
			runs[n-1].Length += length
			continue
		}
		runs = append(runs, filesys.Run{Offset: off, Length: length})
	}
	return runs
}
