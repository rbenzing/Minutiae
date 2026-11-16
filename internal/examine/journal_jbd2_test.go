package examine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// The ext4 journal listing, report, block copies and transaction view (3G Task 10), on builder images whose journal
// holds live and stale transactions. The layout of the main image (64 journal blocks, 1 KiB, csum v3, s_first 1):
//
//	seq 10  block 1..4    stale, committed; tag 101's slot was overwritten by a later lap (mismatch)
//	seq 11  block 5..8    live, committed;   tag 103 is escaped
//	seq 12  block 9..10   live, uncommitted
//	seq 11  block 30..32  stale, committed (the same sequence as the live one)

const jbBS = 1024

var jbUUID = [16]byte{0xB1, 0xC2, 0xD3, 0xE4, 0xF5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func jbFill(n byte) []byte { return bytes.Repeat([]byte{n}, jbBS) }

func jbTxn(seq uint32, at int, fsblks ...uint64) ext4test.JTxn {
	t := ext4test.JTxn{Seq: seq, At: at, CommitSec: 1700000000 + uint64(seq)}
	for i, b := range fsblks {
		t.Tags = append(t.Tags, ext4test.JTag{FSBlock: b, Data: jbFill(byte(seq)*16 + byte(i) + 1)})
	}
	return t
}

// jbHeader is a journal header block of type typ and sequence seq.
func jbHeader(typ, seq uint32) []byte {
	b := make([]byte, jbBS)
	binary.BigEndian.PutUint32(b[0:], ext4test.JBD2Magic)
	binary.BigEndian.PutUint32(b[4:], typ)
	binary.BigEndian.PutUint32(b[8:], seq)
	return b
}

func jbMainJournal() ext4test.JBD2 {
	stale10 := jbTxn(10, 1, 100, 101)
	stale10.Tags[1].Overwrite = []byte("a later lap wrote here")
	live11 := jbTxn(11, 5, 102, 103)
	live11.Tags[1].Data = append(binary.BigEndian.AppendUint32(nil, ext4test.JBD2Magic), jbFill(0x77)[4:]...)
	live12 := jbTxn(12, 9, 104)
	live12.NoCommit = true
	return ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 11, Start: 5, UUID: jbUUID,
		Txns: []ext4test.JTxn{stale10, live11, live12, jbTxn(11, 30, 105)},
	}
}

type jbImg struct {
	img []byte
	l   *ext4test.Layout
}

func jbBuild(t *testing.T, j ext4test.JBD2, edit func(img []byte, l *ext4test.Layout)) jbImg {
	t.Helper()
	o := ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, UUID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, JournalBlocks: 64, BlocksPerGroup: 2048}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a.txt", Data: pat(1, 3000)}})
	l.WriteJournal(img, j)
	if j.Start != 0 {
		l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	}
	if edit != nil {
		edit(img, l)
	}
	return jbImg{img: img, l: l}
}

func jbSession(t *testing.T, j jbImg) (*examine.Session, []byte, int64) {
	t.Helper()
	s, disk := imageSession(t, j.img)
	part, err := s.Partition(-1)
	if err != nil {
		t.Fatal(err)
	}
	return s, disk, part.Start
}

func jbMain(t *testing.T) (*examine.Session, jbImg, []byte, int64) {
	t.Helper()
	j := jbBuild(t, jbMainJournal(), nil)
	s, disk, start := jbSession(t, j)
	return s, j, disk, start
}

