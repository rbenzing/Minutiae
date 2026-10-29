package ext4

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Recovery methods (spec 3.3). Task 9 adds the leaf source; 3G adds the journal source.
const (
	MethodInodeIntact = "ext-inode-intact"
	MethodExtentLeaf  = "ext4-extent-leaf"
)

// maxBasisTreeBlocks bounds the map blocks a Basis names one by one; past it the count is stated.
const maxBasisTreeBlocks = 16

var _ filesys.Recoverer = (*FS)(nil)

// Recoverable returns where the content of the deleted entry e may still be. Only e.ID is used. The pipeline is:
// parse the ID (canonical forms only, no I/O), re-resolve the record from disk (resolveDeleted), accept the stale inode
// it names (staleInodeFor), then ask each candidate source. With no candidate the answer is nil, nil: nothing the
// reader can claim. Every returned slice is fresh. Typed errors (filesys.ErrNotFound, ErrNotDeleted, ErrReused,
// ErrUndecided, ErrLimit, ErrIncomplete, ErrUnsupported, *filesys.CorruptError, or an I/O error) pass through as the
// earlier stages made them.
func (f *FS) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	if n, err := parseInodeID(e.ID); err == nil {
		if n > f.sb.inodesCount {
			return nil, fmt.Errorf("%w: %q names an inode beyond the %d the filesystem holds", filesys.ErrNotFound, e.ID, f.sb.inodesCount)
		}
		return nil, fmt.Errorf("%w: %s is a live entry id", filesys.ErrNotDeleted, e.ID)
	}
	id, ok := f.parseDeletedID(e.ID)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a deleted-entry id this reader made", filesys.ErrNotFound, e.ID)
	}
	r, err := f.resolveDeleted(id)
	if err != nil {
		return nil, err
	}
	st, err := f.staleInodeFor(r)
	if err != nil {
		return nil, err
	}
	var out []filesys.Candidate
	if c, ok, err := f.intactCandidate(st); err != nil {
		return nil, err
	} else if ok {
		out = append(out, c)
	}
	return out, nil
}

// intactCandidate is the ext-inode-intact source (3C-R10): the stale inode's own map, walked from logical block 0 up
// to its first stop. The candidate exists when the inode still claims a size and the map yields at least one run, or
// stops before its first run for a reason worth reporting (a hole or an uninitialized extent at block 0 is a stop, not
// silence). A map that holds nothing at all, which is what a kernel unlink leaves, is no candidate.
func (f *FS) intactCandidate(st *staleInode) (filesys.Candidate, bool, error) {
	in := st.in
	switch in.mode & modeTypeMask {
	case modeReg, modeSymlink:
	default:
		return filesys.Candidate{}, false, fmt.Errorf("%w: inode %d is neither a regular file nor a symlink, so it has no data map to recover", filesys.ErrUnsupported, in.num)
	}
	if in.size <= 0 {
		return filesys.Candidate{}, false, nil
	}
	m, err := f.mapStale(in, min(in.size, f.size))
	if err != nil {
		return filesys.Candidate{}, false, err
	}
	if m.cleared || (len(m.runs) == 0 && m.stop == "") {
		return filesys.Candidate{}, false, nil
	}
	kind := "blockmap"
	if in.flags&inodeFlagExtents != 0 {
		kind = "extents"
	}
	basis := []string{st.rec.idStr, "inode=" + strconv.FormatUint(uint64(in.num), 10), "generation=" + strconv.FormatUint(uint64(in.generation), 10), "map=" + kind}
	for i, blk := range m.tree {
		if i == maxBasisTreeBlocks {
			basis = append(basis, "tree-blocks="+strconv.Itoa(len(m.tree)))
			break
		}
		basis = append(basis, "tree-block="+strconv.FormatUint(blk, 10))
	}
	assumptions := []string{"inode-not-reused-since-entry-deleted", "size-from-inode"}
	return f.candidateFrom(st, MethodInodeIntact, m, in.size, basis, assumptions), true, nil
}

// candidateFrom builds a Candidate from any decoded stale inode and map (3C-R15: 3G reuses it for an inode taken from
// a journal copy). The common claims live here: the stop, the unclean-image assumption, the deletion-time check, the
// untrusted times and mode, and the encryption flag. Basis and assumptions are numbers and fixed tokens only; a
// deleted name never enters them (3C-R16).
func (f *FS) candidateFrom(st *staleInode, method string, m staleMap, size int64, basis, assumptions []string) filesys.Candidate {
	in := st.in
	c := filesys.Candidate{
		Method:      method,
		Size:        size,
		Runs:        slices.Clone(m.runs),
		Basis:       slices.Clone(basis),
		Assumptions: slices.Clone(assumptions),
		Times:       in.times,
		Mode:        uint32(in.mode),
		Encrypted:   in.flags&inodeFlagEncrypt != 0,
		Warnings:    slices.Concat(st.notes, m.notes),
	}
	if m.stop != "" {
		c.Assumptions = append(c.Assumptions, "stale-chain-stop="+m.stop)
	}
	if f.sb.hasIncompat(incompatRecover) {
		// The on-disk inode, bitmaps and map blocks may predate committed journal transactions (3C-R24).
		c.Assumptions = append(c.Assumptions, "journal-not-replayed")
	}
	if d, ch := in.times.Deleted.T, in.times.Changed.T; !d.IsZero() && !ch.IsZero() && d.Unix() < ch.Unix() {
		// The check flags, it does not decide reuse: the dirent holds no generation to compare (3C-R32).
		c.Assumptions = append(c.Assumptions, "dtime-before-ctime")
		c.Warnings = append(c.Warnings, "the inode's deletion time is before its change time, so it may have been changed after it was deleted")
	}
	return c
}
