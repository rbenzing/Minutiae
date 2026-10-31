package ext4

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The ext4-journal-inode source (3G-R11, R12, R13): a deleted file's inode as an older inode-table block of the journal
// still holds it, with its map blocks taken from the same point in time. The journal holds copies of blocks as they
// were when a transaction committed, so every claim here is historical, never live (spec 9.1).

const (
	// stopJournalCopyUnreadable stops a map when the journal copy of a map block lies beyond the end of the image.
	stopJournalCopyUnreadable = "journal-copy-unreadable"

	// maxJournalCopiesPerEntry bounds the copies of an inode-table block one entry examines. Reaching it makes the
	// journal source unknown for the entry (filesys.ErrLimit), never "no candidate".
	maxJournalCopiesPerEntry = 1 << 16
)

var _ filesys.JournalRecoverer = (*FS)(nil)

// jrecState is what PrepareJournalRecovery builds. It is immutable once stored.
type jrecState struct {
	use         filesys.JournalUse
	idx         *jindex
	unusable    error               // the reason the journal source answers unknown
	inodeCopies map[uint64][]uint32 // inode-table fs block -> tag indexes, newest first
	byBlock     []uint32            // every tag index ordered by fs block, then newest first
}

// journalUnknownError is leafUnknownError for the journal source: it cannot say whether it has an answer.
type journalUnknownError = leafUnknownError

func unknownJournal(kind error, warning string) *journalUnknownError {
	return &journalUnknownError{kind: kind, warning: warning}
}

// allowed reports whether the transaction may be a source under use: committed, and in an allowed region.
func (s *jrecState) allowed(t *jtxn) bool { return journalAllowed(s.use, t) }

func journalAllowed(use filesys.JournalUse, t *jtxn) bool {
	return t.committed && (use == filesys.JournalUseStale || !t.stale)
}

// eligibleCopy reports whether the logged bytes of a tag have a state that lets them be a source.
func eligibleCopy(g *jtag) bool { return g.copy == copyVerified || g.copy == copyUnverified }

// newest orders tag indexes newest first (3G-R31): by sequence (wrap-aware), live before stale on a tie, then by the
// later start block, then by the later tag.
func (x *jindex) newest(a, b uint32) int {
	ga, gb := &x.tags[a], &x.tags[b]
	ta, tb := &x.txns[ga.txn], &x.txns[gb.txn]
	switch {
	case ta.seq != tb.seq:
		if seqNewer(ta.seq, tb.seq) {
			return -1
		}
		return 1
	case ta.stale != tb.stale:
		if !ta.stale {
			return -1
		}
		return 1
	}
	return cmp.Or(cmp.Compare(tb.start, ta.start), cmp.Compare(b, a))
}

// PrepareJournalRecovery builds the index the journal source needs (filesys.JournalRecoverer): the journal index, and
// the tags that are copies of an inode-table block. It is idempotent, honours ctx, and never writes. Use none clears
// the preparation. An unusable journal (corrupt, unsupported, absent, over the byte cap) is recorded and returns nil:
// the journal source then answers unknown. An I/O error and a cancel are returned and leave nothing prepared.
func (f *FS) PrepareJournalRecovery(ctx context.Context, use filesys.JournalUse) error {
	u, err := filesys.ParseJournalUse(string(use))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.jrecMu.Lock()
	defer f.jrecMu.Unlock()
	if u == filesys.JournalUseNone {
		f.jrec = nil
		return nil
	}
	if f.jrec != nil {
		if f.jrec.use != u {
			next := *f.jrec
			next.use = u
			f.jrec = &next
		}
		return nil
	}
	s := &jrecState{use: u}
	x, err := f.journalIndex()
	switch {
	case err == nil:
		s.idx = x
	case errors.Is(err, filesys.ErrNoJournal), errors.Is(err, filesys.ErrUnsupported),
		errors.Is(err, filesys.ErrLimit), errors.Is(err, filesys.ErrCorrupt):
		s.unusable = err
	default:
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.idx != nil {
		if err := f.indexCopies(ctx, s); err != nil {
			return err
		}
	}
	f.jrec = s
	return nil
}

// itableRanges is the merged, sorted list of [start, end) fs blocks of the inode tables of the groups whose descriptor
// is usable.
func (f *FS) itableRanges() [][2]uint64 {
	var rs [][2]uint64
	for i := range f.groups {
		if g := &f.groups[i]; !g.bad {
			rs = append(rs, [2]uint64{g.inodeTable, g.inodeTable + uint64(f.sb.itableBlocks)})
		}
	}
	slices.SortFunc(rs, func(a, b [2]uint64) int { return cmp.Compare(a[0], b[0]) })
	var out [][2]uint64
	for _, r := range rs {
		if n := len(out); n > 0 && r[0] <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], r[1])
			continue
		}
		out = append(out, r)
	}
	return out
}

