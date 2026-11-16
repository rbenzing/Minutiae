package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// The ext4 side of `image journal` (3G Task 12). A jbd2 transaction is history: it was not replayed, and whether it
// was ever applied is unknown, so every output ends with journalCaution. An incomplete scan is never shown as an
// empty or whole journal: its INCOMPLETE line says later transactions are unknown.

// journalCaution follows every ext4 journal output.
const journalCaution = "journal transactions are history: they were not replayed and are not part of the live view"

// isJBD2 reports whether the listing is a jbd2 journal (a roll-forward filesystem has a chain).
func isJBD2(l examine.JournalListing) bool { return l.Chain == nil && l.Info.Type == "jbd2" }

// isJBD2Artifact reports whether a journal report artifact is the ext4 one.
func isJBD2Artifact(rec evidence.ManifestRecord) bool {
	d := rec.Source.Derived
	return d != nil && d.Recovery != nil && strings.HasPrefix(d.Recovery.Method, evidence.Ext4JournalMethod)
}

// printIncompleteJournal prints the INCOMPLETE line of a journal whose later transactions are unknown.
func printIncompleteJournal(w io.Writer, stop string) {
	fmt.Fprintf(w, "INCOMPLETE: %s; later transactions are unknown\n", escapeText(stop))
}

func jbd2Features(f []string) string {
	if len(f) == 0 {
		return "none"
	}
	esc := make([]string, len(f))
	for i, x := range f {
		esc[i] = escapeText(x)
	}
	return strings.Join(esc, ",")
}

func jbd2Committed(t filesys.JournalTxn) string {
	if t.Committed {
		return "committed"
	}
	return "uncommitted"
}

// printJBD2Txn prints the transaction line and the warnings of a transaction.
func printJBD2Txn(w io.Writer, t filesys.JournalTxn) {
	fmt.Fprintf(w, "transaction %d [%s] %s: %d blocks, %d revoke records, checksum %s\n",
		t.Seq, escapeText(t.Region), jbd2Committed(t), len(t.Blocks), len(t.Revoked), escapeText(t.Checksum))
	for _, wn := range t.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", escapeText(wn))
	}
}

type jsonJBD2Txn struct {
	Seq           uint32   `json:"seq"`
	Region        string   `json:"region"`
	Committed     bool     `json:"committed"`
	Blocks        int      `json:"blocks"`
	RevokeRecords int      `json:"revoke_records"`
	Checksum      string   `json:"checksum"`
	StartBlock    int64    `json:"start_block"`
	CommitBlock   int64    `json:"commit_block"`
	Warnings      []string `json:"warnings"`
}

func newJSONJBD2Txn(t filesys.JournalTxn) jsonJBD2Txn {
	return jsonJBD2Txn{
		Seq: t.Seq, Region: t.Region, Committed: t.Committed, Blocks: len(t.Blocks), RevokeRecords: len(t.Revoked),
		Checksum: t.Checksum, StartBlock: t.StartBlock, CommitBlock: t.CommitBlock, Warnings: nonNil(t.Warnings),
	}
}

type jsonJBD2Journal struct {
	Version       int      `json:"version"`
	BlockSize     int      `json:"block_size"`
	Blocks        int64    `json:"blocks"`
	Features      []string `json:"features"`
	Sequence      uint32   `json:"sequence"`
	Start         uint32   `json:"start"`
	Clean         bool     `json:"clean"`
	NeedsRecovery bool     `json:"needs_recovery"`
	Complete      bool     `json:"complete"`
	Incomplete    bool     `json:"incomplete"`
	Stop          string   `json:"stop"`
	Warnings      []string `json:"warnings"`
}

type jsonJBD2Ls struct {
	Partition    int             `json:"partition"`
	FSType       string          `json:"fs_type"`
	Type         string          `json:"type"`
	Journal      jsonJBD2Journal `json:"journal"`
	NoStale      bool            `json:"no_stale"`
	StaleOmitted int             `json:"stale_omitted"`
	Transactions []jsonJBD2Txn   `json:"transactions"`
}

func newJSONJBD2Journal(info filesys.JournalInfo) jsonJBD2Journal {
	return jsonJBD2Journal{
		Version: info.Version, BlockSize: info.BlockSize, Blocks: info.Blocks, Features: nonNil(info.Features), Sequence: info.Sequence,
		Start: info.Start, Clean: info.Start == 0, NeedsRecovery: info.NeedsRecovery, Complete: !info.Incomplete, Incomplete: info.Incomplete,
		Stop: info.Stop, Warnings: nonNil(info.Warnings),
	}
}

