package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// rollForwardCaution follows every roll-forward listing: the data is what a replay would find, never live.
const rollForwardCaution = "roll-forward data is what a replay would find; it was not replayed and is not part of the live view"

// journalUnavailable is examine.ErrJournalNotAvailable with the filesystem named. It never says the filesystem
// has no journal: this build having no analysis of a journal is not that journal being absent.
type journalUnavailable struct{ fsType string }

func (e *journalUnavailable) Error() string {
	return fmt.Sprintf("journal analysis is not available for %s in this build (this says nothing about whether the filesystem keeps a journal)", escapeText(e.fsType))
}

func (e *journalUnavailable) Unwrap() error { return examine.ErrJournalNotAvailable }

// journalErr names the filesystem in a "not available" error; every other error passes unchanged.
func journalErr(s *examine.Session, pidx int, err error) error {
	if err == nil || !errors.Is(err, examine.ErrJournalNotAvailable) {
		return err
	}
	if fsys, _, ferr := s.FS(pidx); ferr == nil {
		return &journalUnavailable{fsType: fsys.Info().Type}
	}
	return err
}

func newImageJournalCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("journal", "List and export the journal of a partition's filesystem (ext4 jbd2 journal, F2FS roll-forward log)")
	cmd.AddCommand(newJournalLsCmd(d, opts), newJournalShowCmd(d, opts), newJournalReportCmd(d, opts), newJournalBlocksCmd(d, opts))
	return cmd
}

// parseRegion validates a --region value: "", live or stale. Anything else is a usage error.
func parseRegion(r string) error {
	if r != "" && r != "live" && r != "stale" {
		return usageErrorf("invalid --region %q: expected live or stale", r)
	}
	return nil
}

// noStaleFlag adds --no-stale: the stale transactions of an ext4 journal are left out (F2FS ignores it).
func noStaleFlag(cmd *cobra.Command) *bool {
	return cmd.Flags().Bool("no-stale", false, "leave the stale transactions of an ext4 journal out (they are listed by default)")
}

// regionFlag adds --region, which names one of several ext4 transactions that share a sequence number.
func regionFlag(cmd *cobra.Command) *string {
	return cmd.Flags().String("region", "", "ext4: live or stale, to name one of several transactions with the same sequence")
}

// parseSeq parses a transaction sequence number.
func parseSeq(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, usageErrorf("invalid transaction sequence %q: expected a number from 0 to 4294967295", s)
	}
	return uint32(v), nil
}

// chainMode names how the chain's footer version is derived (3H-R2).
func chainMode(c filesys.RollForwardChain) string {
	switch {
	case c.NoCRCRecovery:
		return "nocrc-recovery"
	case c.CRCRecovery:
		return "crc-recovery"
	}
	return "no-crc-flag"
}

// printIncompleteChain prints the INCOMPLETE line of a chain whose later nodes are unknown.
func printIncompleteChain(w io.Writer, c filesys.RollForwardChain) {
	if !c.Complete {
		fmt.Fprintf(w, "INCOMPLETE: the chain is cut at %s; later nodes are unknown\n", escapeText(c.Stop))
	}
}

func fileKindCounts(c filesys.RollForwardChain) (post, live, notReplayed int) {
	for _, f := range c.Files {
		switch f.Kind {
		case "post-checkpoint":
			post++
		case "live-update":
			live++
		case "not-replayed":
			notReplayed++
		}
	}
	return post, live, notReplayed
}

type jsonJournalTxn struct {
	Seq       uint32 `json:"seq"`
	Blocks    int    `json:"blocks"`
	Committed bool   `json:"committed"`
	Region    string `json:"region"`
	Replay    int    `json:"would_replay"`
}

type jsonJournalFiles struct {
	PostCheckpoint int `json:"post_checkpoint"`
	LiveUpdates    int `json:"live_updates"`
	NotReplayed    int `json:"not_replayed"`
}

type jsonJournalLs struct {
	Partition     int              `json:"partition"`
	FSType        string           `json:"fs_type"`
	Type          string           `json:"type"`
	Checkpoint    uint64           `json:"checkpoint_version"`
	Pack          int              `json:"checkpoint_pack"`
	Mode          string           `json:"mode"`
	Start         uint64           `json:"start_block"`
	Nodes         int              `json:"nodes"`
	Stop          string           `json:"stop"`
	Complete      bool             `json:"complete"`
	Incomplete    bool             `json:"incomplete"`
	NeedsRecovery bool             `json:"needs_recovery"`
	Files         jsonJournalFiles `json:"files"`
	Transactions  []jsonJournalTxn `json:"transactions"`
	Warnings      []string         `json:"warnings"`
}

func newJournalLsCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls <ref> [-p N]",
		Short: "List the journal of a partition's filesystem (read-only)",
		Long: "Ls describes the journal of the filesystem and its transactions. For ext4 it lists the jbd2 journal: the live\n" +
			"region a recovery would scan and the stale transactions still physically present (--no-stale leaves those out);\n" +
			"none of them was replayed. For F2FS it lists the roll-forward log: the\n" +
			"nodes written after the last checkpoint. Nothing is written to the case but the usual case.open audit entry.\n" +
			"A checkpoint written at a clean unmount does not mean the log is empty: read the node count, not the checkpoint.",
		Args: exactArgs(1),
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	noStale := noStaleFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		jo := examine.JournalOptions{Partition: pidx, NoStale: *noStale}
		l, err := s.JournalList(jo)
		if err != nil {
			return journalErr(s, pidx, err)
		}
		_, part, _ := s.FS(pidx)
		if isJBD2(l) {
			return listJBD2(d.Out, opts.json, part.Index, l)
		}
		out := jsonJournalLs{
			Partition: part.Index, FSType: l.FSType, Type: l.Info.Type, NeedsRecovery: l.Info.NeedsRecovery,
			Transactions: []jsonJournalTxn{}, Warnings: append([]string{}, l.Info.Warnings...), Complete: true,
		}
		if l.Chain != nil {
			c := l.Chain
			out.Checkpoint, out.Pack, out.Mode, out.Start, out.Nodes, out.Stop = c.CheckpointVersion, c.CheckpointPack, chainMode(*c), c.Start, c.Nodes, c.Stop
			out.Complete, out.Incomplete = c.Complete, !c.Complete
			out.Files.PostCheckpoint, out.Files.LiveUpdates, out.Files.NotReplayed = fileKindCounts(*c)
			out.Warnings = append(out.Warnings, c.Warnings...)
		}
		for _, t := range l.Txns {
			jt := jsonJournalTxn{Seq: t.Seq, Blocks: len(t.Blocks), Committed: t.Committed, Region: t.Region}
			if l.Chain != nil { // how many of its nodes a replay would apply
				if _, err := s.JournalShow(jo, t.Seq, func(n filesys.RollForwardNode) bool {
					if n.WouldReplay {
						jt.Replay++
					}
					return true
				}); err != nil {
					return journalErr(s, pidx, err)
				}
			}
			out.Transactions = append(out.Transactions, jt)
		}
		if opts.json {
			return writeJSON(d.Out, out)
		}
		printJournalLs(d.Out, l, out)
		return nil
	}
	return cmd
}

func printJournalLs(w io.Writer, l examine.JournalListing, j jsonJournalLs) {
	if l.Chain == nil {
		fmt.Fprintf(w, "journal of partition %d (%s, type %s): %d blocks, %d transactions\n", j.Partition, escapeText(l.FSType), escapeText(l.Info.Type), l.Info.Blocks, len(l.Txns))
		for _, t := range l.Txns {
			fmt.Fprintf(w, "transaction %d: %d blocks\n", t.Seq, len(t.Blocks))
		}
		for _, wn := range j.Warnings {
			fmt.Fprintf(w, "warning: %s\n", escapeText(wn))
		}
		return
	}
	fmt.Fprintf(w, "%s roll-forward log of partition %d (checkpoint %d, pack %d, %s): start block %d, %d nodes, stop %s\n",
		escapeText(l.FSType), j.Partition, j.Checkpoint, j.Pack, j.Mode, j.Start, j.Nodes, escapeText(j.Stop))
	if len(j.Transactions) == 0 {
		if j.Complete {
			fmt.Fprintln(w, "(empty: the checkpoint's log holds no node written after it)")
		} else {
			fmt.Fprintf(w, "(no node was read before the chain was cut at %s; later nodes are unknown, so the log is not shown empty)\n", escapeText(j.Stop))
		}
	}
	for _, t := range j.Transactions {
		fmt.Fprintf(w, "transaction %d: %d blocks, %d would be replayed; files: %d post-checkpoint, %d live update(s), %d not replayed\n",
			t.Seq, t.Blocks, t.Replay, j.Files.PostCheckpoint, j.Files.LiveUpdates, j.Files.NotReplayed)
	}
	if j.Nodes > 0 || !j.Complete || j.NeedsRecovery {
		fmt.Fprintf(w, "needs recovery: %s (the kernel's recovery would find a node to apply)\n", map[bool]string{true: "yes", false: "no"}[j.NeedsRecovery])
	}
	printIncompleteChain(w, *l.Chain)
	for _, wn := range j.Warnings {
		fmt.Fprintf(w, "warning: %s\n", escapeText(wn))
	}
	fmt.Fprintln(w, rollForwardCaution)
}