// indexCopies orders the tags by block, newest first, and groups the copies of inode-table blocks. Identity comes from
// the block number, never from the content of a copy (3G-R11).
func (f *FS) indexCopies(ctx context.Context, s *jrecState) error {
	x := s.idx
	s.byBlock = make([]uint32, len(x.tags)) // bounded by the tag cap
	for i := range s.byBlock {
		s.byBlock[i] = uint32(i)
	}
	slices.SortFunc(s.byBlock, func(a, b uint32) int {
		if fa, fb := x.tags[a].fsblk, x.tags[b].fsblk; fa != fb {
			return cmp.Compare(fa, fb)
		}
		return x.newest(a, b)
	})
	rs := f.itableRanges()
	s.inodeCopies = map[uint64][]uint32{}
	for n, ti := range s.byBlock {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		blk := x.tags[ti].fsblk
		k := sort.Search(len(rs), func(i int) bool { return rs[i][1] > blk })
		if k < len(rs) && rs[k][0] <= blk {
			s.inodeCopies[blk] = append(s.inodeCopies[blk], ti)
		}
	}
	return nil
}

// journalCandidates is the ext4-journal-inode source. Without PrepareJournalRecovery it answers nothing, so Recoverable
// is 3C's. The notes are reasons a copy was found but not used; Recoverable puts them on the entry's other candidates.
// An error of type *journalUnknownError means the source cannot say whether it has an answer.
func (f *FS) journalCandidates(st *staleInode) ([]filesys.Candidate, []string, error) {
	f.jrecMu.Lock()
	s := f.jrec
	f.jrecMu.Unlock()
	if s == nil {
		return nil, nil, nil
	}
	if s.unusable != nil {
		return nil, nil, unknownJournal(
			fmt.Errorf("%w: the journal could not be searched: %v", filesys.ErrUndecided, s.unusable),
			"journal not searched: "+s.unusable.Error())
	}
	x, in := s.idx, st.in
	off, err := f.inodeOffset(in.num)
	if err != nil {
		return nil, nil, err
	}
	bs := int64(f.sb.blockSize)
	blk, within, isz := uint64(off/bs), int(off%bs), f.sb.inodeSize
	limit := cmp.Or(f.jrecCopyCap, maxJournalCopiesPerEntry)

	var (
		notes    []string
		otherGen int
		examined int
		found    *jcopy
	)
	for _, ti := range s.inodeCopies[blk] {
		g := &x.tags[ti]
		t := &x.txns[g.txn]
		if !s.allowed(t) || !eligibleCopy(g) || g.revoked {
			continue
		}
		if examined++; examined > limit {
			return nil, notes, unknownJournal(
				fmt.Errorf("%w: more than %d journal copies of the inode table block of inode %d", filesys.ErrLimit, limit, in.num),
				fmt.Sprintf("journal search stopped at %d copies of the inode table block of inode %d", limit, in.num))
		}
		f.mapWork.Add(1)
		block, err := x.readRaw(f, g.jblk)
		if errors.Is(err, errImageEnd) {
			return nil, notes, unknownJournal(
				fmt.Errorf("%w: a journal copy of the inode table block of inode %d lies beyond the end of the image", filesys.ErrIncomplete, in.num),
				"journal copy lies beyond the end of the image")
		}
		if err != nil {
			return nil, nil, err
		}
		if g.flags&tagEscape != 0 {
			binary.BigEndian.PutUint32(block, jbd2Magic) // the log keeps the magic zeroed (JBD2 escape)
		}
		if len(block) < within+isz {
			continue // not a block this inode can be in: the journal block size differs from the filesystem's
		}
		raw := block[within : within+isz]
		le := binary.LittleEndian
		switch {
		case le.Uint32(raw[iGeneration:]) != in.generation:
			otherGen++
			continue
		case le.Uint16(raw[iLinks:]) == 0 || le.Uint32(raw[iDtime:]) != 0:
			continue // a deleted-state copy: it holds no live map
		}
		found = &jcopy{tag: ti, raw: raw, escaped: g.flags&tagEscape != 0}
		break // the newest live-state copy of this generation
	}

	if found != nil {
		c, why, untrusted, err := f.journalCandidateFrom(s, st, found, blk, otherGen)
		if err != nil {
			return nil, notes, err
		}
		if why == "" {
			if x.info.Incomplete {
				c.Assumptions = append(c.Assumptions, "journal-scan-incomplete")
			}
			return []filesys.Candidate{c}, notes, nil
		}
		t := &x.txns[x.tags[found.tag].txn]
		notes = append(notes, fmt.Sprintf("journal copy of inode %d (sequence %d) not used: %s", in.num, t.seq, why))
		if untrusted && !x.info.Incomplete {
			// The only journal copy was found and rejected: that is unknown, never "not found" (3G C12). The reason is the
			// warning on any other candidate and the cause of the undecided answer when there is none.
			return nil, nil, unknownJournal(fmt.Errorf("%w: %s", filesys.ErrUndecided, notes[0]), notes[0])
		}
	}
	if x.info.Incomplete {
		return nil, notes, unknownJournal(
			fmt.Errorf("%w: the journal scan was incomplete (%s), so a copy of inode %d may be missing", filesys.ErrIncomplete, x.info.Stop, in.num),
			fmt.Sprintf("journal scan incomplete (%s): a copy of inode %d may be missing", x.info.Stop, in.num))
	}
	return nil, notes, nil
}