// listJBD2 prints (or writes as JSON) the ext4 journal listing.
func listJBD2(w io.Writer, asJSON bool, part int, l examine.JournalListing, noStale bool) error {
	if asJSON {
		out := jsonJBD2Ls{
			Partition: part, FSType: l.FSType, Type: l.Info.Type, Journal: newJSONJBD2Journal(l.Info),
			NoStale: noStale, StaleOmitted: l.StaleOmitted, Transactions: []jsonJBD2Txn{},
		}
		for _, t := range l.Txns {
			out.Transactions = append(out.Transactions, newJSONJBD2Txn(t))
		}
		return writeJSON(w, out)
	}
	i := l.Info
	start := "clean"
	if i.Start != 0 {
		start = fmt.Sprint(i.Start)
	}
	fmt.Fprintf(w, "jbd2 journal of partition %d (v%d, %d-byte blocks, %d blocks, features %s): sequence %d, start %s\n",
		part, i.Version, i.BlockSize, i.Blocks, jbd2Features(i.Features), i.Sequence, start)
	fmt.Fprintf(w, "needs_recovery: %s\n", yesNo(i.NeedsRecovery))
	if len(l.Txns) == 0 && l.StaleOmitted == 0 {
		if i.Incomplete {
			fmt.Fprintf(w, "(no transaction was read before the scan stopped at %s; later transactions are unknown, so the journal is not shown empty)\n", escapeText(i.Stop))
		} else {
			fmt.Fprintln(w, "(empty: the journal holds no transaction)")
		}
	}
	for _, t := range l.Txns {
		printJBD2Txn(w, t)
	}
	if l.StaleOmitted > 0 {
		fmt.Fprintf(w, "(%d stale transactions not shown)\n", l.StaleOmitted)
	}
	if i.Incomplete {
		printIncompleteJournal(w, i.Stop)
	}
	for _, wn := range i.Warnings {
		fmt.Fprintf(w, "warning: %s\n", escapeText(wn))
	}
	fmt.Fprintln(w, journalCaution)
	return nil
}

type jsonJBD2Tag struct {
	Index        int    `json:"index"`
	FSBlock      uint64 `json:"fsblock"`
	JournalBlock uint64 `json:"jblk"`
	Copy         string `json:"copy"`
	Escaped      bool   `json:"escaped"`
	Revoked      bool   `json:"revoked"`
}

type jsonJBD2Show struct {
	jsonJBD2Txn
	Tags       []jsonJBD2Tag `json:"tags"`
	RevokedFS  []uint64      `json:"revoked"`
	Complete   bool          `json:"complete"`
	Incomplete bool          `json:"incomplete"`
	Stop       string        `json:"stop"`
}

// showJBD2 prints the transaction(s) named seq: the transaction line, one line per tag and the revoked blocks. A seq
// that names several transactions prints their lines and fails with the error that names every start block.
func showJBD2(w io.Writer, asJSON bool, s *examine.Session, jo examine.JournalOptions, seq uint32, region string, wrap func(error) error) error {
	txns, err := s.JournalTxns(jo, seq, region)
	if err != nil && !errors.Is(err, filesys.ErrAmbiguous) {
		return wrap(err)
	}
	if err != nil { // ambiguous: say which, name no tag
		if !asJSON {
			for _, t := range txns {
				printJBD2Txn(w, t)
			}
			fmt.Fprintln(w, journalCaution)
		}
		return err
	}
	l, lerr := s.JournalList(jo)
	if lerr != nil {
		return wrap(lerr)
	}
	t := txns[0]
	if asJSON {
		out := jsonJBD2Show{jsonJBD2Txn: newJSONJBD2Txn(t), Tags: []jsonJBD2Tag{}, RevokedFS: append([]uint64{}, t.Revoked...), Complete: !l.Info.Incomplete, Incomplete: l.Info.Incomplete, Stop: l.Info.Stop}
		for i, g := range t.Blocks {
			out.Tags = append(out.Tags, jsonJBD2Tag{Index: i, FSBlock: g.FSBlock, JournalBlock: g.JournalBlock, Copy: g.Copy, Escaped: g.Escaped, Revoked: g.Revoked})
		}
		return writeJSON(w, out)
	}
	printJBD2Txn(w, t)
	for i, g := range t.Blocks {
		line := fmt.Sprintf("#%d fsblock %d jblk %d copy %s", i, g.FSBlock, g.JournalBlock, escapeText(g.Copy))
		if g.Escaped {
			line += " escaped"
		}
		if g.Revoked {
			line += " revoked"
		}
		fmt.Fprintln(w, line)
	}
	if len(t.Revoked) > 0 {
		list := make([]string, len(t.Revoked))
		for i, b := range t.Revoked {
			list[i] = fmt.Sprint(b)
		}
		fmt.Fprintf(w, "revoked: %s\n", strings.Join(list, ", "))
	}
	if l.Info.Incomplete {
		printIncompleteJournal(w, l.Info.Stop)
	}
	fmt.Fprintln(w, journalCaution)
	return nil
}

type jsonJBD2Report struct {
	AnalysisID       string `json:"analysis_id"`
	Artifact         string `json:"artifact"`
	Path             string `json:"path"`
	Transactions     int    `json:"transactions"`
	LiveTransactions int    `json:"live_transactions"`
	StaleTxns        int    `json:"stale_transactions"`
	NoStale          bool   `json:"no_stale"`
	Tags             int    `json:"tags"`
	Committed        int    `json:"committed"`
	RevokeRecords    int    `json:"revoke_records"`
	Cut              bool   `json:"cut"`
	Complete         bool   `json:"complete"`
	Stop             string `json:"stop"`
	Incomplete       bool   `json:"incomplete"`
	IncompleteReason string `json:"incomplete_reason"`
}

