package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// `image journal` and `image recover --use-journal` on ext4 (3G Task 12), through the real reader on builder images.
// The journal of the main image (64 blocks of 1 KiB, csum v3):
//
//	seq 10  block 1..4    stale, committed
//	seq 11  block 5..8    live, committed; tag 103 is escaped
//	seq 12  block 9..10   live, uncommitted
//	seq 11  block 30..32  stale, committed (the same sequence as the live one)

const jcBS = 1024

var jcUUID = [16]byte{0xB1, 0xC2, 0xD3, 0xE4, 0xF5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func jcFill(n byte) []byte { return bytes.Repeat([]byte{n}, jcBS) }

func jcTxn(seq uint32, at int, fsblks ...uint64) ext4test.JTxn {
	t := ext4test.JTxn{Seq: seq, At: at, CommitSec: 1700000000 + uint64(seq)}
	for i, b := range fsblks {
		t.Tags = append(t.Tags, ext4test.JTag{FSBlock: b, Data: jcFill(byte(seq)*16 + byte(i) + 1)})
	}
	return t
}

func jcJournal() ext4test.JBD2 {
	stale10 := jcTxn(10, 1, 100, 101)
	live11 := jcTxn(11, 5, 102, 103)
	live11.Tags[1].Data = append(binary.BigEndian.AppendUint32(nil, ext4test.JBD2Magic), jcFill(0x77)[4:]...)
	live12 := jcTxn(12, 9, 104)
	live12.NoCommit = true
	return ext4test.JBD2{
		Bit64: true, CsumV3: true, Sequence: 11, Start: 5, UUID: jcUUID,
		Txns: []ext4test.JTxn{stale10, live11, live12, jcTxn(11, 30, 105)},
	}
}

func jcImage(j ext4test.JBD2) []byte {
	o := ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, UUID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, JournalBlocks: 64, BlocksPerGroup: 2048}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/a.txt", Data: bytes.Repeat([]byte("a"), 3000)}})
	l.WriteJournal(img, j)
	if j.Start != 0 {
		l.PatchSuper(img, func(sb []byte) { binary.LittleEndian.PutUint32(sb[0x60:], binary.LittleEndian.Uint32(sb[0x60:])|0x4) })
	}
	return img
}

func jcEnv(t *testing.T, mod ...func(j *ext4test.JBD2)) *imgEnv {
	t.Helper()
	j := jcJournal()
	for _, m := range mod {
		m(&j)
	}
	return f2Env(t, jcImage(j))
}

func jcCut(j *ext4test.JBD2) { j.FastCommit, j.NumFC = true, 8 }