// jcopy is the live-state copy of an inode selected from the journal.
type jcopy struct {
	tag     uint32 // index into jindex.tags
	raw     []byte // the inode slot, unescaped
	escaped bool
}

// journalCandidateFrom checks the selected copy L and builds the candidate. A copy that cannot be a source gives a
// reason and no candidate; untrusted says the copy cannot be believed (it does not decode, fails its checksum or
// disagrees with the directory entry), which makes the entry undecided rather than not found (3G C12). The error is an
// I/O failure only.
func (f *FS) journalCandidateFrom(s *jrecState, st *staleInode, c *jcopy, itableBlk uint64, otherGen int) (cand filesys.Candidate, why string, untrusted bool, err error) {
	x := s.idx
	g := &x.tags[c.tag]
	t := &x.txns[g.txn]
	lin, lnotes, err := f.decodeInodeQuiet(st.in.num, c.raw)
	if err != nil {
		return filesys.Candidate{}, "the inode does not decode", true, nil
	}
	switch {
	case !lin.csumOK:
		return filesys.Candidate{}, "its inode checksum does not match", true, nil
	case f.sb.hasIncompat(incompatFiletype) && st.rec.rec.ftype != 0 && modeFiletype(lin.mode) != st.rec.rec.ftype:
		return filesys.Candidate{}, fmt.Sprintf("its file type (%#x) differs from the directory entry's (%d)", lin.mode&modeTypeMask, st.rec.rec.ftype), true, nil
	case lin.flags&inodeFlagEAInode != 0:
		return filesys.Candidate{}, "it is an EA inode", false, nil
	case lin.flags&inodeFlagInlineData != 0:
		return filesys.Candidate{}, "it holds inline data, which is in the inode table", false, nil
	case lin.mode&modeTypeMask != modeReg && lin.mode&modeTypeMask != modeSymlink:
		return filesys.Candidate{}, "it is neither a regular file nor a symlink", false, nil
	case lin.mode&modeTypeMask == modeSymlink && lin.flags&inodeFlagExtents == 0 && lin.size < fastSymlinkMax:
		return filesys.Candidate{}, "it is a fast symlink, whose target is in the inode table", false, nil
	case lin.size <= 0:
		return filesys.Candidate{}, "its size is zero", false, nil
	}
	lin.offset = st.in.offset
	lin.times.Deleted = st.in.times.Deleted // the copy predates the delete: the deletion time is the on-disk inode's
	m, err := f.mapStaleWith(lin, min(lin.size, f.size), s.source(g.txn))
	if err != nil {
		return filesys.Candidate{}, "", false, err
	}
	if m.cleared || (len(m.runs) == 0 && m.stop == "") {
		return filesys.Candidate{}, "its map holds nothing", false, nil
	}

	kind := "blockmap"
	if lin.flags&inodeFlagExtents != 0 {
		kind = "extents"
	}
	u64 := func(v uint64) string { return strconv.FormatUint(v, 10) }
	basis := []string{
		st.rec.idStr, "inode=" + u64(uint64(lin.num)), "generation=" + u64(uint64(lin.generation)),
		"journal:seq=" + u64(uint64(t.seq)), "journal:region=" + regionName(t.stale),
		"journal:jblk=" + u64(uint64(g.jblk)), "journal:fsblk=" + u64(itableBlk),
	}
	if otherGen > 0 {
		basis = append(basis, "journal-copies-other-generation="+strconv.Itoa(otherGen))
	}
	if c.escaped {
		basis = append(basis, "unescaped")
	}
	basis = append(basis, "map="+kind)
	for i, blk := range m.tree {
		if i == maxBasisTreeBlocks {
			basis = append(basis, "tree-blocks="+strconv.Itoa(len(m.tree)))
			break
		}
		basis = append(basis, "tree-block="+u64(blk)+"@"+m.treeFrom[i])
	}
	assumptions := []string{"inode-copy-from-journal", "journal-copy-may-predate-last-change", "size-from-journal-copy", "inode-not-reused-since-entry-deleted"}
	if t.stale {
		assumptions = append(assumptions, "journal-stale-region")
	}
	if g.copy == copyUnverified {
		assumptions = append(assumptions, "journal-copy-unverified")
	}
	if c.escaped {
		assumptions = append(assumptions, "jbd2-unescaped")
	}
	cand = f.candidateFrom(&staleInode{in: lin, raw: c.raw, rec: st.rec, notes: lnotes}, MethodJournalInode, m, lin.size, basis, assumptions)
	cand.Journal = &filesys.CandidateJournal{Seq: t.seq, Committed: true, Region: regionName(t.stale), Revoked: false}
	return cand, "", false, nil
}

