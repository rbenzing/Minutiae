package examine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

// The ext4 side of `image journal` (3G Task 10): the transaction view, the deterministic report and the exact copies of
// logged blocks. Nothing here replays anything: a transaction is history, and whether it was ever applied to the
// filesystem is unknown (spec 9.1), which every line of the report says.

// JBD2ReportFormat names the line format of the ext4 journal report (3G-R16).
const JBD2ReportFormat = "minutiae-ext4-journal/1"

// jbd2Caution follows the journal line of every report.
const jbd2Caution = "journal transactions are history: they were not replayed, and whether each was applied to the filesystem is unknown"

// jbd2StaleOffNote is the journal line's warning of a --no-stale report: a stale count of 0 must not read as "none".
const jbd2StaleOffNote = "stale transactions are not listed (--no-stale): stale_txns is 0 because they were left out, not because there are none"

// jbd2 tag flags (journal_block_tag_t.t_flags, from memory of include/linux/jbd2.h; the reader's Task 1 probe
// confirmed them for what the kernels write).
const (
	jbd2TagSameUUID = 0x2
	jbd2TagDeleted  = 0x4
	jbd2TagLast     = 0x8
)

// journalOf resolves the partition's Journaler and its description. ErrJournalNotAvailable names a filesystem this
// build cannot analyse; a reader that says it keeps no journal gives filesys.ErrNoJournal.
func (s *Session) journalOf(o JournalOptions) (filesys.FileSystem, volume.Partition, filesys.Journaler, filesys.JournalInfo, error) {
	fsys, part, err := s.FS(o.Partition)
	if err != nil {
		return nil, part, nil, filesys.JournalInfo{}, err
	}
	j, err := journalerOf(fsys)
	if err != nil {
		return nil, part, nil, filesys.JournalInfo{}, err
	}
	info, err := j.Journal()
	if err != nil {
		return nil, part, nil, filesys.JournalInfo{}, journalInfoErr(fsys, err)
	}
	return fsys, part, j, info, nil
}

// JournalTxns returns the transactions named seq, optionally of one region ("live", "stale", or "" for either).
// Read-only: no audit entry. None is ErrNotFound, except that a journal that is incomplete cannot say a transaction
// is absent: the transactions after its stop are unknown, so that is ErrIncomplete naming the stop. More than one is
// filesys.ErrAmbiguous naming the start blocks; the matches are returned with it so a caller can show them.
// JournalOptions.NoStale leaves the stale transactions out of the search.
func (s *Session) JournalTxns(o JournalOptions, seq uint32, region string) ([]filesys.JournalTxn, error) {
	if region != "" && region != "live" && region != "stale" {
		return nil, fmt.Errorf("region %q: want live or stale", region)
	}
	_, _, j, info, err := s.journalOf(o)
	if err != nil {
		return nil, err
	}
	var out []filesys.JournalTxn
	excluded := 0 // stale matches that NoStale left out
	if err := j.JournalTransactions(func(t filesys.JournalTxn) bool {
		if t.Seq == seq && (region == "" || t.Region == region) {
			if o.NoStale && t.Region == "stale" {
				excluded++
			} else {
				out = append(out, t)
			}
		}
		return true
	}); err != nil {
		return nil, err
	}
	what := fmt.Sprintf("transaction %d", seq)
	if region != "" {
		what += " of the " + region + " region"
	}
	switch len(out) {
	case 0:
		if excluded > 0 {
			return nil, fmt.Errorf("%s: %w", what, ErrStaleExcluded)
		}
		if info.Incomplete {
			return nil, fmt.Errorf("%s: not among the transactions read; the journal scan stopped at %s with later transactions unknown: %w", what, info.Stop, filesys.ErrIncomplete)
		}
		return nil, fmt.Errorf("%s is not a transaction of the journal: %w", what, filesys.ErrNotFound)
	case 1:
		return out, nil
	}
	starts := make([]string, len(out))
	for i, t := range out {
		starts[i] = fmt.Sprintf("%s region, journal block %d", t.Region, t.StartBlock)
	}
	return out, fmt.Errorf("%s names %d transactions (%s); name one with a region: %w", what, len(out), strings.Join(starts, "; "), filesys.ErrAmbiguous)
}

// jbd2Counts is what a rendered report holds.
type jbd2Counts struct{ Txns, Live, Stale, Committed, Tags, RevokeRecords int }

