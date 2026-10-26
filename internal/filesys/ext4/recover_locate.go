package ext4

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Re-resolution of a deleted-entry ID (3C-R6). The reader returns maps only; a "dirent:" ID names where a record
// lay, and recovery re-reads that one block (or inline area) from disk. Which directory owns the block comes from an
// index of the blocks live directories map, built once per filesystem.

const (
	// maxLocateDirs and maxLocateRuns bound the directory index: the directories it visits and the block runs it
	// stores. maxLocateEntries bounds the live records it examines to find subdirectories. Past a bound the index is
	// capped, and a miss in it is filesys.ErrLimit, never "not found".
	maxLocateDirs, maxLocateRuns = 1 << 18, 1 << 21
	maxLocateEntries             = 1 << 22
)

// deletedID is a parsed canonical "dirent:" ID.
type deletedID struct {
	inline   bool
	blk      uint64 // !inline: the physical block
	dirInode uint32 // inline: the directory inode
	off      int    // the record's offset in the block, or in the inline data
}

// deletedRec is a deleted record re-read from disk.
type deletedRec struct {
	dir   *inode // the live directory that holds the record
	rec   dirRec // the record as re-read (inode 0 = unknown)
	lblk  int64  // logical block of the record's block (-1 inline)
	idStr string
}

// canonDecimal parses a canonical decimal: digits only, no sign, no leading zero, within bitSize bits.
func canonDecimal(s string, bitSize int) (uint64, bool) {
	if s == "" || len(s) > 20 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 10, bitSize)
	return v, err == nil
}

// parseDeletedID accepts only the canonical forms of the IDs this reader makes (direntID): "dirent:<blk>:<off>" and
// "dirent:inline:<ino>:<off>". It does no I/O. Anything else (a leading zero, a sign, a block outside the filesystem,
// an offset that is not a multiple of 4 or leaves no room for a record) is not an ID this reader made.
func (f *FS) parseDeletedID(id string) (deletedID, bool) {
	rest, ok := strings.CutPrefix(id, direntPrefix)
	if !ok || strings.Count(rest, ":") > 2 {
		return deletedID{}, false
	}
	parts := strings.Split(rest, ":")
	switch {
	case len(parts) == 2:
		blk, ok1 := canonDecimal(parts[0], 64)
		off, ok2 := canonDecimal(parts[1], 31)
		if !ok1 || !ok2 || blk >= uint64(f.sb.blocksCount) || off%4 != 0 || off > uint64(f.sb.blockSize-direntMin) {
			return deletedID{}, false
		}
		return deletedID{blk: blk, off: int(off)}, true
	case len(parts) == 3 && parts[0] == "inline":
		ino, ok1 := canonDecimal(parts[1], 32)
		off, ok2 := canonDecimal(parts[2], 31)
		if !ok1 || !ok2 || ino < 1 || ino > uint64(f.sb.inodesCount) || off%4 != 0 || off < 4 || off >= maxInlineData {
			return deletedID{}, false
		}
		return deletedID{inline: true, dirInode: uint32(ino), off: int(off)}, true
	}
	return deletedID{}, false
}

func (d deletedID) String() string {
	if d.inline {
		return fmt.Sprintf("dirent:inline:%d:%d", d.dirInode, d.off)
	}
	return fmt.Sprintf("dirent:%d:%d", d.blk, d.off)
}

// locDir is one directory of the index.
type locDir struct {
	num         uint32
	isizeBlocks int64
}

// locRun is a run of a directory's mapped physical blocks.
type locRun struct {
	start, n, lblk int64 // physical start block, block count, logical block of the first
	dir            int32 // index into dirIndex.dirs
}

// dirIndex maps physical blocks to the live directory that maps them. runs is sorted by start; ends holds the same
// runs' end blocks sorted; maxEnd[i] is the index of the run with the largest end among runs[:i+1]. Together they
// answer "how many runs contain block b" and "which one" in O(log n).
type dirIndex struct {
	dirs   []locDir
	inline map[uint32]struct{} // inline directories, by inode
	runs   []locRun
	ends   []int64
	maxEnd []int32
	// capped is the first bound the build reached; cut is the first reason a directory map or scan was cut. Either
	// makes a miss undecidable.
	capped string
	cut    scanCut
}

// quietView returns a view of f with a private warning collector, so that recovery reads (3C-R5) add no filesystem
// warning: stale and cross-linked structures are not damage of the live filesystem. Callers hold recMu.
func (f *FS) quietView() *FS {
	if f.quiet == nil {
		f.quiet = &FS{
			sb: f.sb, groups: f.groups, r: f.r, data: f.data, size: f.size, metaRanges: f.metaRanges,
			dirRecordCap: f.dirRecordCap, slackScanCap: f.slackScanCap, xattrCap: f.xattrCap, eaHashCap: f.eaHashCap,
			locateDirsCap: f.locateDirsCap, locateRunsCap: f.locateRunsCap,
		}
	}
	return f.quiet
}