// jbTxnsOf lists the transactions through the plain filesystem interface: the oracle of the examine view.
func jbTxnsOf(t *testing.T, s *examine.Session) []filesys.JournalTxn {
	t.Helper()
	fsys, _, err := s.FS(-1)
	if err != nil {
		t.Fatal(err)
	}
	j, ok := filesys.As[filesys.Journaler](fsys)
	if !ok {
		t.Fatal("no journaler")
	}
	var out []filesys.JournalTxn
	if err := j.JournalTransactions(func(x filesys.JournalTxn) bool { out = append(out, x); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func jbReport(t *testing.T, s *examine.Session, o examine.JournalOptions, maxBytes int64) (examine.JournalReportSummary, evidence.ManifestRecord) {
	t.Helper()
	sum, err := s.JournalReport(context.Background(), o, maxBytes)
	if err != nil {
		t.Fatalf("JournalReport: %v", err)
	}
	if len(sum.Artifacts) != 1 {
		t.Fatalf("%d artifacts, want 1", len(sum.Artifacts))
	}
	return sum, sum.Artifacts[0]
}

func TestJournalListOfExt4(t *testing.T) {
	s, _, _, _ := jbMain(t)
	beforeTree := hashTree(t, s.Case.Dir)
	beforeAudit := len(auditEntries(t, s.Case))
	l, err := s.JournalList(jo)
	if err != nil {
		t.Fatal(err)
	}
	if l.FSType != "ext4" || l.Info.Type != "jbd2" || l.Chain != nil || !l.Info.NeedsRecovery || l.Info.Sequence != 11 || l.Info.Start != 5 || l.Info.First != 1 {
		t.Fatalf("listing %+v", l)
	}
	want := jbTxnsOf(t, s)
	if len(l.Txns) != len(want) || len(want) < 4 {
		t.Fatalf("%d transactions, the reader has %d", len(l.Txns), len(want))
	}
	regions := map[string]int{}
	for i, x := range l.Txns {
		regions[x.Region]++
		if x.Seq != want[i].Seq || x.Region != want[i].Region || x.StartBlock != want[i].StartBlock || x.Committed != want[i].Committed || len(x.Blocks) != len(want[i].Blocks) {
			t.Errorf("transaction %d = %+v, reader %+v", i, x, want[i])
		}
	}
	if regions["live"] != 2 || regions["stale"] < 2 || l.StaleOmitted != 0 {
		t.Errorf("regions %v, omitted %d", regions, l.StaleOmitted)
	}
	if got := len(auditEntries(t, s.Case)); got != beforeAudit {
		t.Errorf("the audit log gained %d entries", got-beforeAudit)
	}
	after := hashTree(t, s.Case.Dir)
	if len(after) != len(beforeTree) {
		t.Errorf("case files %d -> %d", len(beforeTree), len(after))
	}
	for k, v := range beforeTree {
		if after[k] != v {
			t.Errorf("case file %s changed", k)
		}
	}
}

func TestJournalListNoStale(t *testing.T) {
	s, _, _, _ := jbMain(t)
	all, err := s.JournalList(jo)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.JournalList(examine.JournalOptions{Partition: -1, NoStale: true})
	if err != nil {
		t.Fatal(err)
	}
	stale := 0
	for _, x := range all.Txns {
		if x.Region == "stale" {
			stale++
		}
	}
	if len(l.Txns) != 2 || l.StaleOmitted != stale || stale == 0 {
		t.Fatalf("%d transactions, %d omitted; want the 2 live ones and %d omitted", len(l.Txns), l.StaleOmitted, stale)
	}
	for _, x := range l.Txns {
		if x.Region != "live" {
			t.Errorf("a %s transaction in a --no-stale listing", x.Region)
		}
	}
}

func TestJournalTxnsAmbiguousSeq(t *testing.T) {
	s, _, _, _ := jbMain(t)
	// seq 11 is both the live transaction at block 5 and a stale one at block 30
	got, err := s.JournalTxns(jo, 11, "")
	if !errors.Is(err, filesys.ErrAmbiguous) || len(got) != 2 {
		t.Fatalf("no region: %d transactions, err %v; want ErrAmbiguous with both", len(got), err)
	}
	for _, blk := range []string{"5", "30"} {
		if !strings.Contains(err.Error(), blk) {
			t.Errorf("error %q does not name start block %s", err, blk)
		}
	}
	for region, start := range map[string]int64{"live": 5, "stale": 30} {
		got, err := s.JournalTxns(jo, 11, region)
		if err != nil || len(got) != 1 || got[0].Region != region || got[0].StartBlock != start {
			t.Errorf("region %s: %+v, %v", region, got, err)
		}
	}
	if _, err := s.JournalTxns(jo, 999, ""); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown seq: %v, want ErrNotFound", err)
	}
	if _, err := s.JournalTxns(jo, 12, "stale"); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("seq 12 has no stale transaction: %v, want ErrNotFound", err)
	}
	if got := len(auditByAction(t, s.Case, "analysis.start")); got != 0 {
		t.Errorf("a read audited %d analysis.start entries", got)
	}
}

// A seq that an incomplete journal does not hold is unknown, never absent (the 3H rule).
func TestJournalTxnsOnIncompleteJournalIsIncomplete(t *testing.T) {
	j := jbMainJournal()
	j.FastCommit, j.NumFC = true, 8
	s, _, _ := jbSession(t, jbBuild(t, j, nil))
	_, err := s.JournalTxns(jo, 999, "")
	if !errors.Is(err, filesys.ErrIncomplete) || errors.Is(err, filesys.ErrNotFound) || !strings.Contains(err.Error(), "fast-commit-not-analysed") {
		t.Errorf("err = %v, want ErrIncomplete naming the stop", err)
	}
}

func jbLines(t *testing.T, s *examine.Session, rec evidence.ManifestRecord) []map[string]any {
	t.Helper()
	return jrnLines(t, readArtifact(t, s.Case, rec))
}

func TestJBD2ReportWritesOneReport(t *testing.T) {
	s, j, _, _ := jbMain(t)
	want := jbTxnsOf(t, s)
	sum, rec := jbReport(t, s, jo, 0)
	rv, d := recoveryOf(rec), rec.Source.Derived
	if rec.Source.Kind != evidence.KindReport || rv.Class != evidence.ClassJournalReport || rv.Method != "ext4-journal" || rv.Scope != "journal" || rv.Confidence != nil {
		t.Errorf("kind %q recovery %+v", rec.Source.Kind, rv)
	}
	if len(d.Runs) != 0 || d.RunsArtifact != "" || rv.Journal != nil || rec.Incomplete {
		t.Errorf("a report has no runs and no single transaction: %+v, journal %+v, incomplete %v", d, rv.Journal, rec.Incomplete)
	}
	if !strings.HasSuffix(rec.Path, "reports/p1-ext4/journal.jsonl") {
		t.Errorf("path %q", rec.Path)
	}
	if rv.Params["format"] != examine.JBD2ReportFormat || rv.Params["max_bytes"] != fmt.Sprint(examine.DefaultMaxBytes) || rv.Params["stale"] != "on" {
		t.Errorf("params %v", rv.Params)
	}
	raw := readArtifact(t, s.Case, rec)
	if !bytes.HasPrefix(raw, []byte(`{"type":"journal","format":"minutiae-ext4-journal/1","partition":1,"version":2,"block_size":1024,"blocks":64,"first":1,"sequence":11,"start":5,"features":`)) {
		t.Errorf("journal line / field order: %.200s", raw)
	}
	lines := jrnLines(t, raw)
	var txnLines, tagLines []map[string]any
	for _, l := range lines {
		switch l["type"] {
		case "txn":
			txnLines = append(txnLines, l)
		case "tag":
			tagLines = append(tagLines, l)
		}
	}
	tags := 0
	for _, x := range want {
		tags += len(x.Blocks)
	}
	if len(txnLines) != len(want) || len(tagLines) != tags {
		t.Fatalf("%d txn lines (want %d), %d tag lines (want %d)", len(txnLines), len(want), len(tagLines), tags)
	}
	k := 0
	for i, x := range want {
		g := txnLines[i]
		commit := float64(x.CommitBlock)
		if x.CommitBlock <= 0 {
			commit = -1
		}
		if uint32(g["seq"].(float64)) != x.Seq || g["region"] != x.Region || g["committed"] != x.Committed || int64(g["start_jblk"].(float64)) != x.StartBlock ||
			g["commit_jblk"].(float64) != commit || g["checksum"] != x.Checksum || int(g["tags"].(float64)) != len(x.Blocks) || g["applied"] != "unknown" {
			t.Errorf("txn line %d = %v, transaction %+v", i, g, x)
		}
		for ti, tg := range x.Blocks {
			l := tagLines[k]
			k++
			loc := float64(-1)
			if tg.Copy != "missing" && tg.Copy != "outside" {
				loc = float64(j.l.JournalFSBlock(int(tg.JournalBlock)) * jbBS)
			}
			if uint32(l["seq"].(float64)) != x.Seq || l["region"] != x.Region || int64(l["txn_start"].(float64)) != x.StartBlock || int(l["index"].(float64)) != ti ||
				uint64(l["fsblock"].(float64)) != tg.FSBlock || uint64(l["jblk"].(float64)) != tg.JournalBlock || l["fs_location"].(float64) != loc ||
				uint32(l["flags"].(float64)) != tg.Flags || l["escaped"] != tg.Escaped || l["copy"] != tg.Copy || l["revoked"] != tg.Revoked {
				t.Errorf("tag line %d of txn %d = %v, tag %+v (location %v)", ti, i, l, tg, loc)
			}
			if (tg.Flags&ext4test.JTagLast != 0) != l["last"].(bool) || (tg.Flags&ext4test.JTagSameUUID != 0) != l["same_uuid"].(bool) {
				t.Errorf("tag line %v: flag fields do not follow flags %#x", l, tg.Flags)
			}
		}
	}
	first, end := lines[0], lines[len(lines)-1]
	if first["complete"] != true || first["live_txns"].(float64) != 2 || int(first["stale_txns"].(float64)) != len(want)-2 ||
		!strings.Contains(fmt.Sprint(first["caution"]), "were not replayed") {
		t.Errorf("journal line %v", first)
	}
	if end["type"] != "end" || int(end["txns"].(float64)) != len(want) || int(end["tags"].(float64)) != tags || end["live"].(float64) != 2 {
		t.Errorf("end line %v", end)
	}
	if sum.Txns != len(want) || sum.LiveTxns != 2 || sum.Tags != tags || !sum.Complete || sum.Path != rec.Path || sum.Summary.Files != 1 || sum.Cut {
		t.Errorf("summary %+v", sum)
	}
	if starts := auditByAction(t, s.Case, "analysis.start"); len(starts) != 1 {
		t.Errorf("%d analysis.start entries", len(starts))
	}
	verifyOK(t, s.Case)
}

func TestJBD2ReportDeterministic(t *testing.T) {
	var out [2][]byte
	for i := range out {
		s, _, _, _ := jbMain(t)
		_, rec := jbReport(t, s, jo, 0)
		out[i] = readArtifact(t, s.Case, rec)
	}
	if !bytes.Equal(out[0], out[1]) || len(out[0]) == 0 {
		t.Errorf("two runs differ (%d and %d bytes)", len(out[0]), len(out[1]))
	}
}

func TestJBD2ReportIncompleteFlagged(t *testing.T) {
	j := jbMainJournal()
	j.FastCommit, j.NumFC = true, 8
	s, _, _ := jbSession(t, jbBuild(t, j, nil))
	sum, rec := jbReport(t, s, jo, 0)
	if !rec.Incomplete || !strings.Contains(rec.Error, "fast-commit-not-analysed") || sum.Complete || sum.Stop != "fast-commit-not-analysed" || sum.Txns == 0 {
		t.Errorf("incomplete %v error %q summary %+v; want a report flagged for the fast-commit area", rec.Incomplete, rec.Error, sum)
	}
	if first := jbLines(t, s, rec)[0]; first["complete"] != false || first["stop"] != "fast-commit-not-analysed" {
		t.Errorf("journal line %v", first)
	}
	verifyOK(t, s.Case)
}

func TestJBD2ReportMaxBytesCut(t *testing.T) {
	s, _, _, _ := jbMain(t)
	all := len(jbTxnsOf(t, s))
	if _, err := s.JournalReport(context.Background(), jo, -1); !errors.Is(err, examine.ErrInvalidLimit) {
		t.Errorf("negative limit: %v", err)
	}
	if got := len(auditByAction(t, s.Case, "analysis.start")); got != 0 {
		t.Errorf("a refused limit audited %d analysis.start entries", got)
	}
	const limit = 1400
	sum, rec := jbReport(t, s, jo, limit)
	raw := readArtifact(t, s.Case, rec)
	lines := jrnLines(t, raw)
	last := lines[len(lines)-1]
	if last["type"] != "cut" || last["reason"] != "max-bytes" || !rec.Incomplete || !strings.Contains(rec.Error, "max-bytes") || !sum.Cut {
		t.Fatalf("last line %v, incomplete %v, error %q", last, rec.Incomplete, rec.Error)
	}
	if len(raw) > limit {
		t.Errorf("the report is %d bytes, over its limit %d", len(raw), limit)
	}
	if after := int(last["after_txns"].(float64)); after >= all {
		t.Errorf("after_txns %d of %d transactions", after, all)
	}
	if rv := recoveryOf(rec); rv.Params["max_bytes"] != fmt.Sprint(limit) {
		t.Errorf("params %v: the limit must be recorded so verify can render the same cut", rv.Params)
	}
	verifyOK(t, s.Case)
}

func TestJBD2ReportNoStaleParam(t *testing.T) {
	s, _, _, _ := jbMain(t)
	sum, rec := jbReport(t, s, examine.JournalOptions{Partition: -1, NoStale: true}, 0)
	if rv := recoveryOf(rec); rv.Params["stale"] != "off" {
		t.Errorf("params %v, want stale off", rv.Params)
	}
	lines := jbLines(t, s, rec)
	for _, l := range lines {
		if l["region"] == "stale" {
			t.Errorf("a stale line in a --no-stale report: %v", l)
		}
	}
	if sum.StaleTxns != 0 || sum.LiveTxns != 2 || rec.Incomplete {
		t.Errorf("summary %+v incomplete %v", sum, rec.Incomplete)
	}
	if first := fmt.Sprint(lines[0]["warnings"]); !strings.Contains(first, "stale transactions are not listed") {
		t.Errorf("journal line warnings %s: a stale count of 0 must say that stale transactions were left out", first)
	}
	verifyOK(t, s.Case)
}

// jbCopyChecks compares a block copy with its log slot.
func jbCopyChecks(t *testing.T, s *examine.Session, rec evidence.ManifestRecord, disk []byte, j jbImg, start int64, x filesys.JournalTxn, tag int) {
	t.Helper()
	tg := x.Blocks[tag]
	off := start + int64(j.l.JournalFSBlock(int(tg.JournalBlock)))*jbBS
	rv, d := recoveryOf(rec), rec.Source.Derived
	if rec.Source.Kind != evidence.KindJournal || rv.Class != evidence.ClassJournalBlock || rv.Method != "ext4-journal" || rv.Scope != "journal" ||
		rv.Confidence != nil || rv.Alloc != (evidence.AllocSummary{Allocated: jbBS}) || rec.Incomplete {
		t.Errorf("kind %q recovery %+v incomplete %v", rec.Source.Kind, rv, rec.Incomplete)
	}
	if want := (evidence.JournalRef{Seq: x.Seq, Committed: x.Committed, Region: x.Region, Revoked: tg.Revoked}); rv.Journal == nil || *rv.Journal != want {
		t.Errorf("journal ref %+v, want %+v", rv.Journal, want)
	}
	if len(d.Runs) != 1 || d.Runs[0] != (evidence.Run{Offset: off, Length: jbBS}) {
		t.Errorf("runs %v, want [{%d %d}]", d.Runs, off, jbBS)
	}
	if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, disk[off:off+jbBS]) {
		t.Error("the copy differs from the log slot")
	}
	wantBasis := []string{
		fmt.Sprintf("jbd2:seq=%d", x.Seq), "region=" + x.Region, fmt.Sprintf("txn-start=%d", x.StartBlock),
		fmt.Sprintf("tag=%d", tag), fmt.Sprintf("fsblk=%d", tg.FSBlock), fmt.Sprintf("jblk=%d", tg.JournalBlock),
	}
	if !slices.Equal(rv.Basis, wantBasis) {
		t.Errorf("basis %v, want %v", rv.Basis, wantBasis)
	}
	if want := fmt.Sprintf("journal/p1-ext4/seq%d-fsblk%d.bin", x.Seq, tg.FSBlock); !strings.HasSuffix(rec.Path, want) {
		t.Errorf("path %q, want ...%s", rec.Path, want)
	}
}

