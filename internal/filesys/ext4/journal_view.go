package ext4

import (
	"errors"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

var _ filesys.Journaler = (*FS)(nil)

// jindex is the journal as located and decoded once per FS: the superblock, the block map, and the transactions of the
// live region (Task 6 adds the stale fragments).
type jindex struct {
	sb       *jsuper
	jm       *journalMap
	txns     []jtxn
	tags     []jtag
	revokes  []uint64
	consumed []uint64 // bitmap over journal blocks: the blocks the scans have consumed
	last     uint32   // the log wraps to s_first at this block (s_maxlen less the fast-commit area)
	liveStop string   // why the live scan ended: end-of-log@<b>, descriptor-checksum@<b>, ... "" when s_start is 0
	info     filesys.JournalInfo
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
		sb: sb, jm: jm,
		consumed: make([]uint64, (uint64(sb.maxLen)+63)/64), // maxLen is bounded by the inode's mapped blocks and the byte cap
		last:     sb.maxLen - sb.fcBlocks(),
	}
	x.info = filesys.JournalInfo{
		Type:          "jbd2",
		Version:       sb.version,
		BlockSize:     int(sb.blockSize),
		Blocks:        int64(sb.maxLen),
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

// JournalTransactions is not implemented until Task 6; it says so rather than reporting no transactions.
func (f *FS) JournalTransactions(_ func(filesys.JournalTxn) bool) error {
	return fmt.Errorf("ext4: journal transactions are not read yet: %w", filesys.ErrUnsupported)
}

// JournalBlock is not implemented until Task 6.
func (f *FS) JournalBlock(_ filesys.JournalTxn, _ int) ([]byte, filesys.Run, error) {
	return nil, filesys.Run{}, fmt.Errorf("ext4: journal blocks are not read yet: %w", filesys.ErrUnsupported)
}
