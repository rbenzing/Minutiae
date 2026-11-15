package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// `image journal` and `image recover --rollforward` (3H Task 11) through the real F2FS reader on builder images
// whose warm-node log holds fsynced files that no checkpoint knows.

func rfOpts() f2fstest.Options {
	return f2fstest.Options{NodeSegs: 2, SSA: true, Unclean: true, CPFlags: f2fstest.CPCRCRecovery, ExtraAttr: true, InodeChksum: true}
}

func rfFiles() []f2fstest.File {
	return []f2fstest.File{
		{Path: "/rf", Dir: true},
		{Path: "/live.bin", Data: f2Stamp(2*f2bs, "L")},
		{Path: "/rf/a.bin", Data: f2Stamp(3*f2bs, "a"), PostCheckpoint: true},
		{Path: "/rf/b.bin", Data: f2Stamp(2*f2bs, "b"), PostCheckpoint: true},
	}
}

func rfImage(patch ...func(img []byte, tr *f2fstest.Tree)) []byte {
	img, tr := f2fstest.BuildTree(rfOpts(), rfFiles())
	for _, p := range patch {
		p(img, tr)
	}
	return img
}

func rfEnv(t *testing.T, patch ...func(img []byte, tr *f2fstest.Tree)) *imgEnv {
	t.Helper()
	return f2Env(t, rfImage(patch...))
}

// cutChain plants a footer of the wrong version right after the chain, so the walk stops with later nodes unknown.
func cutChain(img []byte, tr *f2fstest.Tree) {
	stop := tr.ChainAddrs[len(tr.ChainAddrs)-1] + 1
	f2fstest.SetFooter(img, stop, f2fstest.Footer{NID: 20, Ino: 0, CPVer: f2fstest.ChainFooterVersion(img)})
}

// cli runs one command with stdout and stderr apart.
func (e *imgEnv) cli(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	d := e.d
	d.Out, d.Err, d.In, d.Registry = &out, &errb, strings.NewReader(""), device.NewRegistry()
	code = Run(args, d)
	return code, out.String(), errb.String()
}

func (e *imgEnv) journal(sub string, args ...string) (int, string, string) {
	return e.cli(append([]string{"image", "journal", sub, "--case", e.c}, args...)...)
}

var txnLine = regexp.MustCompile(`(?m)^transaction (\d+): (\d+) blocks, (\d+) would be replayed; files: (\d+) post-checkpoint, (\d+) live update\(s\), (\d+) not replayed$`)

func rfSeq(t *testing.T, e *imgEnv) string {
	t.Helper()
	code, out, errOut := e.journal("ls", e.ref)
	m := txnLine.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("journal ls: %d\n%s%s", code, out, errOut)
	}
	return m[1]
}

