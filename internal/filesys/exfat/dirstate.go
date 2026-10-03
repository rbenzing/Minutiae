package exfat

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/filesys"
)

const (
	// maxDirBudget is how many bytes of directories one FS instance reads in
	// total (full directory scans; the small windows read to re-parse one entry
	// set are not counted). A volume that needs more is hostile or enormous:
	// reads stop with a warning.
	maxDirBudget = 1 << 30
	// maxDirClaims bounds the directories remembered by their first cluster.
	maxDirClaims = 1 << 20
	// maxResolveDirs bounds the directories scanned to find a directory by its
	// first cluster when the instance has not met it yet.
	maxResolveDirs = 1 << 14
)

// dirSpec says where a directory's data is.
type dirSpec struct {
	first uint32
	size  int64 // DataLength; unused when whole
	noFat bool
	whole bool // the root: follow the FAT chain to its end
}

// dirState is a directory known by its first cluster. Its extent comes only
// from the Stream Extension entry of the first live directory entry set that
// claimed the cluster (the root: the boot sector), never from a caller; the
// clusters are resolved on first use.
type dirState struct {
	spec   dirSpec
	loaded bool
	err    error    // set when no cluster of the directory could be mapped
	exts   []extent // clusters of the directory, in order
	cum    []uint64 // cum[i] is the number of clusters before exts[i]
	limit  uint64   // bytes of directory data (a multiple of 32)
}

// claim records the directory that the live entry set r describes, keyed by its
// first cluster; the first claim wins and a later one with a different extent
// (a cross-linked directory) is a warning. It returns the state to use.
func (f *FS) claim(r *setRec) *dirState {
	spec := dirSpec{first: r.first, size: int64(min(r.dataLen, math.MaxInt64)), noFat: r.noFatChain()}
	f.dmu.Lock()
	defer f.dmu.Unlock()
	if st, ok := f.dirs[spec.first]; ok {
		if st.spec.whole {
			f.warn("directory entry %d claims cluster %d, the root directory's first cluster; the root keeps its own extent", r.idx, spec.first)
		} else if st.spec.size != spec.size || st.spec.noFat != spec.noFat {
			f.warn("directories with different extents (size %d / %d, NoFatChain %v / %v) share first cluster %d (cross-linked); the first one met is used", st.spec.size, spec.size, st.spec.noFat, spec.noFat, spec.first)
		}
		return st
	}
	st := &dirState{spec: spec}
	if len(f.dirs) >= maxDirClaims {
		f.warn("more than %d directories; the rest are not remembered and cannot be reached by id", maxDirClaims)
		return st
	}
	f.dirs[spec.first] = st
	return st
}

func (f *FS) knownDir(first uint32) *dirState {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	return f.dirs[first]
}

// loadDir resolves the clusters of a directory (once). Damage to the chain that
// leaves some clusters is a warning; a directory with no mappable cluster is an
// error.
func (f *FS) loadDir(st *dirState) error {
	f.dmu.Lock()
	defer f.dmu.Unlock()
	if st.loaded {
		return st.err
	}
	st.loaded = true
	sp := st.spec
	maxClusters := max(uint64(maxDirBytes)/uint64(f.cs), 1)
	var (
		exts  []extent
		cerr  error
		limit uint64
	)
	if sp.whole {
		exts, cerr = f.chain(sp.first, maxClusters, false, false)
		limit = clusterTotal(exts) * uint64(f.cs)
		if clusterTotal(exts) == maxClusters {
			f.warn("directory (first cluster %d) is %d MiB or larger; only the first %d MiB are read", sp.first, maxDirBytes>>20, maxDirBytes>>20)
		}
	} else {
		size := sp.size
		if size > maxDirBytes {
			f.warn("directory (first cluster %d): DataLength %d exceeds the %d MiB limit; only the first %d MiB are read", sp.first, sp.size, maxDirBytes>>20, maxDirBytes>>20)
			size = maxDirBytes
		}
		if size <= 0 {
			return nil
		}
		exts, cerr = f.chain(sp.first, uint64(size+f.cs-1)/uint64(f.cs), true, sp.noFat) // size <= 256 MiB: cannot overflow
		limit = uint64(size)
	}
	if cerr != nil {
		if len(exts) == 0 {
			st.err = cerr
			return cerr
		}
		f.warn("directory (first cluster %d): %v; only the part before the damage is read", sp.first, cerr)
	}
	st.exts = exts
	var n uint64
	for _, x := range exts {
		st.cum = append(st.cum, n)
		n += x.n
	}
	st.limit = min(limit, n*uint64(f.cs)) &^ (entrySize - 1)
	return nil
}