// jcDeleted is an image with one deleted file whose inode-table block (as of before the delete) is logged in a
// committed transaction: the journal is the only source of its map. clean makes that transaction stale.
func jcDeleted(clean bool) []byte {
	o := ext4test.Options{Extents: true, MetadataCsum: true, Bit64: true, UUID: [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, JournalBlocks: 128, BlocksPerGroup: 2048}
	data := make([]byte, 5*1024+300)
	for i := range data {
		data[i] = byte(i*7 + 1)
	}
	img, l := ext4test.BuildLayout(o, []ext4test.File{{Path: "/gone.bin", Data: data, Deleted: true, Generation: 7}})
	loc := l.File("/gone.bin")
	live := l.InodeCopy(img, loc.Inode, func(raw []byte) {
		binary.LittleEndian.PutUint16(raw[0x1A:], 1)
		binary.LittleEndian.PutUint32(raw[0x14:], 0)
	})
	blk, _ := l.InodeTableBlock(loc.Inode)
	txns := []ext4test.JTxn{{Seq: 10, At: -1, Tags: []ext4test.JTag{{FSBlock: uint64(blk), Data: live}}}}
	l.Unlink(img, "/gone.bin", ext4test.UnlinkEffects{FreeBlocks: true, FreeInode: true, ZeroSize: true, ZeroBlocksCount: true, ClearExtentRoot: true})
	j := ext4test.JBD2{Bit64: true, CsumV3: true, Sequence: 10, Start: 1, UUID: jcUUID, Txns: txns}
	if clean {
		j.Sequence, j.Start = 11, 0
	}
	l.WriteJournal(img, j)
	return img
}

func TestImageJournalLsExt4(t *testing.T) {
	e := jcEnv(t)
	before := caseFiles(t, e.c)
	nAudit := len(auditActions(t, e.c))
	code, out, errOut := e.journal("ls", e.ref)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, re := range []string{
		`(?m)^jbd2 journal of partition 1 \(v2, 1024-byte blocks, 64 blocks, features [a-z0-9_,-]+\): sequence 11, start 5$`,
		`(?m)^needs_recovery: yes$`,
		`(?m)^transaction 10 \[stale\] committed: 2 blocks, 0 revoke records, checksum ok$`,
		`(?m)^transaction 11 \[live\] committed: 2 blocks, 0 revoke records, checksum ok$`,
		`(?m)^transaction 11 \[stale\] committed: 1 blocks, 0 revoke records, checksum ok$`,
		`(?m)^transaction 12 \[live\] uncommitted: 1 blocks, 0 revoke records, checksum \S+$`,
		`(?m)^journal transactions are history: they were not replayed and are not part of the live view$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("missing %s:\n%s", re, out)
		}
	}
	if strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "not shown") {
		t.Errorf("a whole journal says INCOMPLETE or not shown:\n%s", out)
	}
	after := caseFiles(t, e.c)
	for name, h := range before {
		if name != "audit.jsonl" && after[name] != h {
			t.Errorf("%s changed", name)
		}
	}
	if len(after) != len(before) {
		t.Errorf("files %d -> %d", len(before), len(after))
	}
	if acts := auditActions(t, e.c)[nAudit:]; !slices.Equal(acts, []string{"case.open"}) {
		t.Errorf("audit gained %v, want only case.open", acts)
	}
}

func TestImageJournalLsExt4Clean(t *testing.T) {
	e := f2Env(t, jcDeleted(true))
	code, out, errOut := e.journal("ls", e.ref)
	if code != 0 || !regexp.MustCompile(`(?m): sequence 11, start clean$`).MatchString(out) || !strings.Contains(out, "needs_recovery: no") ||
		!strings.Contains(out, "transaction 10 [stale] committed:") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestImageJournalLsExt4NoStale(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("ls", e.ref, "--no-stale")
	if code != 0 || strings.Contains(out, "[stale]") || !strings.Contains(out, "transaction 11 [live]") ||
		!strings.Contains(out, "(2 stale transactions not shown)") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	var j struct {
		StaleOmitted int `json:"stale_omitted"`
		Transactions []struct {
			Seq    uint32 `json:"seq"`
			Region string `json:"region"`
		} `json:"transactions"`
	}
	code, out, errOut = e.journal("ls", e.ref, "--no-stale", "--json")
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || j.StaleOmitted != 2 || len(j.Transactions) != 2 {
		t.Fatalf("exit %d json %+v %v\n%s%s", code, j, err, out, errOut)
	}
}

func TestImageJournalLsExt4IncompleteIsNotEmpty(t *testing.T) {
	e := jcEnv(t, jcCut)
	code, out, errOut := e.journal("ls", e.ref)
	if code != 0 || !regexp.MustCompile(`(?m)^INCOMPLETE: fast-commit-not-analysed; later transactions are unknown$`).MatchString(out) ||
		!strings.Contains(out, "needs_recovery: yes") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	// a cut journal that read nothing must not say "empty"
	e2 := jcEnv(t, jcCut, func(j *ext4test.JBD2) { j.Txns = nil })
	code, out, errOut = e2.journal("ls", e2.ref)
	if code != 0 || strings.Contains(out, "(empty") || !strings.Contains(out, "INCOMPLETE:") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestImageJournalShowExt4(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("show", e.ref, "11", "--region", "live")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, re := range []string{
		`(?m)^transaction 11 \[live\] committed: 2 blocks, 0 revoke records, checksum ok$`,
		`(?m)^#0 fsblock 102 jblk \d+ copy verified$`,
		`(?m)^#1 fsblock 103 jblk \d+ copy verified escaped$`,
		`(?m)^journal transactions are history: they were not replayed and are not part of the live view$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("missing %s:\n%s", re, out)
		}
	}
	code, out, errOut = e.journal("show", e.ref, "10")
	if code != 0 || !strings.Contains(out, "transaction 10 [stale]") {
		t.Fatalf("stale only seq: exit %d\n%s%s", code, out, errOut)
	}
	var j struct {
		Seq  uint32 `json:"seq"`
		Tags []struct {
			FSBlock uint64 `json:"fsblock"`
			Escaped bool   `json:"escaped"`
		} `json:"tags"`
	}
	code, out, _ = e.journal("show", e.ref, "11", "--region", "live", "--json")
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || len(j.Tags) != 2 || !j.Tags[1].Escaped {
		t.Errorf("json: exit %d %+v %v", code, j, err)
	}
}

func TestImageJournalShowExt4NoStaleNamesTheFlag(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("show", e.ref, "10", "--no-stale")
	if code != 1 || !strings.Contains(errOut, "--no-stale") || strings.Contains(errOut, "not a transaction") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestImageJournalShowAmbiguousSeqExitsOne(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("show", e.ref, "11")
	if code != 1 || !strings.Contains(errOut, "journal block 5") || !strings.Contains(errOut, "journal block 30") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if strings.Contains(out, "#0 fsblock") {
		t.Errorf("an ambiguous seq printed tags:\n%s", out)
	}
	if code, _, errOut := e.journal("show", e.ref, "11", "--region", "bogus"); code != 2 {
		t.Errorf("bad --region exits %d (%s), want 2", code, errOut)
	}
}

func TestImageJournalShowUnknownSeqOfCutJournal(t *testing.T) {
	e := jcEnv(t, jcCut)
	code, _, errOut := e.journal("show", e.ref, "999")
	if code != 1 || !strings.Contains(errOut, "later transactions unknown") || strings.Contains(errOut, "not a transaction") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
	e2 := jcEnv(t)
	code, _, errOut = e2.journal("show", e2.ref, "999")
	if code != 1 || !strings.Contains(errOut, "not a transaction of the journal") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
}

func TestImageJournalReportExt4AndVerify(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("report", e.ref)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !regexp.MustCompile(`(?m)^report \S+ \S*reports/p1-ext4/journal\.jsonl: 4 transactions \(2 live, 2 stale\), 6 tags$`).MatchString(out) ||
		strings.Contains(out, "INCOMPLETE") || !strings.Contains(out, "are not part of the live view") {
		t.Errorf("report line:\n%s", out)
	}
	code, vout := run(t, Deps{}, "case", "verify", "--case", e.c)
	if code != 0 {
		t.Fatalf("case verify: %d\n%s", code, vout)
	}
	if got := lastVerifyRun(t, e.c)["journal_rechecked"]; got != float64(1) {
		t.Errorf("verify.run journal_rechecked = %v, want 1 (R13 ran on the report)", got)
	}
}

func TestImageJournalReportExt4NoStale(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("report", e.ref, "--no-stale")
	if code != 0 || !strings.Contains(out, ": 2 transactions (2 live, 0 stale), 3 tags") || !strings.Contains(out, "--no-stale") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if code, vout := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, vout)
	}
}

func TestImageJournalReportExt4IncompleteLine(t *testing.T) {
	e := jcEnv(t, jcCut)
	code, out, errOut := e.journal("report", e.ref)
	if code != 0 || !regexp.MustCompile(`(?m)^INCOMPLETE: fast-commit-not-analysed; later transactions are unknown$`).MatchString(out) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	e2 := jcEnv(t, jcCut)
	code, out, _ = e2.journal("report", e2.ref, "--json")
	var j struct {
		Incomplete bool   `json:"incomplete"`
		Complete   bool   `json:"complete"`
		Stop       string `json:"stop"`
		Txns       int    `json:"transactions"`
		Tags       int    `json:"tags"`
	}
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || !j.Incomplete || j.Complete || j.Stop != "fast-commit-not-analysed" || j.Txns == 0 || j.Tags == 0 {
		t.Fatalf("exit %d json %+v %v\n%s", code, j, err, out)
	}
	e3 := jcEnv(t)
	code, out, _ = e3.journal("report", e3.ref, "--max-bytes", "900")
	if code != 0 || !strings.Contains(out, "INCOMPLETE: the report is cut") {
		t.Errorf("max-bytes: exit %d\n%s", code, out)
	}
}

var jcBlockLine = regexp.MustCompile(`(?m)^\[journal-block:ext4-journal seq (\d+) (live|stale)\] fsblock (\d+) -> (\S+) (\S+)$`)

func TestImageJournalBlocksExt4(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("blocks", e.ref, "11")
	if code != 1 || !strings.Contains(errOut, "region") {
		t.Fatalf("an ambiguous seq: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = e.journal("blocks", e.ref, "11", "--region", "live")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lines := jcBlockLine.FindAllStringSubmatch(out, -1)
	if len(lines) != 2 || lines[0][3] != "102" || lines[1][3] != "103" {
		t.Fatalf("%d block lines:\n%s", len(lines), out)
	}
	for _, re := range []string{
		`(?m)^copied 2$`, `(?m)^skipped copy-mismatch: 0, missing: 0, not-allocated: 0$`, `(?m)^bitmap unconfirmed: \d+$`,
		`(?m)^journal transactions are history: they were not replayed and are not part of the live view$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("missing %s:\n%s", re, out)
		}
	}
	if code, vout := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, vout)
	}
	// --fsblock copies one block, in a case that has not copied it yet
	e2 := jcEnv(t)
	code, out, errOut = e2.journal("blocks", e2.ref, "11", "--region", "live", "--fsblock", "103")
	if one := jcBlockLine.FindAllStringSubmatch(out, -1); code != 0 || len(one) != 1 || one[0][3] != "103" {
		t.Errorf("--fsblock: exit %d\n%s%s", code, out, errOut)
	}
	var j struct {
		Copied       int  `json:"copied"`
		AllocUnknown *int `json:"alloc_unknown"`
		CopyMismatch *int `json:"copy_mismatch"`
		Missing      *int `json:"missing"`
		NotAllocated *int `json:"not_allocated"`
		Blocks       []struct {
			FSBlock uint64 `json:"fsblock"`
			Region  string `json:"region"`
		} `json:"blocks"`
	}
	e3 := jcEnv(t)
	code, out, _ = e3.journal("blocks", e3.ref, "11", "--region", "live", "--json")
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || j.Copied != 2 || len(j.Blocks) != 2 || j.Blocks[0].Region != "live" ||
		j.AllocUnknown == nil || j.CopyMismatch == nil || j.Missing == nil || j.NotAllocated == nil {
		t.Errorf("json: exit %d %+v %v\n%s", code, j, err, out)
	}
}