type jsonJournalNode struct {
	Index       int      `json:"index"`
	Block       uint64   `json:"block"`
	NID         uint32   `json:"nid"`
	Ino         uint32   `json:"ino"`
	Kind        string   `json:"kind"`
	Ofs         uint32   `json:"ofs"`
	Fsync       bool     `json:"fsync"`
	Dentry      bool     `json:"dentry"`
	WouldReplay bool     `json:"would_replay"`
	LiveIno     bool     `json:"live_ino"`
	Next        uint64   `json:"next"`
	Inode       *jsonInd `json:"inode,omitempty"`
}

type jsonInd struct {
	Size     int64  `json:"size"`
	Mode     uint32 `json:"mode"`
	Parent   uint32 `json:"parent"`
	Name     string `json:"name"`
	Checksum string `json:"checksum"`
	Err      string `json:"error,omitempty"`
}

type jsonJournalShow struct {
	Seq        uint32            `json:"seq"`
	Nodes      []jsonJournalNode `json:"nodes"`
	Complete   bool              `json:"complete"`
	Incomplete bool              `json:"incomplete"`
	Stop       string            `json:"stop"`
}

func newJournalShowCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <ref> <seq> [-p N]",
		Short: "Show the nodes of one transaction of the journal (read-only)",
		Long: "Show prints one transaction of the journal. For ext4 it prints the transaction line and one line per logged\n" +
			"block (a sequence that names a live and a stale transaction needs --region). For F2FS it prints one line per node of\n" +
			"the roll-forward log (the transaction named by the sequence that ls prints):\n" +
			"its block, ids, kind, marks, whether a replay would apply it and whether its inode number is live. Nothing is\n" +
			"written to the case but the usual case.open audit entry.",
		Args: exactArgs(2),
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	noStale := noStaleFlag(cmd)
	region := regionFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		seq, err := parseSeq(args[1])
		if err != nil {
			return err
		}
		if err := parseRegion(*region); err != nil {
			return err
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		res := jsonJournalShow{Seq: seq, Nodes: []jsonJournalNode{}}
		var lines strings.Builder
		jo := examine.JournalOptions{Partition: pidx, NoStale: *noStale}
		chain, err := s.JournalShow(jo, seq, func(n filesys.RollForwardNode) bool {
			jn := jsonJournalNode{
				Index: n.Index, Block: n.Block, NID: n.NID, Ino: n.Ino, Kind: n.Kind, Ofs: n.Ofs, Fsync: n.Fsync, Dentry: n.Dentry,
				WouldReplay: n.WouldReplay, LiveIno: n.LiveIno, Next: n.Next,
			}
			fmt.Fprintf(&lines, "#%d blk %d nid %d ino %d %s ofs %d", n.Index, n.Block, n.NID, n.Ino, escapeText(n.Kind), n.Ofs)
			if n.Fsync {
				lines.WriteString(" fsync")
			}
			if n.Dentry {
				lines.WriteString(" dentry")
			}
			fmt.Fprintf(&lines, " replay=%s live-ino=%s next %d\n", yesNo(n.WouldReplay), yesNo(n.LiveIno), n.Next)
			if in := n.Inode; in != nil {
				jn.Inode = &jsonInd{Size: in.Size, Mode: in.Mode, Parent: in.Pino, Name: in.Name, Checksum: in.Checksum, Err: in.Err}
				fmt.Fprintf(&lines, "    size %d mode %#o parent %d name %s checksum %s\n", in.Size, in.Mode, in.Pino, printable(in.Name), escapeText(in.Checksum))
				if in.Err != "" {
					fmt.Fprintf(&lines, "    unreadable inode: %s\n", escapeText(in.Err))
				}
			}
			res.Nodes = append(res.Nodes, jn)
			return true
		})
		if errors.Is(err, examine.ErrJournalNotAvailable) { // not a roll-forward filesystem: the ext4 journal, or none
			return showJBD2(d.Out, opts.json, s, jo, seq, *region, func(err error) error { return journalErr(s, pidx, err) })
		}
		if err != nil {
			return journalErr(s, pidx, err)
		}
		res.Complete, res.Incomplete, res.Stop = chain.Complete, !chain.Complete, chain.Stop
		if opts.json {
			return writeJSON(d.Out, res)
		}
		fmt.Fprint(d.Out, lines.String())
		printIncompleteChain(d.Out, chain)
		fmt.Fprintln(d.Out, rollForwardCaution)
		return nil
	}
	return cmd
}