func clusterTotal(exts []extent) uint64 {
	var n uint64
	for _, x := range exts {
		n += x.n
	}
	return n
}

// readDirRange reads len(dst) bytes of the directory at byte offset off, which
// must lie inside [0, limit-len(dst)].
func (f *FS) readDirRange(st *dirState, off uint64, dst []byte) error {
	cs := uint64(f.cs)
	for done := 0; done < len(dst); {
		pos := off + uint64(done)
		ci := pos / cs
		i := sort.Search(len(st.cum), func(i int) bool { return st.cum[i] > ci }) - 1
		if i < 0 || ci-st.cum[i] >= st.exts[i].n {
			return fmt.Errorf("directory byte %d is not mapped", pos)
		}
		within := pos - st.cum[i]*cs                            // offset inside the extent
		n := min(uint64(len(dst)-done), st.exts[i].n*cs-within) // the clusters of an extent are consecutive
		if err := readFull(f.r, dst[done:done+int(n)], f.clusterOff(st.exts[i].first)+int64(within)); err != nil {
			return err
		}
		done += int(n)
	}
	return nil
}

// readDirData returns the entries of a directory as one buffer, whole entries
// only, cut at the end-of-directory marker. At most 256 MiB of one directory
// and 1 GiB per FS are read; damage to the chain, a failed read or the
// exhausted budget is a warning and ends the read, and only a directory of
// which nothing can be mapped is an error.
func (f *FS) readDirData(st *dirState) ([]byte, error) {
	if err := f.loadDir(st); err != nil {
		return nil, err
	}
	buf := make([]byte, 0, min(st.limit, 1<<16))
	for off := uint64(0); off < st.limit; {
		n := min(st.limit-off, dirReadChunk)
		f.dmu.Lock()
		over := f.dirBudget < int64(n)
		if !over {
			f.dirBudget -= int64(n)
		}
		f.dmu.Unlock()
		if over {
			f.warn("directory read budget of %d bytes per volume exhausted (directory at first cluster %d); directories are no longer read in full", f.dirBudgetTotal, st.spec.first)
			return buf, nil
		}
		start := len(buf)
		buf = slices.Grow(buf, int(n))[:start+int(n)]
		if err := f.readDirRange(st, off, buf[start:]); err != nil {
			f.warn("directory (first cluster %d): unreadable at byte %d: %v; the rest is not listed", st.spec.first, off, err)
			return buf[:start], nil
		}
		for p := start; p < len(buf); p += entrySize {
			if buf[p] == typeEnd {
				return buf[:p], nil
			}
		}
		off += n
	}
	return buf, nil
}

