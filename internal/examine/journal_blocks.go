package examine

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// rfBlockSize is the size of an F2FS block, the unit of a roll-forward block copy.
const rfBlockSize = 4096

// JournalBlocksOptions selects the chain blocks to copy.
type JournalBlocksOptions struct {
	JournalOptions
	Seq      uint32  // the transaction: the chain's sequence (the checkpoint version's low 32 bits)
	FSBlock  *uint64 // nil = every chain block; else only the node at this filesystem block
	MaxFiles int     // 0 = DefaultMaxFiles
	MaxBytes int64   // 0 = DefaultMaxBytes
}

// JournalBlocksSummary is the outcome of JournalBlocks.
type JournalBlocksSummary struct {
	Summary
	Copied, NotFree int
}

// rfBlockSel is one chain node selected for a block copy.
type rfBlockSel struct {
	index       int
	block       uint64
	nid, ino    uint32
	wouldReplay bool
}

// JournalBlocks copies the node blocks of the roll-forward chain (journal-block artifacts, method
// f2fs-rollforward). A block is copied only when its whole image range is free in the checkpoint view: one that
// is not is skipped as "not-free" (warned and counted), never copied. A seq that is not the chain's, or an
// FSBlock that is not a chain node, is ErrNotFound before anything is audited.
func (s *Session) JournalBlocks(ctx context.Context, o JournalBlocksOptions) (JournalBlocksSummary, error) {
	b, err := newBudget(o.MaxFiles, o.MaxBytes)
	if err != nil {
		return JournalBlocksSummary{}, err
	}
	fsys, part, rf, err := s.rollForwarderForJournal(o.JournalOptions)
	if err != nil {
		return JournalBlocksSummary{}, err
	}
	var sel []rfBlockSel
	chain, err := rf.RollForward(func(n filesys.RollForwardNode) bool {
		if o.FSBlock == nil || *o.FSBlock == n.Block {
			sel = append(sel, rfBlockSel{index: n.Index, block: n.Block, nid: n.NID, ino: n.Ino, wouldReplay: n.WouldReplay})
		}
		return true
	})
	if err != nil {
		return JournalBlocksSummary{}, err
	}
	if !chainHasSeq(chain, o.Seq) {
		return JournalBlocksSummary{}, errNoSuchSeq(o.Seq)
	}
	if len(sel) == 0 {
		return JournalBlocksSummary{}, fmt.Errorf("filesystem block %d is not a node of the roll-forward log: %w", *o.FSBlock, filesys.ErrNotFound)
	}
	fsType := fsys.Info().Type
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType, "format": evidence.AlgorithmRecover,
		"seq": o.Seq, "max_files": b.maxFiles, "max_bytes": b.maxBytes,
	}
	if o.FSBlock != nil {
		details["fsblock"] = *o.FSBlock
	}
	var js JournalBlocksSummary
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "journal-blocks", details, func(a *analysis) error {
		defer func() {
			a.setEnd("selected", len(sel))
			a.setEnd("copied", js.Copied)
			a.setEnd("not_free", js.NotFree)
			a.setEnd("chain_stop", chain.Stop)
			a.setEnd("chain_complete", chain.Complete)
		}()
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		runs, _, err := s.unallocatedRunsNote(fsys, part, false)
		if err != nil {
			return fmt.Errorf("the unallocated space of partition %d cannot be listed: %w", part.Index, err)
		}
		// the free map is built exactly as the recovery plan and the allocation check build it
		free := newFreeMap(runs, len(fsys.Info().Warnings) > 0)
		if err := a.syncFS(); err != nil {
			return err
		}
		if err := s.checkSpace(int64(len(sel)) * rfBlockSize); err != nil {
			return err
		}
		for _, n := range sel {
			if err := ctx.Err(); err != nil {
				return err
			}
			label := fmt.Sprintf("journal block %d", n.block)
			off, ok := blockOffset(part, n.block)
			if !ok || !free.contains(off, rfBlockSize) {
				js.NotFree++
				if err := a.warn(label, "not-free: the block is not free in the checkpoint view, so it is not copied"); err != nil {
					return err
				}
				continue
			}
			if reason := b.admit(rfBlockSize); reason != "" {
				a.sum.OutputLimit = reason
				return a.warn(label, "not copied: output limit "+reason+" reached")
			}
			if err := s.copyJournalBlock(ctx, a, part, fsType, o.Seq, n, off, &js); err != nil {
				return err
			}
			if err := a.syncFS(); err != nil {
				return err
			}
		}
		return nil
	})
	js.Summary = sum
	return js, err
}