func TestImageJournalBlocksExt4UnknownFSBlockWritesNothing(t *testing.T) {
	e := jcEnv(t)
	nRecs := func() int { r, _ := manifest(e.c); return len(r) }
	before := nRecs()
	code, out, errOut := e.journal("blocks", e.ref, "11", "--region", "live", "--fsblock", "999")
	if code != 1 || out != "" || !strings.Contains(errOut, "not logged by transaction 11") || nRecs() != before {
		t.Errorf("exit %d stdout %q stderr %q, %d -> %d records", code, out, errOut, before, nRecs())
	}
	for _, a := range auditActions(t, e.c) {
		if a == "analysis.start" {
			t.Error("an analysis.start entry was written")
		}
	}
}

func TestImageJournalBlocksExt4NoStaleNamesTheFlag(t *testing.T) {
	e := jcEnv(t)
	code, out, errOut := e.journal("blocks", e.ref, "10", "--no-stale")
	if code != 1 || out != "" || !strings.Contains(errOut, "--no-stale") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestImageJournalOnExt2SaysNoJournal(t *testing.T) {
	img := ext4test.Build(ext4test.Options{}, []ext4test.File{{Path: "/a.txt", Data: []byte("hello")}})
	e := f2Env(t, img)
	for _, sub := range []string{"ls", "report"} {
		code, out, errOut := e.journal(sub, e.ref)
		if code != 1 || !strings.Contains(errOut, "keeps no journal") || strings.Contains(errOut+out, "not available") {
			t.Errorf("%s: exit %d stdout %q stderr %q", sub, code, out, errOut)
		}
	}
	if code, _, errOut := e.journal("show", e.ref, "1"); code != 1 || !strings.Contains(errOut, "keeps no journal") {
		t.Errorf("show: exit %d stderr %q", code, errOut)
	}
}

func jcRecover(e *imgEnv, args ...string) (int, string, string) {
	return e.cli(append([]string{"image", "recover", "--case", e.c, e.ref}, args...)...)
}

var jcMarker = regexp.MustCompile(`(?m)^\[recovered:ext4-journal-inode conf (\d+) \w+\] \[journal seq 10 (live|stale)\] `)

func TestImageRecoverUseJournal(t *testing.T) {
	e := f2Env(t, jcDeleted(true))
	code, out, errOut := jcRecover(e, "--all", "--use-journal", "stale")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	m := jcMarker.FindStringSubmatch(out)
	if m == nil || m[1] != "40" || m[2] != "stale" || !strings.Contains(out, "/gone.bin") || !strings.Contains(out, "are not part of the live view") {
		t.Fatalf("marker %v:\n%s", m, out)
	}
	if code, vout := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, vout)
	}
	recs := recoveredRecords(t, e.c)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	evidencetest.FlipArtifactByte(t, e.c, recs[0], 0)
	if code, vout := run(t, Deps{}, "case", "verify", "--case", e.c); code != 4 {
		t.Errorf("a flipped byte: case verify exit %d, want 4\n%s", code, vout)
	}
}