// reportJBD2 prints the outcome of the ext4 journal report. The artifact is kept and flagged incomplete when the
// journal scan was cut or the report was; either way the command exits 0 unless err says otherwise.
func reportJBD2(w io.Writer, asJSON bool, sum examine.JournalReportSummary, rec evidence.ManifestRecord, noStale bool, err error) error {
	if asJSON {
		return joinJSON(err, writeJSON(w, jsonJBD2Report{
			AnalysisID: sum.AnalysisID, Artifact: rec.ID, Path: rec.Path, Transactions: sum.Txns, LiveTransactions: sum.LiveTxns,
			StaleTxns: sum.StaleTxns, NoStale: noStale, Tags: sum.Tags, Committed: sum.Committed, RevokeRecords: sum.RevokeRecords,
			Cut: sum.Cut, Complete: sum.Complete, Stop: sum.Stop, Incomplete: rec.Incomplete, IncompleteReason: rec.Error,
		}))
	}
	fmt.Fprintf(w, "report %s %s: %d transactions (%d live, %d stale), %d tags\n", escapeText(rec.ID), escapeText(rec.Path), sum.Txns, sum.LiveTxns, sum.StaleTxns, sum.Tags)
	if noStale {
		fmt.Fprintln(w, "(--no-stale: the stale count is 0 because those transactions were left out, not because there are none)")
	}
	if !sum.Complete {
		printIncompleteJournal(w, sum.Stop)
	}
	if sum.Cut {
		fmt.Fprintf(w, "INCOMPLETE: the report is cut: %s\n", escapeText(rec.Error))
	}
	fmt.Fprintln(w, journalCaution)
	return err
}

type jsonJBD2Block struct {
	FSBlock  uint64 `json:"fsblock"`
	Region   string `json:"region"`
	Artifact string `json:"artifact"`
	Path     string `json:"path"`
}

type jsonJBD2Blocks struct {
	AnalysisID   string          `json:"analysis_id"`
	Seq          uint32          `json:"seq"`
	Copied       int             `json:"copied"`
	CopyMismatch int             `json:"copy_mismatch"`
	Missing      int             `json:"missing"`
	NotAllocated int             `json:"not_allocated"`
	AllocUnknown int             `json:"alloc_unknown"`
	Complete     bool            `json:"complete"`
	Incomplete   bool            `json:"incomplete"`
	Stop         string          `json:"stop"`
	Blocks       []jsonJBD2Block `json:"blocks"`
}

// blocksJBD2 prints the outcome of the ext4 block copies. Copies whose bitmap view was unknown are counted apart
// (they carry the bitmap-unconfirmed assumption), never folded into the plain count.
func blocksJBD2(w io.Writer, asJSON bool, seq uint32, l examine.JournalListing, sum examine.JournalBlocksSummary, err error) error {
	res := jsonJBD2Blocks{
		AnalysisID: sum.AnalysisID, Seq: seq, Copied: sum.Copied, CopyMismatch: sum.CopyMismatch, Missing: sum.Missing,
		NotAllocated: sum.NotAllocated, AllocUnknown: sum.AllocUnknown, Complete: !l.Info.Incomplete, Incomplete: l.Info.Incomplete,
		Stop: l.Info.Stop, Blocks: []jsonJBD2Block{},
	}
	for _, a := range sum.Artifacts {
		d := a.Source.Derived
		if a.Source.Kind != evidence.KindJournal || d == nil || d.Recovery == nil {
			continue
		}
		blk, _ := basisUint(d.Recovery.Basis, "fsblk")
		region := ""
		if d.Recovery.Journal != nil {
			region = d.Recovery.Journal.Region
		}
		res.Blocks = append(res.Blocks, jsonJBD2Block{FSBlock: blk, Region: region, Artifact: a.ID, Path: a.Path})
	}
	if asJSON {
		return joinJSON(err, writeJSON(w, res))
	}
	for _, b := range res.Blocks {
		fmt.Fprintf(w, "[journal-block:%s seq %d %s] fsblock %d -> %s %s\n", evidence.Ext4JournalMethod, seq, escapeText(b.Region), b.FSBlock, escapeText(b.Artifact), escapeText(b.Path))
	}
	fmt.Fprintf(w, "copied %d\nskipped copy-mismatch: %d, missing: %d, not-allocated: %d\nbitmap unconfirmed: %d\n",
		sum.Copied, sum.CopyMismatch, sum.Missing, sum.NotAllocated, sum.AllocUnknown)
	if l.Info.Incomplete {
		printIncompleteJournal(w, l.Info.Stop)
	}
	fmt.Fprintln(w, journalCaution)
	return err
}