func TestJournalBlocksCopiesExt4Tags(t *testing.T) {
	s, j, disk, start := jbMain(t)
	before := sha256.Sum256(j.img)
	txns, err := s.JournalTxns(jo, 11, "live")
	if err != nil || len(txns) != 1 {
		t.Fatalf("%v, %v", txns, err)
	}
	x := txns[0]
	sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11, Region: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Artifacts) != 2 || sum.Copied != 2 || sum.Files != 2 || sum.CopyMismatch != 0 || sum.Missing != 0 || sum.NotAllocated != 0 {
		t.Fatalf("%d artifacts, summary %+v", len(sum.Artifacts), sum)
	}
	for i, rec := range sum.Artifacts {
		jbCopyChecks(t, s, rec, disk, j, start, x, i)
	}
	verifyOK(t, s.Case)
	if sha256.Sum256(j.img) != before {
		t.Error("the source image changed")
	}
}

func TestJournalBlocksSkipsMismatchAndMissing(t *testing.T) {
	// seq 10 of the main image: tag 100 verified, tag 101 mismatch
	s, _, _, _ := jbMain(t)
	sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 10})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || sum.CopyMismatch != 1 || sum.Missing != 0 || sum.Skipped != 1 || len(sum.Artifacts) != 1 || !strings.Contains(fmt.Sprint(sum.Warnings), "copy-mismatch") {
		t.Errorf("mismatch: %+v", sum)
	}
	if rv := recoveryOf(sum.Artifacts[0]); !slices.Contains(rv.Basis, "fsblk=100") {
		t.Errorf("the copied tag is %v, want fsblk 100", rv.Basis)
	}
	verifyOK(t, s.Case)

	// a stale fragment whose second slot holds a foreign header: the tags from it on are missing
	tx := jbTxn(7, 1, 100, 101, 102)
	tx.Tags[1].Overwrite = jbHeader(2, 99)
	tx.NoCommit = true // no orphan commit block: the fragment is the only transaction of seq 7
	s2, _, _ := jbSession(t, jbBuild(t, ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 30, UUID: jbUUID, Txns: []ext4test.JTxn{tx}}, nil))
	got, err := s2.JournalTxns(jo, 7, "")
	if err != nil || len(got) < 1 {
		t.Fatalf("%v %v", got, err)
	}
	wantStates := []string{"verified", "missing", "missing"}
	for i, tg := range got[0].Blocks {
		if tg.Copy != wantStates[i] {
			t.Fatalf("tag %d copy %q, want %q", i, tg.Copy, wantStates[i])
		}
	}
	sum2, err := s2.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 7})
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Copied != 1 || sum2.Missing != 2 || sum2.CopyMismatch != 0 || sum2.Skipped != 2 || !strings.Contains(fmt.Sprint(sum2.Warnings), "missing") {
		t.Errorf("missing: %+v", sum2)
	}
	verifyOK(t, s2.Case)
}

