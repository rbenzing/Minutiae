package ext4

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/rbenzing/minutiae/internal/filesys"
)

var _ filesys.Journaler = (*FS)(nil)

// jindex is the journal as located and decoded once per FS: the superblock, the block map, and the transactions of the
// live region and the stale fragments.
type jindex struct {
	sb       *jsuper
	jm       *journalMap
	txns     []jtxn
	tags     []jtag
	revokes  []uint64
	consumed []uint64 // bitmap over journal blocks: the blocks the scans have consumed
	last     uint32   // the log wraps to s_first at this block (s_maxlen less the fast-commit area)
	liveStop string   // why the live scan ended: end-of-log@<b>, descriptor-checksum@<b>, ... "" when s_start is 0
	capped   bool     // a scan cap was reached
	byStart  []uint32 // transaction indexes ordered by start block

	// peek is the last block the scans read (see readBlock); imageEnds counts the reads past the end of the image.
	endOff    int64 // the first offset a read ran past the end of the image at; -1 = none
	peek      []byte
	peekJ     uint32
	peekOK    bool
	keep      []byte // the block the live scan stopped at: the stale pass starts at the first block, so it would be evicted
	keepJ     uint32
	keepOK    bool
	imageEnds int
	info      filesys.JournalInfo
}

func (x *jindex) isConsumed(j uint32) bool {
	return j < x.sb.maxLen && x.consumed[j/64]&(1<<(j%64)) != 0
}

func (x *jindex) consume(j uint32) { x.consumed[j/64] |= 1 << (j % 64) }

// journalIndex returns the journal, locating and scanning it on first use. A corrupt, unsupported, over-limit or absent
// journal is cached; an I/O error is returned and not cached, so a later call tries again.
func (f *FS) journalIndex() (*jindex, error) {
	f.jrnMu.Lock()
	defer f.jrnMu.Unlock()
	if f.jrn != nil {
		return f.jrn, nil
	}
	if f.jrnErr != nil {
		return nil, f.jrnErr
	}
	sb, jm, err := f.openJournal()
	if err == nil {
		var x *jindex
		if x, err = f.buildIndex(sb, jm); err == nil {
			f.jrn = x
			return x, nil
		}
	}
	if errors.Is(err, filesys.ErrNoJournal) || errors.Is(err, filesys.ErrUnsupported) ||
		errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrCorrupt) {
		f.jrnErr = err
	}
	return nil, err
}

// buildIndex scans the live region (3G-R3). The caller holds jrnMu.
func (f *FS) buildIndex(sb *jsuper, jm *journalMap) (*jindex, error) {
	x := &jindex{
		sb: sb, jm: jm, endOff: -1,
		consumed: make([]uint64, (uint64(sb.maxLen)+63)/64), // maxLen is bounded by the inode's mapped blocks and the byte cap
		last:     sb.maxLen - sb.fcBlocks(),
	}
	x.info = filesys.JournalInfo{
		Type:          "jbd2",
		Version:       sb.version,
		BlockSize:     int(sb.blockSize),
		Blocks:        int64(sb.maxLen),
		First:         sb.first,
		Sequence:      sb.sequence,
		Start:         sb.start,
		Features:      sb.featureNames(),
		NeedsRecovery: f.sb.hasIncompat(incompatRecover),
		Warnings:      slices.Clone(sb.warnings),
	}
	if sb.start != 0 && !x.info.NeedsRecovery {
		x.info.Warnings = append(x.info.Warnings, "the journal has a live region but the filesystem is not marked needs_recovery")
	}
	if fc := sb.fcBlocks(); fc != 0 {
		// Unknown is never absent: a replay would apply these blocks.
		x.info.Incomplete, x.info.Stop = true, "fast-commit-not-analysed"
		x.info.Warnings = append(x.info.Warnings, fmt.Sprintf("the fast-commit area (%d blocks at the end of the journal) is not decoded; a replay would apply it", fc))
	}
	if err := x.scanLive(f); err != nil {
		return nil, err
	}
	x.keepJ, x.keep, x.keepOK = x.peekJ, x.peek, x.peekOK
	if err := x.scanStale(f); err != nil {
		return nil, err
	}
	x.resolveRevokes()
	x.sortByStart()
	x.peek, x.peekOK, x.keep, x.keepOK = nil, false, nil, false
	return x, nil
}