// source returns the tree source for an inode copy taken from transaction t.
func (s *jrecState) source(t uint32) *journalTreeSource {
	return &journalTreeSource{x: s.idx, t: t, use: s.use, byBlock: s.byBlock}
}

// journalTreeSource reads the map blocks of an inode copy from the journal (3G-R12): block B from the newest eligible
// copy (revoked allowed) whose transaction is not newer than the inode copy's, T. With no such copy, B is read at home
// when no committed copy of it is newer than T, and otherwise the map stops: the home block may hold a later state.
type journalTreeSource struct {
	x       *jindex
	t       uint32 // the transaction of the inode copy
	use     filesys.JournalUse
	byBlock []uint32
}

func (s *journalTreeSource) mapBlock(f *FS, blk uint64) ([]byte, string, bool, string, error) {
	x := s.x
	at := x.txns[s.t].seq
	lo := sort.Search(len(s.byBlock), func(i int) bool { return x.tags[s.byBlock[i]].fsblk >= blk })
	newer := false
	for i := lo; i < len(s.byBlock) && x.tags[s.byBlock[i]].fsblk == blk; i++ {
		f.mapWork.Add(1)
		g := &x.tags[s.byBlock[i]]
		t := &x.txns[g.txn]
		switch {
		case !t.committed:
			continue
		case seqNewer(t.seq, at):
			newer = true
			continue
		case !journalAllowed(s.use, t):
			continue
		case !eligibleCopy(g):
			// A committed copy at or before T whose bytes are mismatch or missing: B's state at T is unknown (3G C11).
			// An older copy or the home block would be a guess, so the map stops here with the same token as a later copy.
			return nil, "", false, stopMapBlockNewer, nil
		}
		buf, err := x.readRaw(f, g.jblk)
		if errors.Is(err, errImageEnd) {
			return nil, "", false, stopJournalCopyUnreadable, nil
		}
		if err != nil {
			return nil, "", false, "", fmt.Errorf("ext4 journal copy of map block %d: %w", blk, err)
		}
		if len(buf) != f.sb.blockSize {
			return nil, "", false, stopMapDamaged, nil
		}
		if g.flags&tagEscape != 0 {
			binary.BigEndian.PutUint32(buf, jbd2Magic)
		}
		from := "journal:" + strconv.FormatUint(uint64(t.seq), 10)
		if g.revoked {
			from += ":revoked"
		}
		return buf, from, false, "", nil
	}
	if newer {
		return nil, "", false, stopMapBlockNewer, nil
	}
	return nil, "home", true, "", nil
}