type jsonJournalReport struct {
	AnalysisID       string `json:"analysis_id"`
	Artifact         string `json:"artifact"`
	Path             string `json:"path"`
	Nodes            int    `json:"nodes"`
	Files            int    `json:"files"`
	PostCheckpoint   int    `json:"post_checkpoint"`
	LiveUpdates      int    `json:"live_updates"`
	NotReplayed      int    `json:"not_replayed"`
	WouldReplay      int    `json:"would_replay"`
	Complete         bool   `json:"complete"`
	Stop             string `json:"stop"`
	Incomplete       bool   `json:"incomplete"`
	IncompleteReason string `json:"incomplete_reason"`
}

func newJournalReportCmd(d Deps, opts *rootOptions) *cobra.Command {
	var maxBytes int64
	cmd := &cobra.Command{
		Use:   "report <ref> [-p N] [--max-bytes N]",
		Short: "Write the roll-forward log of a partition into the case as a report artifact",
		Long: "Report writes a derived artifact that case verify renders again from the image. For ext4 it is\n" +
			"reports/p<N>-ext4/journal.jsonl: one line per transaction and per logged block (--no-stale leaves the stale\n" +
			"transactions out). For F2FS it is reports/p<N>-f2fs/rollforward.jsonl: one line per chain node and per file, as a derived artifact\n" +
			"that case verify renders again from the image. A chain cut with later nodes unknown, or a report cut by\n" +
			"--max-bytes, is kept and flagged INCOMPLETE; the command still exits 0.",
		Args: exactArgs(1),
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.Flags().Int64Var(&maxBytes, "max-bytes", 0, fmt.Sprintf("cut the report at this many bytes (default %d; 0 uses the default)", examine.DefaultMaxBytes))
	noStale := noStaleFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		if maxBytes < 0 {
			return usageErrorf("--max-bytes must not be negative, got %d", maxBytes)
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		sum, err := s.JournalReport(cmd.Context(), examine.JournalOptions{Partition: pidx, NoStale: *noStale}, maxBytes)
		if sum.AnalysisID == "" || len(sum.Artifacts) == 0 {
			if err == nil {
				err = errors.New("internal error: the roll-forward report wrote no artifact")
			}
			return journalErr(s, pidx, err)
		}
		rec := sum.Artifacts[0]
		if isJBD2Artifact(rec) {
			return reportJBD2(d.Out, opts.json, sum, rec, *noStale, err)
		}
		j := jsonJournalReport{
			AnalysisID: sum.AnalysisID, Artifact: rec.ID, Path: rec.Path, Nodes: sum.Nodes, Files: sum.Files,
			PostCheckpoint: sum.PostCheckpoint, LiveUpdates: sum.LiveUpdates, NotReplayed: sum.NotReplayed, WouldReplay: sum.WouldReplay,
			Complete: sum.Complete, Stop: sum.Stop, Incomplete: rec.Incomplete, IncompleteReason: rec.Error,
		}
		if opts.json {
			return joinJSON(err, writeJSON(d.Out, j))
		}
		fmt.Fprintf(d.Out, "report %s %s: %d nodes, %d files\n", escapeText(rec.ID), escapeText(rec.Path), sum.Nodes, sum.Files)
		switch {
		case !sum.Complete:
			printIncompleteChain(d.Out, filesys.RollForwardChain{Stop: sum.Stop})
		case rec.Incomplete:
			fmt.Fprintf(d.Out, "INCOMPLETE: the report is cut: %s\n", escapeText(rec.Error))
		}
		fmt.Fprintln(d.Out, rollForwardCaution)
		return err
	}
	return cmd
}

type jsonJournalBlock struct {
	Block    uint64 `json:"block"`
	Artifact string `json:"artifact"`
	Path     string `json:"path"`
}

type jsonJournalBlocks struct {
	AnalysisID string             `json:"analysis_id"`
	Seq        uint32             `json:"seq"`
	Copied     int                `json:"copied"`
	NotFree    int                `json:"skipped_not_free"`
	Complete   bool               `json:"complete"`
	Stop       string             `json:"stop"`
	Incomplete bool               `json:"incomplete"`
	Blocks     []jsonJournalBlock `json:"blocks"`
}

// basisUint returns the value of the "<key>=<n>" entry of basis.
func basisUint(basis []string, key string) (uint64, bool) {
	for _, b := range basis {
		if v, ok := strings.CutPrefix(b, key+"="); ok {
			n, err := strconv.ParseUint(v, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func newJournalBlocksCmd(d Deps, opts *rootOptions) *cobra.Command {
	var fsblock uint64
	var maxFiles int
	var maxBytes int64
	cmd := &cobra.Command{
		Use:   "blocks <ref> <seq> [-p N] [--fsblock N] [--max-files N] [--max-bytes N]",
		Short: "Copy the node blocks of the roll-forward log into the case",
		Long: "Blocks writes one journal-block artifact per logged block of an ext4 transaction (or only the block at --fsblock),\n" +
			"under journal/p<N>-ext4/: the exact bytes of its log slot. A block whose copy fails its checksum, has no readable\n" +
			"copy or lies in space the bitmap calls free is counted and never copied.\n" +
			"For F2FS it writes one journal-block artifact per node of the roll-forward log (or only the node at --fsblock), under\n" +
			"journal/p<N>-f2fs/. A block is copied only when the checkpoint view reports it free; one that is not is counted\n" +
			"as skipped not-free and never copied.",
		Args: exactArgs(2),
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.Flags().Uint64Var(&fsblock, "fsblock", 0, "copy only the node at this filesystem block")
	cmd.Flags().IntVar(&maxFiles, "max-files", 0, fmt.Sprintf("stop after this many artifacts (default %d; 0 uses the default)", examine.DefaultMaxFiles))
	cmd.Flags().Int64Var(&maxBytes, "max-bytes", 0, fmt.Sprintf("stop after this many bytes (default %d; 0 uses the default)", examine.DefaultMaxBytes))
	noStale := noStaleFlag(cmd)
	region := regionFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		seq, err := parseSeq(args[1])
		if err != nil {
			return err
		}
		if err := parseRegion(*region); err != nil {
			return err
		}
		if maxFiles < 0 || maxBytes < 0 {
			return usageErrorf("--max-files and --max-bytes must not be negative")
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		o := examine.JournalBlocksOptions{JournalOptions: examine.JournalOptions{Partition: pidx, NoStale: *noStale}, Seq: seq, Region: *region, MaxFiles: maxFiles, MaxBytes: maxBytes}
		if cmd.Flags().Changed("fsblock") {
			o.FSBlock = &fsblock
		}
		sum, err := s.JournalBlocks(cmd.Context(), o)
		if sum.AnalysisID == "" {
			return journalErr(s, pidx, err)
		}
		if l, lerr := s.JournalList(o.JournalOptions); lerr == nil && isJBD2(l) {
			return blocksJBD2(d.Out, opts.json, seq, l, sum, err)
		}
		res := jsonJournalBlocks{AnalysisID: sum.AnalysisID, Seq: seq, Copied: sum.Copied, NotFree: sum.NotFree, Complete: true, Blocks: []jsonJournalBlock{}}
		if l, lerr := s.JournalList(o.JournalOptions); lerr == nil && l.Chain != nil { // the copies cover the nodes read, not an unknown tail
			res.Complete, res.Incomplete, res.Stop = l.Chain.Complete, !l.Chain.Complete, l.Chain.Stop
		}
		for _, a := range sum.Artifacts {
			d := a.Source.Derived
			if a.Source.Kind != evidence.KindJournal || d == nil || d.Recovery == nil {
				continue
			}
			blk, _ := basisUint(d.Recovery.Basis, "blk")
			res.Blocks = append(res.Blocks, jsonJournalBlock{Block: blk, Artifact: a.ID, Path: a.Path})
		}
		if opts.json {
			return joinJSON(err, writeJSON(d.Out, res))
		}
		for _, b := range res.Blocks {
			fmt.Fprintf(d.Out, "[journal-block:%s seq %d] blk %d -> %s %s\n", evidence.RollForwardMethod, seq, b.Block, escapeText(b.Artifact), escapeText(b.Path))
		}
		fmt.Fprintf(d.Out, "copied %d\nskipped not-free: %d\n", sum.Copied, sum.NotFree)
		if res.Incomplete {
			printIncompleteChain(d.Out, filesys.RollForwardChain{Stop: res.Stop})
		}
		fmt.Fprintln(d.Out, rollForwardCaution)
		return err
	}
	return cmd
}
