package examine

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// RollForwardReportFormat names the line format of the roll-forward report (3H-R12).
const RollForwardReportFormat = "minutiae-f2fs-rollforward/1"

// JournalReportSummary is the outcome of JournalReport. Files and the counters after it describe the chain;
// Summary.Files counts the finished artifacts as everywhere else.
type JournalReportSummary struct {
	Summary
	Nodes, Files, PostCheckpoint, LiveUpdates, NotReplayed, WouldReplay int
	Complete                                                            bool
	Stop                                                                string
	Path                                                                string
}

// reportCutReserve is room kept for the closing "cut" line, so a report never exceeds its limit.
const reportCutReserve = 96

// reportCut ends a report that is a deterministic rendering of less than the whole chain: the artifact is
// kept and flagged incomplete with this text (the cut line, or an unknown tail of the chain).
type reportCut struct{ msg string }

func (e *reportCut) Error() string { return e.msg }

// reportRel is the case-relative name of the roll-forward report of a partition.
func reportRel(part int, fsType string) string {
	return fmt.Sprintf("reports/p%d-%s/rollforward.jsonl", part, fsType)
}

// JournalReport writes the roll-forward chain of the partition as one deterministic JSONL report artifact
// (kind report, class journal-report): the chain line, one line per node, one per file and an end line. A
// chain that stops with later nodes unknown, or a report cut by maxBytes (0 = DefaultMaxBytes), is flagged
// incomplete. The report is a pure function of the image (3H-R12): case verify renders it again.
func (s *Session) JournalReport(ctx context.Context, o JournalOptions, maxBytes int64) (JournalReportSummary, error) {
	b, err := newBudget(0, maxBytes)
	if err != nil {
		return JournalReportSummary{}, err
	}
	fsys, part, rf, err := s.rollForwarderForJournal(o)
	if err != nil {
		return JournalReportSummary{}, err
	}
	if _, err := rf.RollForward(func(filesys.RollForwardNode) bool { return false }); err != nil { // a pure read: a refusal audits nothing
		return JournalReportSummary{}, err
	}
	fsType := fsys.Info().Type
	var js JournalReportSummary
	details := map[string]any{
		"parent_id": s.Parent.ID, "partition": part.Index, "fs_type": fsType, "format": RollForwardReportFormat,
		"max_bytes": b.maxBytes,
	}
	sum, err := runAnalysis(s.Case, s.Parent.Source.DeviceID, "journal-report", details, func(a *analysis) error {
		defer func() {
			a.setEnd("chain_nodes", js.Nodes)
			a.setEnd("chain_stop", js.Stop)
			a.setEnd("chain_complete", js.Complete)
		}()
		if err := a.watchFS(fsys); err != nil {
			return err
		}
		d := s.baseDerivation(part, fsType)
		d.Recovery = &evidence.Recovery{
			Class: evidence.ClassJournalReport, Method: evidence.RollForwardMethod, Scope: "unallocated",
			Algorithm: evidence.AlgorithmRecover, Params: map[string]string{"max_bytes": strconv.FormatInt(b.maxBytes, 10)},
			Journal: &evidence.JournalRef{Region: "live"},
		}
		src := evidence.Source{Kind: evidence.KindReport, DeviceID: s.Parent.Source.DeviceID, Derived: d}
		rec, fillErr, err := s.capture(a, reportRel(part.Index, fsType), src, func(w io.Writer) error {
			chain, counts, cut, err := renderRollForwardReport(&ctxWriter{ctx: ctx, w: w}, part.Index, rf, b.maxBytes)
			js.Nodes, js.Files, js.PostCheckpoint, js.LiveUpdates, js.NotReplayed, js.WouldReplay =
				counts.Nodes, counts.Files, counts.PostCheckpoint, counts.LiveUpdates, counts.NotReplayed, counts.WouldReplay
			js.Complete, js.Stop = chain.Complete, chain.Stop
			d.Recovery.Journal.Seq, d.Recovery.Journal.Committed = uint32(chain.CheckpointVersion), counts.WouldReplay > 0
			if err != nil {
				return err
			}
			return reportCutError(cut, chain, b.maxBytes)
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

// reportCutError is the incomplete flag text of a rendering, nil when it is whole.
func reportCutError(cut bool, chain filesys.RollForwardChain, maxBytes int64) error {
	var msgs []string
	if cut {
		msgs = append(msgs, "report cut by max-bytes "+strconv.FormatInt(maxBytes, 10)+": later lines are not written")
	}
	if !chain.Complete {
		msgs = append(msgs, "roll-forward chain incomplete: stopped at "+chain.Stop+", later nodes are unknown")
	}
	if len(msgs) == 0 {
		return nil
	}
	return &reportCut{msg: strings.Join(msgs, "; ")}
}

// ctxWriter stops a long write when ctx ends.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

type reportCounts struct{ Nodes, Files, PostCheckpoint, LiveUpdates, NotReplayed, WouldReplay int }

type rfChainLine struct {
	Type              string   `json:"type"`
	Format            string   `json:"format"`
	Partition         int      `json:"partition"`
	CheckpointVersion uint64   `json:"checkpoint_version"`
	CheckpointPack    int      `json:"checkpoint_pack"`
	FooterVersion     uint64   `json:"footer_version"`
	CRCRecovery       bool     `json:"crc_recovery"`
	NoCRCRecovery     bool     `json:"nocrc_recovery"`
	Unmount           bool     `json:"unmount"`
	Start             uint64   `json:"start"`
	Nodes             int      `json:"nodes"`
	Stop              string   `json:"stop"`
	Complete          bool     `json:"complete"`
	Warnings          []string `json:"warnings"`
}

type rfInodeLine struct {
	Mode       uint32 `json:"mode"`
	Size       int64  `json:"size"`
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
	Links      uint32 `json:"links"`
	Pino       uint32 `json:"pino"`
	Flags      uint32 `json:"flags"`
	Inline     uint8  `json:"inline"`
	Name       string `json:"name"`
	NameRaw    string `json:"name_raw,omitempty"`
	Atime      string `json:"atime,omitempty"`
	Ctime      string `json:"ctime,omitempty"`
	Mtime      string `json:"mtime,omitempty"`
	Crtime     string `json:"crtime,omitempty"`
	Encrypted  bool   `json:"encrypted"`
	InlineData bool   `json:"inline_data"`
	Compressed bool   `json:"compressed"`
	Checksum   string `json:"checksum"`
	Error      string `json:"error,omitempty"`
}

type rfNodeLine struct {
	Type        string       `json:"type"`
	Index       int          `json:"index"`
	Block       uint64       `json:"block"`
	NID         uint32       `json:"nid"`
	Ino         uint32       `json:"ino"`
	Ofs         uint32       `json:"ofs"`
	Kind        string       `json:"kind"`
	Flag        uint32       `json:"flag"`
	Fsync       bool         `json:"fsync"`
	Dentry      bool         `json:"dentry"`
	Cold        bool         `json:"cold"`
	Next        uint64       `json:"next"`
	CPVer       uint64       `json:"cp_ver"`
	LiveIno     bool         `json:"live_ino"`
	WouldReplay bool         `json:"would_replay"`
	Addrs       [][2]uint32  `json:"addrs,omitempty"`
	NIDs        []uint32     `json:"nids,omitempty"`
	Inode       *rfInodeLine `json:"inode,omitempty"`
}

type rfFileLine struct {
	Type           string `json:"type"`
	Ino            uint32 `json:"ino"`
	Kind           string `json:"kind"`
	ID             string `json:"id,omitempty"`
	Name           string `json:"name,omitempty"`
	Pino           uint32 `json:"pino"`
	HasInode       bool   `json:"has_inode"`
	InodeBlock     uint64 `json:"inode_block"`
	LastFsyncBlock uint64 `json:"last_fsync_block"`
	Size           int64  `json:"size"`
	Caveat         string `json:"caveat,omitempty"` // additive: what is unknown or unusable about the entry
}

type rfEndLine struct {
	Type        string `json:"type"`
	Nodes       int    `json:"nodes"`
	Files       int    `json:"files"`
	PostChkpt   int    `json:"post_checkpoint"`
	LiveUpdates int    `json:"live_updates"`
	NotReplayed int    `json:"not_replayed"`
	WouldReplay int    `json:"would_replay"`
}

type rfCutLine struct {
	Type       string `json:"type"`
	Reason     string `json:"reason"`
	AfterNodes int    `json:"after_nodes"`
}

// addrRuns encodes address words as [first, count] runs: a run of non-zero words each one above the last,
// or a run of zeros ([0, count]). Decoding it gives back every word, so the encoding is lossless.
func addrRuns(words []uint32) [][2]uint32 {
	var runs [][2]uint32
	for _, w := range words {
		if n := len(runs); n > 0 {
			last := &runs[n-1]
			switch {
			case w == 0 && last[0] == 0:
				last[1]++
				continue
			case w != 0 && last[0] != 0 && uint64(last[0])+uint64(last[1]) == uint64(w):
				last[1]++
				continue
			}
		}
		runs = append(runs, [2]uint32{w, 1})
	}
	return runs
}

func reportTime(t filesys.Timestamp) string {
	if t.T.IsZero() {
		return ""
	}
	return t.T.UTC().Format(time.RFC3339Nano)
}

func inodeLine(in *filesys.RollForwardInode) *rfInodeLine {
	if in == nil {
		return nil
	}
	l := &rfInodeLine{
		Mode: in.Mode, Size: in.Size, UID: in.UID, GID: in.GID, Links: in.Links, Pino: in.Pino, Flags: in.Flags,
		Inline: in.Inline, Name: in.Name, Atime: reportTime(in.Times.Accessed), Ctime: reportTime(in.Times.Changed),
		Mtime: reportTime(in.Times.Modified), Crtime: reportTime(in.Times.Created), Encrypted: in.Encrypted,
		InlineData: in.InlineData, Compressed: in.Compressed, Checksum: in.Checksum, Error: in.Err,
	}
	if string(in.RawName) != in.Name {
		l.NameRaw = base64.RawURLEncoding.EncodeToString(in.RawName)
	}
	return l
}

// lineWriter writes report lines under a byte limit.
type lineWriter struct {
	w   *bufio.Writer
	n   int64
	max int64
	cut bool
	err error
}

// emit writes one line (newline added) unless it would pass the limit with the cut line's reserve; a refused
// line marks the report cut and nothing after it is written. force writes regardless (the first line and the
// cut line).
func (l *lineWriter) emit(v any, force bool) bool {
	if l.err != nil || (l.cut && !force) {
		return false
	}
	b, err := json.Marshal(v)
	if err != nil {
		l.err = err
		return false
	}
	b = append(b, '\n')
	if !force && l.n+int64(len(b))+reportCutReserve > l.max {
		l.cut = true
		return false
	}
	if _, err := l.w.Write(b); err != nil {
		l.err = err
		return false
	}
	l.n += int64(len(b))
	return true
}

// renderRollForwardReport streams the report of rf's chain to w, at most maxBytes (the first line is always
// written; a line that would pass the limit becomes a cut line and nothing after it is written). It is a pure
// function of the chain: the same chain and limit give the same bytes. cut reports a limit cut.
func renderRollForwardReport(w io.Writer, part int, rf filesys.RollForwarder, maxBytes int64) (filesys.RollForwardChain, reportCounts, bool, error) {
	chain, err := rf.RollForward(func(filesys.RollForwardNode) bool { return false })
	if err != nil {
		return chain, reportCounts{}, false, err
	}
	counts := reportCounts{Nodes: chain.Nodes, Files: len(chain.Files)}
	for _, f := range chain.Files {
		switch f.Kind {
		case "post-checkpoint":
			counts.PostCheckpoint++
		case "live-update":
			counts.LiveUpdates++
		case "not-replayed":
			counts.NotReplayed++
		}
	}
	lw := &lineWriter{w: bufio.NewWriterSize(w, 64<<10), max: maxBytes}
	lw.emit(rfChainLine{
		Type: "chain", Format: RollForwardReportFormat, Partition: part, CheckpointVersion: chain.CheckpointVersion,
		CheckpointPack: chain.CheckpointPack, FooterVersion: chain.FooterVersion, CRCRecovery: chain.CRCRecovery,
		NoCRCRecovery: chain.NoCRCRecovery, Unmount: chain.Unmount, Start: chain.Start, Nodes: chain.Nodes,
		Stop: chain.Stop, Complete: chain.Complete, Warnings: append([]string{}, chain.Warnings...),
	}, true)
	written := 0
	_, err = rf.RollForward(func(n filesys.RollForwardNode) bool {
		if n.WouldReplay {
			counts.WouldReplay++ // counted over the whole chain, written or not
		}
		if lw.emit(rfNodeLine{
			Type: "node", Index: n.Index, Block: n.Block, NID: n.NID, Ino: n.Ino, Ofs: n.Ofs, Kind: n.Kind, Flag: n.Flag,
			Fsync: n.Fsync, Dentry: n.Dentry, Cold: n.Cold, Next: n.Next, CPVer: n.CPVer, LiveIno: n.LiveIno,
			WouldReplay: n.WouldReplay, Addrs: addrRuns(n.Addrs), NIDs: slices.Clone(n.NIDs), Inode: inodeLine(n.Inode),
		}, false) {
			written++
		}
		return lw.err == nil
	})
	if err == nil {
		err = lw.err
	}
	if err != nil {
		return chain, counts, lw.cut, err
	}
	for _, f := range chain.Files {
		lw.emit(rfFileLine{
			Type: "file", Ino: f.Ino, Kind: f.Kind, ID: f.ID, Name: f.Name, Pino: f.Pino, HasInode: f.HasInode,
			InodeBlock: f.InodeBlock, LastFsyncBlock: f.LastFsyncBlock, Size: f.Size, Caveat: f.Caveat,
		}, false)
	}
	lw.emit(rfEndLine{
		Type: "end", Nodes: counts.Nodes, Files: counts.Files, PostChkpt: counts.PostCheckpoint,
		LiveUpdates: counts.LiveUpdates, NotReplayed: counts.NotReplayed, WouldReplay: counts.WouldReplay,
	}, false)
	if lw.cut {
		lw.emit(rfCutLine{Type: "cut", Reason: "max-bytes", AfterNodes: written}, true)
	}
	if lw.err == nil {
		lw.err = lw.w.Flush()
	}
	return chain, counts, lw.cut, lw.err
}