// blockOffset is the image offset of filesystem block blk of part, false when it does not fit an int64.
func blockOffset(part volume.Partition, blk uint64) (int64, bool) {
	if blk > uint64(1<<62)/rfBlockSize {
		return 0, false
	}
	return filesys.AddOK(part.Start, int64(blk)*rfBlockSize)
}

// copyJournalBlock writes one chain block as a journal-block artifact. A read failure keeps the prefix copied
// and flags the artifact incomplete; a failure of the case itself is returned.
func (s *Session) copyJournalBlock(ctx context.Context, a *analysis, part volume.Partition, fsType string, seq uint32, n rfBlockSel, off int64, js *JournalBlocksSummary) error {
	run := evidence.Run{Offset: off, Length: rfBlockSize}
	d := s.baseDerivation(part, fsType)
	d.Runs = []evidence.Run{run}
	rv := &evidence.Recovery{
		Class: evidence.ClassJournalBlock, Method: evidence.RollForwardMethod, Scope: "unallocated",
		Alloc: evidence.AllocSummary{Free: rfBlockSize},
		Basis: []string{
			fmt.Sprintf("rollforward:seq=%d", seq), fmt.Sprintf("chain-index=%d", n.index),
			fmt.Sprintf("nid=%d", n.nid), fmt.Sprintf("ino=%d", n.ino), fmt.Sprintf("blk=%d", n.block),
		},
		Journal:   &evidence.JournalRef{Seq: seq, Committed: n.wouldReplay, Region: "live"},
		Algorithm: evidence.AlgorithmRecover,
	}
	d.Recovery = rv
	src := evidence.Source{Kind: evidence.KindJournal, DeviceID: s.Parent.Source.DeviceID, Derived: d}
	// Nothing that case verify would reject is ever written: a failure here is a defect of this build.
	probe := evidence.ManifestRecord{Path: "journal/probe", Size: rfBlockSize, Source: src}
	if probs := append(rv.Check(evidence.KindJournal), evidence.CheckRecoveredRuns(probe, d.Runs)...); len(probs) > 0 {
		return fmt.Errorf("internal error: the block copy of %d would not pass verification: %s", n.block, strings.Join(probs, "; "))
	}
	rel := fmt.Sprintf("journal/p%d-%s/seq%d-fsblk%d.bin", part.Index, fsType, seq, n.block)
	var copied int64
	rec, fillErr, err := s.capture(a, rel, src, func(w io.Writer) error {
		var err error
		copied, err = io.Copy(w, &ctxReader{ctx: ctx, r: io.NewSectionReader(s.Image, run.Offset, run.Length)})
		if err == nil && copied != run.Length {
			err = fmt.Errorf("image ended after %d of %d bytes: %w", copied, run.Length, io.ErrUnexpectedEOF)
		}
		if err != nil { // the runs of the bytes that were captured
			rv.Alloc.Free = copied
			rv.Assumptions = append(rv.Assumptions, fmt.Sprintf("read-failed-after=%d", copied))
			d.Runs = nil
			if copied > 0 {
				d.Runs = []evidence.Run{{Offset: run.Offset, Length: copied}}
			}
		}
		return err
	})
	if err != nil {
		return err // the case itself failed (or the name was taken): never downgraded
	}
	if fillErr == nil {
		js.Copied++
		a.sum.Files++
		a.sum.Bytes += rec.Size
		return nil
	}
	if ctx.Err() != nil || !skippable(fillErr) {
		return fillErr
	}
	return a.warn(fmt.Sprintf("journal block %d", n.block), fmt.Sprintf("read failed after %d of %d bytes (partial artifact kept, flagged incomplete): %v", copied, run.Length, fillErr))
}