type jbd2JournalLine struct {
	Type          string   `json:"type"`
	Format        string   `json:"format"`
	Partition     int      `json:"partition"`
	Version       int      `json:"version"`
	BlockSize     int      `json:"block_size"`
	Blocks        int64    `json:"blocks"`
	First         uint32   `json:"first"`
	Sequence      uint32   `json:"sequence"`
	Start         uint32   `json:"start"`
	Features      []string `json:"features"`
	NeedsRecovery bool     `json:"needs_recovery"`
	LiveTxns      int      `json:"live_txns"`
	StaleTxns     int      `json:"stale_txns"`
	Complete      bool     `json:"complete"`
	Stop          string   `json:"stop,omitempty"`
	Warnings      []string `json:"warnings"`
	Caution       string   `json:"caution"`
}

type jbd2TxnLine struct {
	Type       string   `json:"type"`
	Seq        uint32   `json:"seq"`
	Region     string   `json:"region"`
	Committed  bool     `json:"committed"`
	StartJBlk  int64    `json:"start_jblk"`
	CommitJBlk int64    `json:"commit_jblk"`
	CommitTime string   `json:"commit_time,omitempty"`
	Checksum   string   `json:"checksum"`
	Tags       int      `json:"tags"`
	Revoked    []uint64 `json:"revoked"`
	Warnings   []string `json:"warnings"`
	Applied    string   `json:"applied"`
}

type jbd2TagLine struct {
	Type       string `json:"type"`
	Seq        uint32 `json:"seq"`
	Region     string `json:"region"`
	TxnStart   int64  `json:"txn_start"`
	Index      int    `json:"index"`
	FSBlock    uint64 `json:"fsblock"`
	JBlk       uint64 `json:"jblk"`
	FSLocation int64  `json:"fs_location"`
	Flags      uint32 `json:"flags"`
	Escaped    bool   `json:"escaped"`
	SameUUID   bool   `json:"same_uuid"`
	Deleted    bool   `json:"deleted"`
	Last       bool   `json:"last"`
	Copy       string `json:"copy"`
	Revoked    bool   `json:"revoked"`
}

type jbd2EndLine struct {
	Type          string `json:"type"`
	Txns          int    `json:"txns"`
	Live          int    `json:"live"`
	Stale         int    `json:"stale"`
	Committed     int    `json:"committed"`
	Tags          int    `json:"tags"`
	RevokeRecords int    `json:"revoke_records"`
}

type jbd2CutLine struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	AfterTxns int    `json:"after_txns"`
}

// jbd2Hidden reports whether a transaction is left out of a --no-stale listing.
func jbd2Hidden(t filesys.JournalTxn, noStale bool) bool { return noStale && t.Region == "stale" }

// copyUsable reports whether a tag's logged copy can be read (it has a log slot with bytes).
func copyUsable(state string) bool { return state != "missing" && state != "outside" }