func TestImageJournalLsIsReadOnly(t *testing.T) {
	e := rfEnv(t)
	before := caseFiles(t, e.c)
	nAudit := len(auditActions(t, e.c))
	code, out, errOut := e.journal("ls", e.ref)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !regexp.MustCompile(`(?m)^f2fs roll-forward log of partition 1 \(checkpoint \d+, pack \d, crc-recovery\): start block \d+, \d+ nodes, stop \S+$`).MatchString(out) {
		t.Errorf("header line missing:\n%s", out)
	}
	m := txnLine.FindStringSubmatch(out)
	if m == nil || m[4] != "2" || m[5] != "0" || m[6] != "0" || m[3] == "0" {
		t.Errorf("transaction line %v:\n%s", m, out)
	}
	if want := "roll-forward data is what a replay would find; it was not replayed and is not part of the live view"; !strings.Contains(out, want) {
		t.Errorf("missing %q:\n%s", want, out)
	}
	if strings.Contains(out, "INCOMPLETE") {
		t.Errorf("a complete chain says INCOMPLETE:\n%s", out)
	}
	after := caseFiles(t, e.c)
	for name, h := range before {
		if name != "audit.jsonl" && after[name] != h {
			t.Errorf("%s changed", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s was created", name)
		}
	}
	if acts := auditActions(t, e.c)[nAudit:]; !slices.Equal(acts, []string{"case.open"}) {
		t.Errorf("audit gained %v, want only case.open", acts)
	}
}

func TestImageJournalLsEmptyChain(t *testing.T) {
	img := f2fstest.Build(f2fstest.Options{NodeSegs: 2, SSA: true, CPFlags: f2fstest.CPCRCRecovery}, []f2fstest.File{{Path: "/live.bin", Data: f2Stamp(f2bs, "L")}})
	e := f2Env(t, img)
	code, out, errOut := e.journal("ls", e.ref)
	if code != 0 || !strings.Contains(out, "(empty: the checkpoint's log holds no node written after it)") || strings.Contains(out, "transaction ") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestImageJournalLsJSON(t *testing.T) {
	e := rfEnv(t, cutChain)
	code, out, errOut := e.journal("ls", e.ref, "--json")
	var j struct {
		Partition  int    `json:"partition"`
		Nodes      int    `json:"nodes"`
		Stop       string `json:"stop"`
		Complete   bool   `json:"complete"`
		Incomplete bool   `json:"incomplete"`
		Txns       []struct {
			Seq uint32 `json:"seq"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil {
		t.Fatalf("exit %d %v\n%s%s", code, err, out, errOut)
	}
	if j.Partition != 1 || j.Nodes == 0 || j.Complete || !j.Incomplete || !strings.HasPrefix(j.Stop, "footer@") || len(j.Txns) != 1 {
		t.Errorf("json %+v", j)
	}
}

func TestImageJournalShow(t *testing.T) {
	e := rfEnv(t)
	seq := rfSeq(t, e)
	before := caseFiles(t, e.c)
	code, out, errOut := e.journal("show", e.ref, seq)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	node := regexp.MustCompile(`(?m)^#(\d+) blk (\d+) nid (\d+) ino (\d+) (inode|direct) ofs (\d+)( fsync)?( dentry)? replay=(yes|no) live-ino=(yes|no) next (\d+)$`)
	if got := len(node.FindAllString(out, -1)); got < 2 {
		t.Errorf("%d node lines:\n%s", got, out)
	}
	if !strings.Contains(out, "replay=yes live-ino=no") || !strings.Contains(out, "fsync dentry") {
		t.Errorf("marks missing:\n%s", out)
	}
	if !regexp.MustCompile(`(?m)^    size 12288 mode \S+ parent \d+ name a\.bin checksum ok$`).MatchString(out) {
		t.Errorf("inode line of a.bin missing:\n%s", out)
	}
	after := caseFiles(t, e.c)
	for name, h := range before {
		if name != "audit.jsonl" && after[name] != h {
			t.Errorf("%s changed", name)
		}
	}
}

func TestImageJournalShowWrongSeqExitsOne(t *testing.T) {
	e := rfEnv(t)
	seq, _ := strconv.Atoi(rfSeq(t, e))
	code, out, errOut := e.journal("show", e.ref, strconv.Itoa(seq+1))
	if code != 1 || out != "" || !strings.Contains(errOut, "is not a transaction of the roll-forward log") {
		t.Errorf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if code, _, _ := e.journal("show", e.ref, "x"); code != 2 {
		t.Errorf("a non-numeric seq exits %d, want 2", code)
	}
}

// A seq missing from a cut chain is unknown, never "not found".
func TestImageJournalShowUnknownSeqOfCutChain(t *testing.T) {
	e := rfEnv(t, cutChain)
	seq, _ := strconv.Atoi(rfSeq(t, e))
	code, _, errOut := e.journal("show", e.ref, strconv.Itoa(seq+1))
	if code != 1 || !strings.Contains(errOut, "later nodes unknown") || strings.Contains(errOut, "is not a transaction") {
		t.Errorf("exit %d stderr %q", code, errOut)
	}
	code, _, errOut = e.journal("blocks", e.ref, strconv.Itoa(seq+1))
	if code != 1 || !strings.Contains(errOut, "later nodes unknown") {
		t.Errorf("blocks: exit %d stderr %q", code, errOut)
	}
}

func TestImageJournalOnExt4SaysNotAvailable(t *testing.T) {
	img := ext4test.Build(ext4test.Options{Extents: true, MetadataCsum: true, Label: "root"}, []ext4test.File{{Path: "/hello.txt", Data: []byte("hello")}})
	e := f2Env(t, img)
	for _, sub := range []string{"ls", "report"} {
		code, out, errOut := e.journal(sub, e.ref)
		if code != 1 || !strings.Contains(errOut, "not available for ext4") || strings.Contains(errOut+out, "no journal") {
			t.Errorf("%s: exit %d stdout %q stderr %q", sub, code, out, errOut)
		}
	}
	code, _, errOut := e.journal("show", e.ref, "1")
	if code != 1 || !strings.Contains(errOut, "not available for ext4") {
		t.Errorf("show: exit %d stderr %q", code, errOut)
	}
}

func TestImageJournalReportAndVerify(t *testing.T) {
	e := rfEnv(t)
	code, out, errOut := e.journal("report", e.ref)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !regexp.MustCompile(`(?m)^report \S+ \S*reports/p1-f2fs/rollforward\.jsonl: \d+ nodes, 2 files$`).MatchString(out) || strings.Contains(out, "INCOMPLETE") {
		t.Errorf("report line:\n%s", out)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
	if got := lastVerifyRun(t, e.c)["rollforward_rechecked"]; got != float64(1) {
		t.Errorf("verify.run rollforward_rechecked = %v, want 1 (R12 ran on the report)", got)
	}
}

func TestImageJournalReportIncompleteLine(t *testing.T) {
	e := rfEnv(t, cutChain)
	code, out, errOut := e.journal("report", e.ref)
	if code != 0 || !regexp.MustCompile(`(?m)^INCOMPLETE: the chain is cut at footer@\d+; later nodes are unknown$`).MatchString(out) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	e2 := rfEnv(t, cutChain)
	code, out, errOut = e2.journal("report", e2.ref, "--json")
	var j struct {
		Incomplete bool   `json:"incomplete"`
		Complete   bool   `json:"complete"`
		Stop       string `json:"stop"`
		Nodes      int    `json:"nodes"`
	}
	if err := json.Unmarshal(jsonPart(out), &j); code != 0 || err != nil || !j.Incomplete || j.Complete || !strings.HasPrefix(j.Stop, "footer@") || j.Nodes == 0 {
		t.Fatalf("exit %d json %+v %v\n%s%s", code, j, err, out, errOut)
	}
}

func TestImageJournalReportMaxBytesCutIsIncomplete(t *testing.T) {
	e := rfEnv(t)
	code, out, errOut := e.journal("report", e.ref, "--max-bytes", "900")
	if code != 0 || !strings.Contains(out, "INCOMPLETE: the report is cut") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if code, _, _ := e.journal("report", e.ref, "--max-bytes", "-1"); code != 2 {
		t.Errorf("negative --max-bytes exits %d, want 2", code)
	}
}

var blockLine = regexp.MustCompile(`(?m)^\[journal-block:f2fs-rollforward seq (\d+)\] blk (\d+) -> (\S+) (\S+)$`)

func TestImageJournalBlocks(t *testing.T) {
	e := rfEnv(t)
	seq := rfSeq(t, e)
	code, out, errOut := e.journal("blocks", e.ref, seq)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	lines := blockLine.FindAllStringSubmatch(out, -1)
	if len(lines) < 2 || !strings.Contains(out, "skipped not-free: 0") {
		t.Fatalf("%d block lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0][4], "journal/p1-f2fs/seq"+seq+"-fsblk"+lines[0][2]+".bin") {
		t.Errorf("path %q", lines[0][4])
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
	// --fsblock copies one node, in a case that has not copied it yet
	e2 := rfEnv(t)
	code, out, errOut = e2.journal("blocks", e2.ref, seq, "--fsblock", lines[1][2])
	if one := blockLine.FindAllStringSubmatch(out, -1); code != 0 || len(one) != 1 || one[0][2] != lines[1][2] {
		t.Errorf("--fsblock: exit %d\n%s%s", code, out, errOut)
	}
}

func TestImageJournalBlocksUnknownFSBlockWritesNothing(t *testing.T) {
	e := rfEnv(t)
	seq := rfSeq(t, e)
	nRecs := func() int { r, _ := manifest(e.c); return len(r) }
	before := nRecs()
	code, out, errOut := e.journal("blocks", e.ref, seq, "--fsblock", "3")
	if code != 1 || out != "" || !strings.Contains(errOut, "is not a node of the roll-forward log") || nRecs() != before {
		t.Errorf("exit %d stdout %q stderr %q, %d -> %d records", code, out, errOut, before, nRecs())
	}
	es, err := evidence.ReadAuditEntries(e.c + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range es {
		if en.Action == "analysis.start" {
			t.Error("an analysis.start entry was written")
		}
	}
}

// lsBoth is the stdout of `image ls -r` and `image ls --deleted -r`.
func (e *imgEnv) lsBoth(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, args := range [][]string{{"-r"}, {"--deleted", "-r"}} {
		code, out, errOut := e.cli(append([]string{"image", "ls", "--case", e.c, e.ref}, args...)...)
		if code != 0 {
			t.Fatalf("ls %v: %d\n%s%s", args, code, out, errOut)
		}
		fmt.Fprintf(&b, "== %v\n%s", args, out)
	}
	return b.String()
}

// The roll-forward commands never change what `image ls` shows (spec 5.2: never applied to the live view).
func TestImageLsUnchangedByRollForward(t *testing.T) {
	e := rfEnv(t)
	before := e.lsBoth(t)
	if strings.Contains(before, "a.bin") || !strings.Contains(before, "live.bin") {
		t.Fatalf("setup: post-checkpoint file visible in the live view:\n%s", before)
	}
	seq := rfSeq(t, e)
	for _, args := range [][]string{
		{"image", "journal", "report", "--case", e.c, e.ref},
		{"image", "journal", "blocks", "--case", e.c, e.ref, seq},
		{"image", "recover", "--case", e.c, e.ref, "--rollforward", "--all"},
	} {
		if code, out, errOut := e.cli(args...); code != 0 {
			t.Fatalf("%v: %d\n%s%s", args, code, out, errOut)
		}
	}
	if after := e.lsBoth(t); after != before {
		t.Errorf("image ls changed:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

// rfFakeFS forwards everything and lets a test choose each post-checkpoint file's typed error.
type rfFakeFS struct {
	filesys.FileSystem
	errs map[string]error // by file name
}

func (f rfFakeFS) Underlying() filesys.FileSystem { return f.FileSystem }

func (f rfFakeFS) inner() filesys.RollForwarder {
	rf, _ := filesys.As[filesys.RollForwarder](f.FileSystem)
	return rf
}

func (f rfFakeFS) RollForward(visit func(filesys.RollForwardNode) bool) (filesys.RollForwardChain, error) {
	return f.inner().RollForward(visit)
}

func (f rfFakeFS) PostCheckpointFiles() ([]filesys.PostCheckpointFile, error) {
	files, err := f.inner().PostCheckpointFiles()
	for i := range files {
		if e, ok := f.errs[files[i].Name]; ok {
			files[i].Err, files[i].Candidates = e, nil
		}
	}
	return files, err
}

func rfFakeDrivers(errs map[string]error) []detect.Driver {
	out := slices.Clone(detect.Drivers)
	for i := range out {
		if out[i].Name != "f2fs" {
			continue
		}
		openReal := out[i].Open
		out[i].Open = func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
			fsys, err := openReal(r, size)
			if err != nil {
				return nil, err
			}
			return rfFakeFS{FileSystem: fsys, errs: errs}, nil
		}
	}
	return out
}

func TestImageRecoverRollForwardUndecidedExitsOne(t *testing.T) {
	e := rfEnv(t)
	e.d = Deps{FSDrivers: rfFakeDrivers(map[string]error{"a.bin": fmt.Errorf("%w: an unmeasured shape", filesys.ErrUndecided)})}
	code, out, errOut := e.cli("image", "recover", "--case", e.c, e.ref, "--rollforward", "--all")
	if code != 1 || !strings.Contains(out, "undecided 1") || !strings.Contains(errOut, "1 post-checkpoint files could not be decided") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	if got := len(recoveredRecords(t, e.c)); got != 1 {
		t.Errorf("%d artifacts, want the decided one", got)
	}

	e = rfEnv(t)
	e.d = Deps{FSDrivers: rfFakeDrivers(map[string]error{"a.bin": fmt.Errorf("%w: inline data", filesys.ErrUnsupported)})}
	code, out, errOut = e.cli("image", "recover", "--case", e.c, e.ref, "--rollforward", "--all")
	if code != 0 || strings.Contains(out, "undecided") || !strings.Contains(out, "unsupported") {
		t.Fatalf("decided skip: exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
}