func TestImageRecoverUseJournalList(t *testing.T) {
	e := f2Env(t, jcDeleted(true))
	before := caseFiles(t, e.c)
	nAudit := len(auditActions(t, e.c))
	code, out, errOut := jcRecover(e, "--list", "--use-journal", "stale")
	if code != 0 || !jcMarker.MatchString(out) || !strings.Contains(out, "[would write]") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if after := caseFiles(t, e.c); len(after) != len(before) {
		t.Errorf("files %d -> %d", len(before), len(after))
	}
	if acts := auditActions(t, e.c)[nAudit:]; !slices.Equal(acts, []string{"case.open"}) {
		t.Errorf("audit gained %v", acts)
	}
	code, out, _ = jcRecover(e, "--list", "--use-journal", "stale", "--json")
	var l struct {
		Items []struct {
			Candidates []struct {
				Method  string               `json:"method"`
				Journal *evidence.JournalRef `json:"journal"`
			} `json:"candidates"`
		} `json:"items"`
	}
	if err := json.Unmarshal(jsonPart(out), &l); code != 0 || err != nil || len(l.Items) != 1 || len(l.Items[0].Candidates) != 1 ||
		l.Items[0].Candidates[0].Journal == nil || l.Items[0].Candidates[0].Journal.Region != "stale" {
		t.Errorf("json: exit %d %+v %v\n%s", code, l, err, out)
	}
	// without --use-journal the journal is not consulted: the 3C answer stays undecided, with no journal marker
	code, out, _ = jcRecover(e, "--list")
	if code != 1 || !strings.Contains(out, "[skipped: undecided]") || strings.Contains(out, "[journal seq") || strings.Contains(out, "not part of the live view") {
		t.Errorf("plain list: exit %d\n%s", code, out)
	}
}