// renderJBD2Report streams the report of j to w, at most maxBytes (the journal line is always written; a line that
// would pass the limit becomes a cut line and nothing after it is written). It is a pure function of the journal:
// the same journal, noStale and limit give the same bytes. cut reports a limit cut. The transactions are visited in
// the reader's order (live in log order, then the stale fragments by start block), so no map is iterated.
func renderJBD2Report(w io.Writer, part int, j filesys.Journaler, noStale bool, maxBytes int64) (filesys.JournalInfo, jbd2Counts, bool, error) {
	info, err := j.Journal()
	if err != nil {
		return info, jbd2Counts{}, false, err
	}
	var total jbd2Counts // what the report lists: the journal line states the live and stale counts
	if err := j.JournalTransactions(func(t filesys.JournalTxn) bool {
		if jbd2Hidden(t, noStale) {
			return true
		}
		total.Txns++
		if t.Region == "stale" {
			total.Stale++
		} else {
			total.Live++
		}
		if t.Committed {
			total.Committed++
		}
		total.Tags += len(t.Blocks)
		total.RevokeRecords += len(t.Revoked)
		return true
	}); err != nil {
		return info, total, false, err
	}
	lw := &lineWriter{w: bufio.NewWriterSize(w, 64<<10), max: maxBytes}
	warnings := append([]string{}, info.Warnings...)
	if noStale {
		warnings = append(warnings, jbd2StaleOffNote)
	}
	lw.emit(jbd2JournalLine{
		Type: "journal", Format: JBD2ReportFormat, Partition: part, Version: info.Version, BlockSize: info.BlockSize,
		Blocks: info.Blocks, First: info.First, Sequence: info.Sequence, Start: info.Start,
		Features: append([]string{}, info.Features...), NeedsRecovery: info.NeedsRecovery, LiveTxns: total.Live,
		StaleTxns: total.Stale, Complete: !info.Incomplete, Stop: info.Stop, Warnings: warnings, Caution: jbd2Caution,
	}, true)

	done := 0 // transactions whose every line was written
	var visitErr error
	err = j.JournalTransactions(func(t filesys.JournalTxn) bool {
		if jbd2Hidden(t, noStale) {
			return true
		}
		commit := t.CommitBlock
		if commit <= 0 {
			commit = -1
		}
		ok := lw.emit(jbd2TxnLine{
			Type: "txn", Seq: t.Seq, Region: t.Region, Committed: t.Committed, StartJBlk: t.StartBlock, CommitJBlk: commit,
			CommitTime: reportTime(t.CommitTime), Checksum: t.Checksum, Tags: len(t.Blocks),
			Revoked: append([]uint64{}, t.Revoked...), Warnings: append([]string{}, t.Warnings...), Applied: "unknown",
		}, false)
		for i, g := range t.Blocks {
			if !ok {
				break
			}
			loc := int64(-1)
			if copyUsable(g.Copy) {
				_, run, err := j.JournalBlock(t, i)
				switch {
				case err == nil:
					loc = run.Offset
				case errors.Is(err, filesys.ErrNotFound):
					// the reader holds no usable slot for it after all: the location stays unknown
				default:
					visitErr = err
					return false
				}
			}
			ok = lw.emit(jbd2TagLine{
				Type: "tag", Seq: t.Seq, Region: t.Region, TxnStart: t.StartBlock, Index: i, FSBlock: g.FSBlock, JBlk: g.JournalBlock,
				FSLocation: loc, Flags: g.Flags, Escaped: g.Escaped, SameUUID: g.Flags&jbd2TagSameUUID != 0,
				Deleted: g.Flags&jbd2TagDeleted != 0, Last: g.Flags&jbd2TagLast != 0, Copy: g.Copy, Revoked: g.Revoked,
			}, false)
		}
		if ok {
			done++
		}
		return ok && lw.err == nil
	})
	if err == nil {
		err = visitErr
	}
	if err == nil {
		err = lw.err
	}
	if err != nil {
		return info, total, lw.cut, err
	}
	lw.emit(jbd2EndLine{
		Type: "end", Txns: total.Txns, Live: total.Live, Stale: total.Stale, Committed: total.Committed, Tags: total.Tags,
		RevokeRecords: total.RevokeRecords,
	}, false)
	if lw.cut {
		lw.emit(jbd2CutLine{Type: "cut", Reason: "max-bytes", AfterTxns: done}, true)
	}
	if lw.err == nil {
		lw.err = lw.w.Flush()
	}
	return info, total, lw.cut, lw.err
}

// jbd2ReportRel is the case-relative name of the ext4 journal report of a partition.
func jbd2ReportRel(part int, fsType string) string {
	return fmt.Sprintf("reports/p%d-%s/journal.jsonl", part, fsType)
}

// jbd2ReportCutError is the incomplete flag text of a rendering, nil when it is whole.
func jbd2ReportCutError(cut bool, info filesys.JournalInfo, maxBytes int64) error {
	var msgs []string
	if cut {
		msgs = append(msgs, "report cut by max-bytes "+strconv.FormatInt(maxBytes, 10)+": later lines are not written")
	}
	if info.Incomplete {
		msgs = append(msgs, "ext4 journal incomplete: stopped at "+info.Stop+", later transactions are unknown")
	}
	if len(msgs) == 0 {
		return nil
	}
	return &reportCut{msg: strings.Join(msgs, "; ")}
}

// jbd2ReportParams are the parameters recorded with a report, so case verify renders the same one (R13).
func jbd2ReportParams(maxBytes int64, noStale bool) map[string]string {
	stale := "on"
	if noStale {
		stale = "off"
	}
	return map[string]string{"format": JBD2ReportFormat, "max_bytes": strconv.FormatInt(maxBytes, 10), "stale": stale}
}