// locator returns the quiet view and the directory index, building the index on first use.
func (f *FS) locator() (*FS, *dirIndex) {
	f.recMu.Lock()
	defer f.recMu.Unlock()
	q := f.quietView()
	if f.locIdx == nil {
		f.locIdx = f.buildDirIndex(q)
	}
	return q, f.locIdx
}

// buildDirIndex walks the live directories breadth-first from the root, once each (a visited set by inode), and
// stores the physical runs each one maps. Every bound is a cap flag, never a silent cut.
func (f *FS) buildDirIndex(q *FS) *dirIndex {
	idx := &dirIndex{inline: map[uint32]struct{}{}}
	capAt := func(format string, a ...any) {
		if idx.capped == "" {
			idx.capped = fmt.Sprintf(format, a...)
		}
	}
	cut := func(c scanCut, format string, a ...any) {
		if idx.cut.reason == "" {
			idx.cut = scanCut{reason: fmt.Sprintf(format, a...) + c.reason, limit: c.limit}
		}
	}
	bs := int64(q.sb.blockSize)
	var totalBlocks, entries int64
	seen := map[uint32]struct{}{rootInode: {}}
	queue := []uint32{rootInode}
	rootIn, err := q.inode(rootInode)
	if err != nil || rootIn.mode&modeTypeMask != modeDir {
		cut(scanCut{}, "the root directory is unreadable or not a directory")
		return idx
	}
	for head := 0; head < len(queue); head++ {
		n := queue[head]
		in, err := q.inode(n)
		if err != nil {
			cut(scanCut{reason: err.Error()}, "directory inode %d is unreadable: ", n)
			continue
		}
		f.locateWork.Add(1)
		d := int32(len(idx.dirs))
		if in.flags&inodeFlagInlineData != 0 {
			idx.dirs = append(idx.dirs, locDir{num: n})
			idx.inline[n] = struct{}{}
		} else {
			runs, isizeBlocks, _ := q.dirRuns(in) // a damaged map is reported by the scan below, with the runs before it
			idx.dirs = append(idx.dirs, locDir{num: n, isizeBlocks: isizeBlocks})
			var lblk int64
			for _, r := range runs {
				if r.Offset < 0 {
					lblk += r.Length / bs
					continue
				}
				nb := r.Length / bs
				totalBlocks += nb
				if totalBlocks > q.sb.blocksCount {
					// Honest directories own distinct blocks, so their sum fits the filesystem; more means
					// cross-linked maps that would make the index read the same blocks again and again.
					capAt("the directories map more blocks than the filesystem holds")
					return idx.finish()
				}
				if len(idx.runs) >= f.locateRunsCap {
					capAt("more than %d directory block runs", f.locateRunsCap)
					return idx.finish()
				}
				f.locateWork.Add(1)
				idx.runs = append(idx.runs, locRun{start: r.Offset / bs, n: nb, lblk: lblk, dir: d})
				lblk += nb
			}
		}
		stopped := false
		c, err := q.scanDirCut(in, false, func(r *dirRec) bool {
			if r.inode == 0 {
				return true
			}
			if entries++; entries > maxLocateEntries {
				capAt("more than %d directory records examined", maxLocateEntries)
				stopped = true
				return false
			}
			if _, ok := seen[r.inode]; ok {
				return true
			}
			ci, err := q.rawInode(r.inode)
			if err != nil {
				cut(scanCut{reason: err.Error()}, "inode %d, named by a record of directory inode %d, is unreadable: ", r.inode, n)
				return true
			}
			if ci.mode&modeTypeMask != modeDir {
				return true
			}
			if len(seen) >= f.locateDirsCap {
				capAt("more than %d directories", f.locateDirsCap)
				return true
			}
			seen[r.inode] = struct{}{}
			queue = append(queue, r.inode)
			return true
		})
		if err != nil {
			cut(scanCut{reason: err.Error()}, "directory inode %d cannot be scanned: ", n)
		}
		if c.reason != "" {
			cut(c, "directory inode %d: ", n)
		}
		if stopped {
			break
		}
	}
	return idx.finish()
}

// finish sorts the runs and builds the lookup arrays.
func (idx *dirIndex) finish() *dirIndex {
	slices.SortFunc(idx.runs, func(a, b locRun) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})
	idx.ends = make([]int64, len(idx.runs))
	idx.maxEnd = make([]int32, len(idx.runs))
	best := int32(0)
	for i, r := range idx.runs {
		idx.ends[i] = r.start + r.n
		if i == 0 || r.start+r.n > idx.runs[best].start+idx.runs[best].n {
			best = int32(i)
		}
		idx.maxEnd[i] = best
	}
	slices.Sort(idx.ends)
	return idx
}