func TestImageRecoverUseJournalBadValueIsUsage(t *testing.T) {
	e := f2Env(t, jcDeleted(true))
	nAudit := len(auditActions(t, e.c))
	code, _, errOut := jcRecover(e, "--all", "--use-journal", "everything")
	if code != 2 || !strings.Contains(errOut, "everything") {
		t.Errorf("exit %d stderr %q, want 2 naming the value", code, errOut)
	}
	if got := len(auditActions(t, e.c)); got != nAudit {
		t.Errorf("audit gained %d entries", got-nAudit)
	}
}

func TestImageRecoverUseJournalWithRollForwardIsUsage(t *testing.T) {
	e := f2Env(t, jcDeleted(true))
	nAudit := len(auditActions(t, e.c))
	code, _, errOut := jcRecover(e, "--all", "--rollforward", "--use-journal", "stale")
	if code != 2 || !strings.Contains(errOut, "--use-journal") {
		t.Errorf("exit %d stderr %q, want 2 naming --use-journal", code, errOut)
	}
	if got := len(auditActions(t, e.c)); got != nAudit {
		t.Errorf("audit gained %d entries", got-nAudit)
	}
}

func TestImageRecoverUseJournalOnFATExitsOne(t *testing.T) {
	img, _ := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{3}, 2048), Deleted: true}})
	e := f2Env(t, img)
	code, out, errOut := jcRecover(e, "--all", "--use-journal", "committed")
	if code != 1 || !strings.Contains(errOut, "journal recovery is not available") || !strings.Contains(errOut, "fat") || strings.Contains(out, "[recovered:") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestImageRecoverUseJournalOnJournallessExt4ExitsOne(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{{Path: "/gone.bin", Data: []byte("hello"), Deleted: true}})
	e := f2Env(t, img)
	code, out, errOut := jcRecover(e, "--all", "--use-journal", "committed")
	if code != 1 || !strings.Contains(errOut, "keeps no journal") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// The journal commands never change what `image ls` shows (spec 9.1: a transaction is history, never applied).