// jbd2Report writes the jbd2 journal of the partition as one deterministic JSONL report (3G-R16): kind report, class
// journal-report, method ext4-journal, scope journal, no confidence, no runs, no single-transaction reference. A
// journal that stops with later transactions unknown, or a report cut by the byte limit, is flagged incomplete.
func (s *Session) jbd2Report(ctx context.Context, o JournalOptions, b *budget, fsys filesys.FileSystem, part volume.Partition) (JournalReportSummary, error) {
	j, err := journalerOf(fsys)
	if err != nil {
		return JournalReportSummary{}, err
	}
	// a pure read: a refusal (no journal, an unusable one) audits nothing
	if _, err := j.Journal(); err != nil {
		return JournalReportSummary{}, journalInfoErr(fsys, err)
	}
	if err := j.JournalTransactions(func(filesys.JournalTxn) bool { return true }); err != nil {
		return JournalReportSummary{}, err
	}
	fsType := fsys.Info().Type
	params := jbd2ReportParams(b.maxBytes, o.NoStale)
	var js JournalReportSummary
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType, "format": JBD2ReportFormat,
		"max_bytes": b.maxBytes, "stale": params["stale"],
	}
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "journal-report", details, func(a *analysis) error {
		defer func() {
			a.setEnd("journal_txns", js.Txns)
			a.setEnd("journal_stop", js.Stop)
			a.setEnd("journal_complete", js.Complete)
		}()
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		d := s.baseDerivation(part, fsType)
		d.Recovery = &evidence.Recovery{
			Class: evidence.ClassJournalReport, Method: "ext4-journal", Scope: "journal",
			Algorithm: evidence.AlgorithmRecover, Params: params,
		}
		src := evidence.Source{Kind: evidence.KindReport, DeviceID: s.Parent.Source.DeviceID, Derived: d}
		rec, fillErr, err := s.capture(a, jbd2ReportRel(part.Index, fsType), src, func(w io.Writer) error {
			info, counts, cut, err := renderJBD2Report(&ctxWriter{ctx: ctx, w: w}, part.Index, j, o.NoStale, b.maxBytes)
			js.Txns, js.LiveTxns, js.StaleTxns, js.Tags, js.Committed, js.RevokeRecords =
				counts.Txns, counts.Live, counts.Stale, counts.Tags, counts.Committed, counts.RevokeRecords
			js.Complete, js.Stop, js.Cut = !info.Incomplete, info.Stop, cut
			if err != nil {
				return err
			}
			return jbd2ReportCutError(cut, info, b.maxBytes)
		})
		if err != nil {
			return err // the case itself failed: never downgraded
		}
		js.Path = rec.Path
		var rc *reportCut
		switch {
		case fillErr == nil:
			a.sum.Files++
			a.sum.Bytes += rec.Size
		case errors.As(fillErr, &rc):
			// a planned cut: described by the artifact itself
		default:
			return fillErr
		}
		return nil
	})
	js.Summary = sum
	return js, err
}

// touchesFree reports whether any byte of [off, off+length) lies in a free run. An empty, negative or overflowing
// range is reported as touching: it cannot be placed, so it is never copied.
func (m *freeMap) touchesFree(off, length int64) bool {
	if off < 0 || length <= 0 {
		return true
	}
	end, ok := filesys.AddOK(off, length)
	if !ok {
		return true
	}
	i := sort.Search(len(m.free), func(i int) bool { return m.free[i].Offset+m.free[i].Length > off })
	return i < len(m.free) && m.free[i].Offset < end
}

// jbd2Sel is one tag of the selected transaction.
type jbd2Sel struct {
	index int
	tag   filesys.JournalTag
}