func TestJournalBlocksEscapedKeepsZeros(t *testing.T) {
	s, j, disk, start := jbMain(t)
	sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11, Region: "live"})
	if err != nil || len(sum.Artifacts) != 2 {
		t.Fatalf("%+v, %v", sum, err)
	}
	esc, plain := sum.Artifacts[1], sum.Artifacts[0]
	if rv := recoveryOf(esc); !slices.Contains(rv.Assumptions, "jbd2-escaped") {
		t.Errorf("escaped copy assumptions %v, want jbd2-escaped", rv.Assumptions)
	}
	if rv := recoveryOf(plain); slices.Contains(rv.Assumptions, "jbd2-escaped") {
		t.Errorf("plain copy assumptions %v", rv.Assumptions)
	}
	got := readArtifact(t, s.Case, esc)
	if !bytes.Equal(got[:4], []byte{0, 0, 0, 0}) || !bytes.Equal(got[4:], jbFill(0x77)[4:]) {
		t.Errorf("the escaped copy is not the exact log bytes (first four %x)", got[:4])
	}
	x, _ := s.JournalTxns(jo, 11, "live")
	off := start + int64(j.l.JournalFSBlock(int(x[0].Blocks[1].JournalBlock)))*jbBS
	if !bytes.Equal(got, disk[off:off+jbBS]) {
		t.Error("the escaped copy differs from the slot")
	}
	verifyOK(t, s.Case)
}

