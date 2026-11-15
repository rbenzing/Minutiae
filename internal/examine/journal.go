package examine

import (
	"errors"
	"fmt"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// ErrJournalNotAvailable is returned (wrapped, naming the filesystem type) when the filesystem of the
// partition has no journal analysis in this build. It never says the filesystem has no journal: the absence
// of an analysis is not the absence of a journal (unknown is never absent).
var ErrJournalNotAvailable = fmt.Errorf("%w: journal analysis is not available for this filesystem in this build", filesys.ErrUnsupported)

// JournalOptions selects the partition (-1 = the only one, as Session.Partition).
type JournalOptions struct{ Partition int }

// JournalListing is what a filesystem reports of its journal. It is read-only: no audit entry, no write.
type JournalListing struct {
	FSType string
	Info   filesys.JournalInfo
	Txns   []filesys.JournalTxn
	Chain  *filesys.RollForwardChain // nil when the filesystem is no RollForwarder
}

func notAvailable(fsys filesys.FileSystem) error {
	return fmt.Errorf("%w (filesystem type %q)", ErrJournalNotAvailable, fsys.Info().Type)
}

// JournalList lists the journal of the partition's filesystem: its description, its transactions and, for a
// roll-forward filesystem, the chain summary. Read-only.
func (s *Session) JournalList(o JournalOptions) (JournalListing, error) {
	fsys, _, err := s.FS(o.Partition)
	if err != nil {
		return JournalListing{}, err
	}
	j, ok := filesys.As[filesys.Journaler](fsys)
	if !ok {
		return JournalListing{}, notAvailable(fsys)
	}
	l := JournalListing{FSType: fsys.Info().Type}
	if l.Info, err = j.Journal(); err != nil {
		if errors.Is(err, filesys.ErrNoJournal) {
			return JournalListing{}, notAvailable(fsys)
		}
		return JournalListing{}, err
	}
	if err = j.JournalTransactions(func(t filesys.JournalTxn) bool { l.Txns = append(l.Txns, t); return true }); err != nil {
		return JournalListing{}, err
	}
	if rf, err := s.rollForwarderOf(fsys); err == nil {
		chain, err := rf.RollForward(func(filesys.RollForwardNode) bool { return false })
		if err != nil {
			return JournalListing{}, err
		}
		l.Chain = &chain
	}
	return l, nil
}

// rollForwarderForJournal is the RollForwarder of the partition, or ErrJournalNotAvailable naming the type.
func (s *Session) rollForwarderForJournal(o JournalOptions) (filesys.FileSystem, volume.Partition, filesys.RollForwarder, error) {
	fsys, part, err := s.FS(o.Partition)
	if err != nil {
		return nil, part, nil, err
	}
	rf, err := s.rollForwarderOf(fsys)
	if err != nil {
		return nil, part, nil, notAvailable(fsys)
	}
	return fsys, part, rf, nil
}

// chainHasSeq reports whether seq names the chain's one transaction.
func chainHasSeq(chain filesys.RollForwardChain, seq uint32) bool {
	return chain.Nodes > 0 && seq == uint32(chain.CheckpointVersion)
}

// errNoSuchSeq is the error for a seq the chain does not hold: ErrNotFound when the chain was read to its end,
// else ErrIncomplete naming the stop, because the nodes after it are unknown (never "absent").
func errNoSuchSeq(chain filesys.RollForwardChain, seq uint32) error {
	if !chain.Complete {
		return fmt.Errorf("transaction %d: not among the nodes read; the chain stopped at %s with later nodes unknown: %w", seq, chain.Stop, filesys.ErrIncomplete)
	}
	return fmt.Errorf("transaction %d is not a transaction of the roll-forward log: %w", seq, filesys.ErrNotFound)
}

// JournalShow visits the nodes of transaction seq (the chain; its sequence is the checkpoint version's low
// 32 bits). Another seq is ErrNotFound and nothing is visited. Read-only.
func (s *Session) JournalShow(o JournalOptions, seq uint32, visit func(filesys.RollForwardNode) bool) (filesys.RollForwardChain, error) {
	_, _, rf, err := s.rollForwarderForJournal(o)
	if err != nil {
		return filesys.RollForwardChain{}, err
	}
	chain, err := rf.RollForward(func(filesys.RollForwardNode) bool { return false })
	if err != nil {
		return chain, err
	}
	if !chainHasSeq(chain, seq) {
		return filesys.RollForwardChain{}, errNoSuchSeq(chain, seq)
	}
	return rf.RollForward(visit)
}