// owner reports how many runs contain block blk and, when it is one, which.
func (idx *dirIndex) owner(blk int64) (count int, run locRun) {
	started := sort.Search(len(idx.runs), func(i int) bool { return idx.runs[i].start > blk }) // runs with start <= blk
	ended := sort.Search(len(idx.ends), func(i int) bool { return idx.ends[i] > blk })         // runs with end <= blk
	count = started - ended
	if count == 1 {
		run = idx.runs[idx.maxEnd[started-1]]
	}
	return count, run
}

// missErr is the answer for an ID the index does not place: not found when the index is complete, else a typed
// undecided answer (3B F81: a capped or cut search never says "not found").
func (idx *dirIndex) missErr(id deletedID) error {
	switch {
	case idx.capped != "":
		return fmt.Errorf("ext4: %w: %w: the directory index is capped (%s), so %s cannot be placed", filesys.ErrLimit, filesys.ErrIncomplete, idx.capped, id)
	case idx.cut.reason != "" && idx.cut.limit:
		return fmt.Errorf("ext4: %w: %w: the directory index was cut (%s), so %s cannot be placed", filesys.ErrLimit, filesys.ErrIncomplete, idx.cut.reason, id)
	case idx.cut.reason != "":
		return fmt.Errorf("%w: %w", corrupt("ext4 directory", -1, "the directory index was cut (%s), so %s cannot be placed", idx.cut.reason, id), filesys.ErrIncomplete)
	}
	return fmt.Errorf("%w: no live directory maps %s", filesys.ErrNotFound, id)
}

// resolveDeleted re-reads the record id names from disk: the directory that owns the block comes from the index,
// then that one block (or the inline area) is scanned through the directory's own scan context. The answer is the
// record as it is now (a live record is filesys.ErrNotDeleted), or a typed error; a miss in an index that is capped
// or cut is never "not found".
func (f *FS) resolveDeleted(id deletedID) (*deletedRec, error) {
	q, idx := f.locator()
	var (
		dir   locDir
		lblk  = int64(-1)
		blk   int64
		found bool
	)
	if id.inline {
		if _, ok := idx.inline[id.dirInode]; !ok {
			return nil, idx.missErr(id)
		}
		dir = locDir{num: id.dirInode}
	} else {
		blk = int64(id.blk)
		n, run := idx.owner(blk)
		switch {
		case n == 0:
			return nil, idx.missErr(id)
		case n > 1:
			return nil, corrupt("ext4 directory", blk*int64(q.sb.blockSize), "block %d is mapped by %d directory block runs (cross-linked directories)", blk, n)
		}
		dir = idx.dirs[run.dir]
		lblk = run.lblk + (blk - run.start)
	}
	in, err := q.inode(dir.num)
	if err != nil {
		return nil, fmt.Errorf("ext4: directory inode %d of %s: %w", dir.num, id, err)
	}
	var rec dirRec
	w := &dirScan{
		f: q, in: in, wantDeleted: true, index: in.flags&inodeFlagIndex != 0, enc: in.flags&inodeFlagEncrypt != 0,
		isizeBlocks: dir.isizeBlocks,
		visit: func(r *dirRec) bool {
			if r.off == id.off {
				rec, found = *r, true
				return false
			}
			return true
		},
	}
	if q.sb.metadataCsum() {
		w.seed = q.inodeSeed(in)
	}
	f.locateWork.Add(1)
	if id.inline {
		if in.flags&inodeFlagInlineData == 0 {
			return nil, fmt.Errorf("%w: directory inode %d no longer holds inline data", filesys.ErrNotFound, dir.num)
		}
		if err := w.inline(); err != nil {
			return nil, fmt.Errorf("ext4: re-reading the inline directory %d: %w", dir.num, err)
		}
	} else {
		bs := int64(q.sb.blockSize)
		buf := make([]byte, bs) // one block: bounded by the block size
		if err := readFull(q.r, buf, blk*bs); err != nil {
			return nil, fmt.Errorf("ext4: re-reading directory block %d: %w", blk, err)
		}
		w.block(buf, blk, lblk)
	}
	switch {
	case !found && w.cut.reason != "":
		return nil, incompleteLookup(dir.num, id.String(), w.cut)
	case !found:
		return nil, fmt.Errorf("%w: no record at %s", filesys.ErrNotFound, id)
	case !rec.deleted:
		return nil, fmt.Errorf("%w: the record at %s is live", filesys.ErrNotDeleted, id)
	}
	return &deletedRec{dir: in, rec: rec, lblk: lblk, idStr: id.String()}, nil
}