func TestJournalBlocksUnverifiedCarriesAssumption(t *testing.T) {
	tx := jbTxn(7, 1, 100)
	s, _, _ := jbSession(t, jbBuild(t, ext4test.JBD2{Bit64: true, Sequence: 30, UUID: jbUUID, Txns: []ext4test.JTxn{tx}}, nil)) // no checksum feature
	sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 7})
	if err != nil || len(sum.Artifacts) != 1 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if rv := recoveryOf(sum.Artifacts[0]); !slices.Contains(rv.Assumptions, "journal-copy-unverified") || rv.Journal.Region != "stale" {
		t.Errorf("recovery %+v, want the unverified assumption on a stale copy", rv)
	}
	verifyOK(t, s.Case)
}

func TestJournalBlocksNotAllocatedSkipped(t *testing.T) {
	j := jbBuild(t, jbMainJournal(), func(img []byte, l *ext4test.Layout) {
		l.SetBlockFree(img, l.JournalFSBlock(6), true) // the data slot of seq 11's first tag
	})
	s, _, _ := jbSession(t, j)
	sum, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11, Region: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 1 || sum.NotAllocated != 1 || sum.Skipped != 1 || !strings.Contains(fmt.Sprint(sum.Warnings), "not-allocated") {
		t.Errorf("summary %+v; want the slot that the bitmap calls free skipped", sum)
	}
	verifyOK(t, s.Case)
}