// resolveDir returns the directory whose first cluster is first, as met on disk
// by this FS instance. hint is the Entry's dirent attribute ("P:I"), used only
// as a hint where to look: the entry set at index I of directory P is
// re-parsed, and nothing is taken from it that the disk does not say. Without a
// hit the directories are scanned breadth-first from the root, within bounds.
func (f *FS) resolveDir(first uint32, hint string) *dirState {
	if st := f.knownDir(first); st != nil {
		return st
	}
	if p, i, ok := strings.Cut(hint, ":"); ok {
		pc, ok1 := parseCanonical(p, 32)
		ic, ok2 := parseCanonical(i, 32)
		if pst := f.knownDir(uint32(pc)); ok1 && ok2 && pst != nil {
			_, _ = f.setIn(pst, int(ic)) // claims the directory it describes
			if st := f.knownDir(first); st != nil {
				return st
			}
		}
	}
	queue := []uint32{f.rootCluster}
	scanned := map[uint32]bool{f.rootCluster: true}
	for len(queue) > 0 && len(scanned) <= maxResolveDirs {
		st := f.knownDir(queue[0])
		queue = queue[1:]
		if st == nil {
			continue
		}
		_ = f.scanDir(st, false, func(r *setRec) bool {
			if r.attrs&attrDirectory != 0 && r.first >= fatReservedClusters && !scanned[r.first] {
				scanned[r.first] = true
				queue = append(queue, r.first)
			}
			return true
		})
		if st := f.knownDir(first); st != nil {
			return st
		}
	}
	return nil
}

// setIn re-parses the entry set at entry index idx of directory st straight
// from the disk. The entry there must be a File entry, live or deleted.
func (f *FS) setIn(st *dirState, idx int) (*setRec, error) {
	notFound := fmt.Errorf("%w: no File entry at index %d of directory %d", filesys.ErrNotFound, idx, st.spec.first)
	if err := f.loadDir(st); err != nil {
		return nil, err
	}
	off := uint64(idx) * entrySize
	if idx < 0 || off >= st.limit {
		return nil, notFound
	}
	win := make([]byte, min(st.limit-off, (maxFileSecondary+1)*entrySize))
	if err := f.readDirRange(st, off, win); err != nil {
		return nil, err
	}
	t := win[0]
	if t != typeFile && t != typeFileDeleted {
		return nil, notFound
	}
	r, _ := f.parseSet(win, 0, idx, st.spec.first, t == typeFileDeleted)
	if r == nil {
		return nil, notFound
	}
	if !r.deleted && !r.damaged && r.attrs&attrDirectory != 0 && r.first >= fatReservedClusters {
		f.claim(r)
	}
	return r, nil
}

// locate returns the entry set that the ID "dirent:P:I" names, read from the
// disk, together with the state of the directory P that holds it.
func (f *FS) locate(p, i uint32) (*setRec, error) {
	st := f.resolveDir(p, "")
	if st == nil {
		return nil, fmt.Errorf("%w: no directory with first cluster %d", filesys.ErrNotFound, p)
	}
	return f.setIn(st, int(i))
}

// dirOf finds the directory that Entry dir stands for, entirely from the disk;
// nothing but the ID (and a dirent attribute used as a search hint) is read from
// the Entry. empty is true for a live directory with no data.
func (f *FS) dirOf(dir filesys.Entry) (st *dirState, empty bool, err error) {
	kind, a, b, err := parseID(dir.ID)
	if err != nil {
		return nil, false, err
	}
	if kind == idDir {
		hint, _ := attrOf(dir, "dirent")
		if st = f.resolveDir(a, hint); st == nil {
			return nil, false, fmt.Errorf("%w: no live directory with first cluster %d", filesys.ErrNotFound, a)
		}
		return st, false, nil
	}
	r, err := f.locate(a, b)
	if err != nil {
		return nil, false, err
	}
	switch {
	case r.deleted:
		return nil, false, filesys.ErrDeleted
	case r.attrs&attrDirectory == 0:
		return nil, false, fmt.Errorf("%w: %q is not a directory", filesys.ErrUnsupported, dir.ID)
	case r.damaged:
		return nil, false, corrupt("exFAT directory", -1, "the entry set of %q is damaged", dir.ID)
	case r.first >= fatReservedClusters:
		return f.claim(r), false, nil
	case r.dataLen == 0:
		return nil, true, nil
	}
	return nil, false, corrupt("exFAT directory", -1, "directory %q has DataLength %d but no first cluster", dir.ID, r.dataLen)
}
