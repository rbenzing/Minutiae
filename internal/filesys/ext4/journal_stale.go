package ext4

import (
	"encoding/binary"
	"errors"
	"slices"
	"sort"
)

// scanStale is the one sequential pass over the journal blocks outside the live region (3G-R5): every unconsumed
// header block starts a fragment that follows that header's own sequence. The live region and the blocks an earlier
// fragment consumed are walls. Blocks are visited in position order, so fragments come out ordered by their first
// block, and each block is read once.
func (x *jindex) scanStale(f *FS) error {
	if x.capped {
		return nil // a cap already made the index incomplete; more fragments would hit it again
	}
	for p := x.sb.first; p < x.last; p++ {
		if x.isConsumed(p) {
			continue
		}
		blk, err := x.readBlock(f, p)
		if errors.Is(err, errImageEnd) {
			x.imageEnd(p)
			continue
		}
		if err != nil {
			return err
		}
		if !isHeader(blk) {
			continue
		}
		if _, _, err := x.scanTxn(f, p, binary.BigEndian.Uint32(blk[8:]), true); err != nil {
			return err
		}
		if x.capped {
			return nil
		}
	}
	if x.imageEnds > 1 {
		x.warn("%d journal blocks in all lie beyond the end of the image", x.imageEnds)
	}
	return nil
}

// revokeRec is one committed revoke record: block blk was revoked by a transaction of sequence seq.
type revokeRec struct {
	blk uint64
	seq uint32
}

// resolveRevokes marks the tags a later revoke covers (3G-R6, the kernel's jbd2_journal_test_revoke from memory): a tag
// of block B in transaction X is revoked iff a COMMITTED transaction R, found in either region, with seqGE(R, X) holds
// a revoke record for B. The records of uncommitted transactions do not count. Sequence numbers wrap, so seqGE is not
// a total order; the test is whether the circular successor of X among the records of B is within half a lap.
func (x *jindex) resolveRevokes() {
	var recs []revokeRec
	for i := range x.txns {
		t := &x.txns[i]
		if !t.committed {
			continue
		}
		for _, b := range x.revokes[t.revokes[0]:t.revokes[1]] {
			recs = append(recs, revokeRec{b, t.seq})
		}
	}
	if len(recs) == 0 {
		return
	}
	slices.SortFunc(recs, func(a, b revokeRec) int {
		switch {
		case a.blk != b.blk:
			if a.blk < b.blk {
				return -1
			}
			return 1
		case a.seq != b.seq:
			if a.seq < b.seq {
				return -1
			}
			return 1
		}
		return 0
	})
	for i := range x.tags {
		g := &x.tags[i]
		x2 := x.txns[g.txn].seq
		lo := sort.Search(len(recs), func(k int) bool { return recs[k].blk >= g.fsblk })
		hi := lo + sort.Search(len(recs)-lo, func(k int) bool { return recs[lo+k].blk > g.fsblk })
		if lo == hi {
			continue
		}
		grp := recs[lo:hi]
		// The smallest sequence >= X, else (circularly) the smallest of all: it has the least distance from X.
		k := sort.Search(len(grp), func(k int) bool { return grp[k].seq >= x2 })
		if k == len(grp) {
			k = 0
		}
		g.revoked = seqGE(grp[k].seq, x2)
	}
}

// sortByStart builds the lookup JournalBlock re-resolves a transaction with: the transaction indexes ordered by start
// block, which no two transactions share.
func (x *jindex) sortByStart() {
	x.byStart = make([]uint32, len(x.txns))
	for i := range x.byStart {
		x.byStart[i] = uint32(i)
	}
	slices.SortFunc(x.byStart, func(a, b uint32) int {
		sa, sb := x.txns[a].start, x.txns[b].start
		switch {
		case sa < sb:
			return -1
		case sa > sb:
			return 1
		}
		return 0
	})
}