func TestJournalBlocksRegionAndFSBlockFilter(t *testing.T) {
	s, _, _, _ := jbMain(t)
	ctx := context.Background()
	// seq 11 names two transactions: ambiguous before any audit entry
	n := len(auditEntries(t, s.Case))
	_, err := s.JournalBlocks(ctx, examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11})
	if !errors.Is(err, filesys.ErrAmbiguous) || len(auditEntries(t, s.Case)) != n {
		t.Fatalf("ambiguous seq: %v (audit %d -> %d)", err, n, len(auditEntries(t, s.Case)))
	}
	blk := uint64(105)
	sum, err := s.JournalBlocks(ctx, examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11, Region: "stale", FSBlock: &blk})
	if err != nil || len(sum.Artifacts) != 1 || !slices.Contains(recoveryOf(sum.Artifacts[0]).Basis, "fsblk=105") || recoveryOf(sum.Artifacts[0]).Journal.Region != "stale" {
		t.Fatalf("stale fsblock 105: %+v, %v", sum, err)
	}
	// an fs block the transaction does not log, an unknown seq: ErrNotFound with nothing audited
	n = len(auditEntries(t, s.Case))
	other := uint64(102)
	if _, err := s.JournalBlocks(ctx, examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11, Region: "stale", FSBlock: &other}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("fsblock of the other transaction: %v, want ErrNotFound", err)
	}
	if _, err := s.JournalBlocks(ctx, examine.JournalBlocksOptions{JournalOptions: jo, Seq: 999}); !errors.Is(err, filesys.ErrNotFound) {
		t.Errorf("unknown seq: %v, want ErrNotFound", err)
	}
	if len(auditEntries(t, s.Case)) != n {
		t.Error("a refused run was audited")
	}
	verifyOK(t, s.Case)
}