// Journal describes the jbd2 journal (3G-R2): its superblock, features and the findings that do not stop a read, as
// warnings of the result (never of Info). A filesystem without has_journal is filesys.ErrNoJournal. Nothing is
// replayed or written.
func (f *FS) Journal() (filesys.JournalInfo, error) {
	x, err := f.journalIndex()
	if err != nil {
		return filesys.JournalInfo{}, err
	}
	info := x.info
	info.Features = slices.Clone(info.Features)
	info.Warnings = slices.Clone(info.Warnings)
	return info, nil
}

// JournalTransactions visits the journal's transactions (3G-R8): the live ones in log order, then the stale fragments
// by start block. Every call builds fresh slices; the walk stops when visit returns false. The index is built once;
// nothing is written and no filesystem warning is added.
func (f *FS) JournalTransactions(visit func(filesys.JournalTxn) bool) error {
	x, err := f.journalIndex()
	if err != nil {
		return err
	}
	for i := range x.txns {
		if !visit(x.journalTxn(i)) {
			return nil
		}
	}
	return nil
}

// journalTxn converts transaction i of the index into its public form.
func (x *jindex) journalTxn(i int) filesys.JournalTxn {
	t := &x.txns[i]
	out := filesys.JournalTxn{
		Seq: t.seq, Committed: t.committed, Region: regionName(t.stale),
		Revoked:    slices.Clone(x.revokes[t.revokes[0]:t.revokes[1]]),
		Warnings:   slices.Clone(t.warnings),
		StartBlock: t.start, CommitBlock: t.commit, CommitTime: t.commitTime, Checksum: t.csum,
	}
	if tags := x.tags[t.tags[0]:t.tags[1]]; len(tags) > 0 {
		out.Blocks = make([]filesys.JournalTag, len(tags))
		for k, g := range tags {
			out.Blocks[k] = filesys.JournalTag{
				FSBlock: g.fsblk, Flags: g.flags, Revoked: g.revoked,
				JournalBlock: uint64(g.jblk), Copy: g.copy.String(), Escaped: g.flags&tagEscape != 0,
			}
		}
	}
	return out
}

// find returns the index of the transaction that starts at journal block start, or -1.
func (x *jindex) find(start int64) int {
	k := sort.Search(len(x.byStart), func(k int) bool { return x.txns[x.byStart[k]].start >= start })
	if k < len(x.byStart) && x.txns[x.byStart[k]].start == start {
		return int(x.byStart[k])
	}
	return -1
}

// JournalBlock returns the logged copy of tag i of t, exactly as logged (an escaped block keeps its zeroed first four
// bytes), and its filesystem-relative run (3G-R8). t is never trusted: its Seq, Region, StartBlock and tag must equal
// a transaction and tag of the index, and the tag must have a copy (not missing or outside); otherwise the result is
// filesys.ErrNotFound and nothing is read.
func (f *FS) JournalBlock(t filesys.JournalTxn, i int) ([]byte, filesys.Run, error) {
	x, err := f.journalIndex()
	if err != nil {
		return nil, filesys.Run{}, err
	}
	notFound := func(why string) ([]byte, filesys.Run, error) {
		return nil, filesys.Run{}, fmt.Errorf("ext4: journal block: %s: %w", why, filesys.ErrNotFound)
	}
	k := x.find(t.StartBlock)
	if k < 0 {
		return notFound("no transaction starts at that journal block")
	}
	it := &x.txns[k]
	if it.seq != t.Seq || regionName(it.stale) != t.Region {
		return notFound("the transaction is not the one in the journal")
	}
	tags := x.tags[it.tags[0]:it.tags[1]]
	if i < 0 || i >= len(tags) || i >= len(t.Blocks) {
		return notFound("the tag index is out of range")
	}
	g := tags[i]
	if tb := t.Blocks[i]; tb.FSBlock != g.fsblk || tb.JournalBlock != uint64(g.jblk) {
		return notFound("the tag is not the one in the journal")
	}
	if g.copy == copyMissing || g.copy == copyOutside {
		return notFound("the tag has no usable copy (" + g.copy.String() + ")")
	}
	data, err := x.readRaw(f, g.jblk)
	if errors.Is(err, errImageEnd) {
		return nil, filesys.Run{}, corrupt("ext4 journal", -1, "the image ends inside journal block %d", g.jblk)
	}
	if err != nil {
		return nil, filesys.Run{}, err
	}
	bs := int64(x.sb.blockSize)
	fsb, _ := x.jm.fsBlock(g.jblk, bs) // readRaw succeeded, so the block is mapped
	return data, filesys.Run{Offset: int64(fsb) * bs, Length: bs}, nil
}