// jbd2Blocks copies the logged blocks of one ext4 transaction (3G-R17): one journal-block artifact per tag whose
// logged copy can be read, each the exact bytes of its log slot (an escaped block keeps its zeroed magic). A tag
// with no readable copy is skipped as "missing", one whose checksum failed as "copy-mismatch", and one whose log
// slot the bitmap calls free or unknown as "not-allocated": each is warned and counted, never copied. An unknown
// seq is ErrNotFound (ErrIncomplete on an incomplete journal) and a seq that names several transactions is
// ErrAmbiguous, both before anything is audited.
func (s *Session) jbd2Blocks(ctx context.Context, o JournalBlocksOptions, b *budget, fsys filesys.FileSystem, part volume.Partition) (JournalBlocksSummary, error) {
	txns, err := s.JournalTxns(o.JournalOptions, o.Seq, o.Region)
	if err != nil {
		return JournalBlocksSummary{}, err
	}
	x := txns[0]
	j, err := journalerOf(fsys)
	if err != nil {
		return JournalBlocksSummary{}, err
	}
	info, err := j.Journal()
	if err != nil {
		return JournalBlocksSummary{}, journalInfoErr(fsys, err)
	}
	var sel []jbd2Sel
	for i, g := range x.Blocks {
		if o.FSBlock == nil || *o.FSBlock == g.FSBlock {
			sel = append(sel, jbd2Sel{index: i, tag: g})
		}
	}
	if o.FSBlock != nil && len(sel) == 0 {
		if info.Incomplete {
			return JournalBlocksSummary{}, fmt.Errorf("filesystem block %d: not among the tags of transaction %d (%s region, journal block %d); the journal scan stopped at %s with later transactions unknown: %w", *o.FSBlock, x.Seq, x.Region, x.StartBlock, info.Stop, filesys.ErrIncomplete)
		}
		return JournalBlocksSummary{}, fmt.Errorf("filesystem block %d is not logged by transaction %d (%s region, journal block %d): %w", *o.FSBlock, x.Seq, x.Region, x.StartBlock, filesys.ErrNotFound)
	}
	fsType := fsys.Info().Type
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType, "format": evidence.AlgorithmRecover,
		"seq": o.Seq, "region": x.Region, "txn_start": x.StartBlock, "max_files": b.maxFiles, "max_bytes": b.maxBytes,
	}
	if o.FSBlock != nil {
		details["fsblock"] = *o.FSBlock
	}
	var js JournalBlocksSummary
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "journal-blocks", details, func(a *analysis) error {
		defer func() {
			a.setEnd("selected", len(sel))
			a.setEnd("copied", js.Copied)
			a.setEnd("copy_mismatch", js.CopyMismatch)
			a.setEnd("missing", js.Missing)
			a.setEnd("not_allocated", js.NotAllocated)
			a.setEnd("alloc_unknown", js.AllocUnknown)
		}()
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		runs, _, err := s.unallocatedRunsNote(fsys, part, false)
		if err != nil {
			return fmt.Errorf("the unallocated space of partition %d cannot be listed: %w", part.Index, err)
		}
		// A log slot is owned by the journal inode, an allocated inode, so it is allocated unless the bitmap calls it free:
		// that is the disagreement this check reports. The "unknown when the filesystem warns" reading of the other
		// recovery checks is not used here: a crash image always carries the needs-recovery warning, and it would skip
		// every block of the one journal worth copying.
		free := newFreeMap(runs, false)
		// the view every other recovery check uses: with a filesystem warning a non-free byte is unknown (ruling C17)
		unknownView := newFreeMap(runs, len(fsys.Info().Warnings) > 0)
		if err := a.syncFS(); err != nil {
			return err
		}
		if err := s.checkSpace(int64(len(sel)) * int64(info.BlockSize)); err != nil {
			return err
		}
		names := map[string]int{}
		for _, g := range sel {
			if err := ctx.Err(); err != nil {
				return err
			}
			label := fmt.Sprintf("journal block seq %d tag %d (fsblock %d)", x.Seq, g.index, g.tag.FSBlock)
			switch g.tag.Copy {
			case "mismatch":
				js.CopyMismatch++
				if err := a.warn(label, "copy-mismatch: the checksum of the logged copy fails (a later lap of the log overwrote the slot, or it is damaged), so it is not copied"); err != nil {
					return err
				}
				continue
			case "missing", "outside":
				js.Missing++
				if err := a.warn(label, "missing: the log slot holds no logged copy ("+g.tag.Copy+"), so nothing is copied"); err != nil {
					return err
				}
				continue
			}
			data, run, err := j.JournalBlock(x, g.index)
			if err != nil {
				if ctx.Err() != nil || (!errors.Is(err, filesys.ErrNotFound) && !skippable(err)) {
					return err
				}
				if err := a.warn(label, "not copied: the logged copy cannot be read: "+err.Error()); err != nil {
					return err
				}
				continue
			}
			size := int64(len(data))
			off, ok := filesys.AddOK(part.Start, run.Offset)
			if !ok || size <= 0 || run.Length != size || free.touchesFree(off, size) {
				js.NotAllocated++
				if err := a.warn(label, "not-allocated: the log slot lies in space the filesystem calls free although the journal inode owns it (the bitmap and the journal inode disagree), so it is not copied"); err != nil {
					return err
				}
				continue
			}
			if reason := b.admit(size); reason != "" {
				a.sum.OutputLimit = reason
				return a.warn(label, "not copied: output limit "+reason+" reached")
			}
			if err := s.copyJBD2Block(ctx, a, part, fsType, x, g, off, size, unknownView.nonFree == "unknown", names, &js); err != nil {
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

// copyJBD2Block writes one logged block as a journal-block artifact. A read failure keeps the prefix copied and flags
// the artifact incomplete; a failure of the case itself is returned.
func (s *Session) copyJBD2Block(ctx context.Context, a *analysis, part volume.Partition, fsType string, x filesys.JournalTxn, g jbd2Sel, off, size int64, unknownAlloc bool, names map[string]int, js *JournalBlocksSummary) error {
	run := evidence.Run{Offset: off, Length: size}
	d := s.baseDerivation(part, fsType)
	d.Runs = []evidence.Run{run}
	var assumptions []string
	if g.tag.Escaped {
		assumptions = append(assumptions, "jbd2-escaped")
	}
	if g.tag.Copy == "unverified" {
		assumptions = append(assumptions, "journal-copy-unverified")
	}
	rv := &evidence.Recovery{
		Class: evidence.ClassJournalBlock, Method: "ext4-journal", Scope: "journal",
		Alloc: evidence.AllocSummary{Allocated: size},
		Basis: []string{
			fmt.Sprintf("jbd2:seq=%d", x.Seq), "region=" + x.Region, fmt.Sprintf("txn-start=%d", x.StartBlock),
			fmt.Sprintf("tag=%d", g.index), fmt.Sprintf("fsblk=%d", g.tag.FSBlock), fmt.Sprintf("jblk=%d", g.tag.JournalBlock),
		},
		Assumptions: assumptions,
		Journal:     &evidence.JournalRef{Seq: x.Seq, Committed: x.Committed, Region: x.Region, Revoked: g.tag.Revoked},
		Algorithm:   evidence.AlgorithmRecover,
	}
	if unknownAlloc { // Allocated rests on the journal inode's map (structure); the bitmap view is unknown
		rv.Basis = append(rv.Basis, "alloc-basis=journal-inode")
		rv.Assumptions = append(rv.Assumptions, "bitmap-unconfirmed")
	}
	d.Recovery = rv
	src := evidence.Source{Kind: evidence.KindJournal, DeviceID: s.Parent.Source.DeviceID, Derived: d}
	// Nothing that case verify would reject is ever written: a failure here is a defect of this build.
	probe := evidence.ManifestRecord{Path: "journal/probe", Size: size, Source: src}
	if probs := append(rv.Check(evidence.KindJournal), evidence.CheckRecoveredRuns(probe, d.Runs)...); len(probs) > 0 {
		return fmt.Errorf("internal error: the block copy of %d would not pass verification: %s", g.tag.FSBlock, strings.Join(probs, "; "))
	}
	rel := fmt.Sprintf("journal/p%d-%s/seq%d-fsblk%d.bin", part.Index, fsType, x.Seq, g.tag.FSBlock)
	names[rel]++
	if n := names[rel]; n > 1 { // the same filesystem block logged twice by one transaction
		rel = strings.TrimSuffix(rel, ".bin") + fmt.Sprintf("-%d.bin", n)
	}
	var copied int64
	rec, fillErr, err := s.capture(a, rel, src, func(w io.Writer) error {
		var err error
		copied, err = io.Copy(w, &ctxReader{ctx: ctx, r: io.NewSectionReader(s.Image, run.Offset, run.Length)})
		if err == nil && copied != run.Length {
			err = fmt.Errorf("image ended after %d of %d bytes: %w", copied, run.Length, io.ErrUnexpectedEOF)
		}
		if err != nil { // the runs of the bytes that were captured
			rv.Alloc.Allocated = copied
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
		if unknownAlloc {
			js.AllocUnknown++
		}
		a.sum.Files++
		a.sum.Bytes += rec.Size
		return nil
	}
	if ctx.Err() != nil || !skippable(fillErr) {
		return fillErr
	}
	return a.warn(fmt.Sprintf("journal block seq %d tag %d", x.Seq, g.index), fmt.Sprintf("read failed after %d of %d bytes (partial artifact kept, flagged incomplete): %v", copied, run.Length, fillErr))
}

// ErrStaleExcluded is returned when the transaction asked for exists only in the stale region and --no-stale left
// that region out. It is not ErrNotFound: the transaction exists.
var ErrStaleExcluded = errors.New("the transaction exists only in the stale region, which --no-stale left out")
