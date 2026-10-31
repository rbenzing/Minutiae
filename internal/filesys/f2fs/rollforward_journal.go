package f2fs

import (
	"slices"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// The Journaler view of the chain (3H-R9). The "journal" is the roll-forward chain; its one
// transaction is the whole chain. Region "live" means "the log region the kernel's recovery
// scans" and never that the data is live. Blocks are filesystem-relative (examine adds the
// partition start).

// incompleteWarning is the warning of a chain whose end is unknown: later nodes may exist,
// so the need for recovery cannot be ruled out.
func (c *rfChain) incompleteWarning() string {
	return "roll-forward chain is incomplete (stop=" + c.sum.Stop + "): later nodes are unknown, so recovery need cannot be ruled out"
}

// replays reports whether the kernel would replay any node of the chain.
func (c *rfChain) replays() bool {
	for i := range c.entries {
		if c.wouldReplay(i) {
			return true
		}
	}
	return false
}

// txn is the one transaction of the chain, built from the index (a 16-byte tag per node:
// bounded by maxChainNodes).
func (c *rfChain) txn() filesys.JournalTxn {
	tx := filesys.JournalTxn{Seq: uint32(c.sum.CheckpointVersion), Region: "live", Committed: c.replays(), Blocks: make([]filesys.JournalTag, len(c.entries))}
	for i, e := range c.entries {
		tx.Blocks[i] = filesys.JournalTag{FSBlock: uint64(e.block), Flags: e.flag}
	}
	if !c.sum.Complete {
		tx.Warnings = []string{c.incompleteWarning()}
	}
	return tx
}

// Journal describes the chain as a journal.
func (f *FS) Journal() (filesys.JournalInfo, error) {
	c, err := f.chain()
	if err != nil {
		return filesys.JournalInfo{}, err
	}
	j := filesys.JournalInfo{
		Type: "f2fs-rollforward", BlockSize: blockSize, Blocks: int64(len(c.entries)),
		Sequence: uint32(c.sum.CheckpointVersion), Start: uint32(c.sum.Start),
		Features: []string{}, NeedsRecovery: c.replays() || !c.sum.Complete, Warnings: slices.Clone(c.sum.Warnings),
	}
	if !c.sum.Complete {
		j.Warnings = append(j.Warnings, c.incompleteWarning())
	}
	switch {
	case c.sum.NoCRCRecovery:
		j.Features = append(j.Features, "nocrc-recovery")
	case c.sum.CRCRecovery:
		j.Features = append(j.Features, "crc-recovery")
	}
	return j, nil
}

// JournalTransactions visits the chain's single transaction; an empty chain has none.
func (f *FS) JournalTransactions(visit func(filesys.JournalTxn) bool) error {
	c, err := f.chain()
	if err != nil {
		return err
	}
	if len(c.entries) > 0 {
		visit(c.txn())
	}
	return nil
}

// JournalBlock returns block i of t. t is never trusted: it must equal the chain's own
// transaction at tag i (same sequence, same block and flags), else ErrNotFound with no read.
func (f *FS) JournalBlock(t filesys.JournalTxn, i int) ([]byte, filesys.Run, error) {
	c, err := f.chain()
	if err != nil {
		return nil, filesys.Run{}, err
	}
	if i < 0 || i >= len(c.entries) || i >= len(t.Blocks) || t.Seq != uint32(c.sum.CheckpointVersion) {
		return nil, filesys.Run{}, filesys.ErrNotFound
	}
	e := c.entries[i]
	if t.Blocks[i] != (filesys.JournalTag{FSBlock: uint64(e.block), Flags: e.flag}) {
		return nil, filesys.Run{}, filesys.ErrNotFound
	}
	b, err := f.rfBlock(e.block)
	if err != nil {
		return nil, filesys.Run{}, err
	}
	return b, filesys.Run{Offset: int64(e.block) * blockSize, Length: blockSize}, nil
}
