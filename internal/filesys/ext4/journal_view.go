package ext4

import (
	"errors"
	"fmt"
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

var _ filesys.Journaler = (*FS)(nil)

// jindex is the journal as located and decoded once per FS (the transaction index is added by Task 5).
type jindex struct {
	sb *jsuper
	jm *journalMap
}

// journalIndex returns the journal, locating it on first use. A corrupt, unsupported, over-limit or absent journal is
// cached; an I/O error is returned and not cached, so a later call tries again.
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
	if err != nil {
		if errors.Is(err, filesys.ErrNoJournal) || errors.Is(err, filesys.ErrUnsupported) ||
			errors.Is(err, filesys.ErrLimit) || errors.Is(err, filesys.ErrCorrupt) {
			f.jrnErr = err
		}
		return nil, err
	}
	f.jrn = &jindex{sb: sb, jm: jm}
	return f.jrn, nil
}

// Journal describes the jbd2 journal (3G-R2): its superblock, features and the findings that do not stop a read, as
// warnings of the result (never of Info). A filesystem without has_journal is filesys.ErrNoJournal. Nothing is
// replayed or written.
func (f *FS) Journal() (filesys.JournalInfo, error) {
	ix, err := f.journalIndex()
	if err != nil {
		return filesys.JournalInfo{}, err
	}
	s := ix.sb
	info := filesys.JournalInfo{
		Type:          "jbd2",
		Version:       s.version,
		BlockSize:     int(s.blockSize),
		Blocks:        int64(s.maxLen),
		Sequence:      s.sequence,
		Start:         s.start,
		Features:      s.featureNames(),
		NeedsRecovery: f.sb.hasIncompat(incompatRecover),
		Warnings:      slices.Clone(s.warnings),
	}
	if s.start != 0 && !info.NeedsRecovery {
		info.Warnings = append(info.Warnings, "the journal has a live region but the filesystem is not marked needs_recovery")
	}
	if fc := s.fcBlocks(); fc != 0 {
		// Unknown is never absent: a replay would apply these blocks.
		info.Incomplete, info.Stop = true, "fast-commit-not-analysed"
		info.Warnings = append(info.Warnings, fmt.Sprintf("the fast-commit area (%d blocks at the end of the journal) is not decoded; a replay would apply it", fc))
	}
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