func TestImageLsUnchangedByJournal(t *testing.T) {
	e := f2Env(t, jcDeleted(true))
	before := e.lsBoth(t)
	if !strings.Contains(before, "gone.bin") {
		t.Fatalf("setup: the deleted file is not listed:\n%s", before)
	}
	for _, args := range [][]string{
		{"image", "journal", "report", "--case", e.c, e.ref},
		{"image", "journal", "blocks", "--case", e.c, e.ref, "10"},
		{"image", "recover", "--case", e.c, e.ref, "--all", "--use-journal", "stale"},
	} {
		if code, out, errOut := e.cli(args...); code != 0 {
			t.Fatalf("%v: %d\n%s%s", args, code, out, errOut)
		}
	}
	if after := e.lsBoth(t); after != before {
		t.Errorf("image ls changed:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

// The ambiguous-seq show prints the transaction lines it found, so it ends with the caution like every journal output.
func TestImageJournalShowAmbiguousSeqPrintsCaution(t *testing.T) {
	e := jcEnv(t)
	code, out, _ := e.journal("show", e.ref, "11")
	if code != 1 || !strings.Contains(out, "transaction 11 [live]") || !strings.HasSuffix(out, journalCaution+"\n") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

// no_stale reports the flag, not what happened to be omitted: with no stale transaction to leave out, --no-stale still
// says the stale ones were not asked for.
func TestImageJournalLsExt4NoStaleJSONIsTheFlag(t *testing.T) {
	liveOnly := func(j *ext4test.JBD2) { j.Txns = []ext4test.JTxn{j.Txns[1], j.Txns[2]} }
	e := jcEnv(t, liveOnly)
	var j struct {
		NoStale      bool `json:"no_stale"`
		StaleOmitted int  `json:"stale_omitted"`
	}
	code, out, errOut := e.journal("ls", e.ref, "--no-stale", "--json")
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || !j.NoStale || j.StaleOmitted != 0 {
		t.Fatalf("exit %d json %+v %v\n%s%s", code, j, err, out, errOut)
	}
	code, out, _ = e.journal("ls", e.ref, "--json")
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || j.NoStale {
		t.Errorf("without the flag: exit %d json %+v %v", code, j, err)
	}
}

// --stale is the default spelled out (spec 12: [--stale|--no-stale]); giving both is a usage error.
func TestImageJournalStaleFlag(t *testing.T) {
	e := jcEnv(t)
	for _, c := range [][]string{{"ls", e.ref}, {"show", e.ref, "10"}, {"report", e.ref}, {"blocks", e.ref, "10"}} {
		code, out, errOut := e.journal(c[0], append(c[1:], "--stale")...)
		if code != 0 || !strings.Contains(out, journalCaution) {
			t.Errorf("%s --stale: exit %d\n%s%s", c[0], code, out, errOut)
		}
		code, out, errOut = e.journal(c[0], append(c[1:], "--stale", "--no-stale")...)
		if code != 2 || out != "" || !strings.Contains(errOut, "--stale") || !strings.Contains(errOut, "--no-stale") {
			t.Errorf("%s --stale --no-stale: exit %d stdout %q stderr %q, want 2", c[0], code, out, errOut)
		}
	}
	code, out, _ := e.journal("ls", e.ref, "--stale")
	if code != 0 || !strings.Contains(out, "[stale]") {
		t.Errorf("--stale lists the stale transactions: exit %d\n%s", code, out)
	}
}