func TestExt4JournalNeverChangesLiveView(t *testing.T) {
	s, _, _, _ := jbMain(t)
	view := func() (string, string) {
		fresh, err := examine.Open(s.Case, s.Parent.ID, examine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fresh.Close() }()
		fsys, part, err := fresh.FS(-1)
		if err != nil {
			t.Fatal(err)
		}
		var tree strings.Builder
		if err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, werr error) error {
			fmt.Fprintf(&tree, "%s %v %d %v\n", p, e.Type, e.Size, werr)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		runs, err := fsys.Unallocated()
		if err != nil {
			t.Fatal(err)
		}
		return tree.String(), fmt.Sprint(part.Start, runs, fsys.Info())
	}
	t1, u1 := view()
	if _, err := s.JournalReport(context.Background(), jo, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JournalBlocks(context.Background(), examine.JournalBlocksOptions{JournalOptions: jo, Seq: 11, Region: "live"}); err != nil {
		t.Fatal(err)
	}
	t2, u2 := view()
	if t1 != t2 || u1 != u2 || !strings.Contains(t1, "a.txt") {
		t.Errorf("the live view changed:\n%s\n%s\n%s\n%s", t1, t2, u1, u2)
	}
}

// ext2 knows it keeps no journal, which is truthful; FAT's journal is not analysed in this build, which must never
// be said as "no journal" (3G-R26).
func TestJournalOnExt2AndFAT(t *testing.T) {
	ext2 := ext4test.Build(ext4test.Options{Label: "root"}, []ext4test.File{{Path: "/hello.txt", Data: []byte("hello")}})
	s, _ := imageSession(t, ext2)
	_, err := s.JournalList(jo)
	if !errors.Is(err, filesys.ErrNoJournal) || errors.Is(err, examine.ErrJournalNotAvailable) {
		t.Errorf("ext2: err = %v, want filesys.ErrNoJournal and not ErrJournalNotAvailable", err)
	}
	if _, err := s.JournalReport(context.Background(), jo, 0); !errors.Is(err, filesys.ErrNoJournal) {
		t.Errorf("ext2 report: %v", err)
	}
	if got := len(auditByAction(t, s.Case, "analysis.start")); got != 0 {
		t.Errorf("%d analysis.start entries for a refused run", got)
	}
	fat := fattest.Build(fattest.Options{Type: 16}, []fattest.File{{Path: "A.TXT", Data: []byte("hi")}})
	s2, _ := imageSession(t, fat)
	if _, err := s2.JournalList(jo); !errors.Is(err, examine.ErrJournalNotAvailable) || errors.Is(err, filesys.ErrNoJournal) {
		t.Errorf("fat: err = %v, want ErrJournalNotAvailable", err)
	}
}
