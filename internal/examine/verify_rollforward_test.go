package examine_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// R12 (3H-R13 e): case verify renders the roll-forward artifacts again from the image.

// rfVerify is case verify with every composed check, as the command line composes them.
func rfVerify(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(context.Background(), examine.VerifyChecks(examine.Options{})...)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// rfCase is a case holding a report, the block copies and the recovered post-checkpoint files.
type rfCase struct {
	s      *examine.Session
	disk   []byte
	report evidence.ManifestRecord
	blocks []evidence.ManifestRecord
	files  []evidence.ManifestRecord
	start  int64
}

func newRFCase(t *testing.T) *rfCase {
	t.Helper()
	s, disk, _ := jrnSession(t)
	part, err := s.Partition(-1)
	if err != nil {
		t.Fatal(err)
	}
	rc := &rfCase{s: s, disk: disk, start: part.Start}
	_, rc.report = jrnReport(t, s, 0)
	rc.blocks = jrnBlocks(t, s, examine.JournalBlocksOptions{JournalOptions: jo, Seq: jrnSeq(t, s)}).Artifacts
	rc.files = rwRecords(rwAll(t, s))
	if len(rc.blocks) != 2 || len(rc.files) != 2 {
		t.Fatalf("%d block copies, %d recovered files; want 2 and 2", len(rc.blocks), len(rc.files))
	}
	return rc
}

// rfProblems are the problems of rep that R12 reports.
func rfProblems(rep evidence.VerifyReport) []string {
	var out []string
	for _, p := range rep.Problems {
		if strings.HasPrefix(p, "rollforward: ") {
			out = append(out, p)
		}
	}
	return out
}

// craft writes a roll-forward artifact of kind with a crafted derivation, as a writer with a bug would.
func (rc *rfCase) craft(t *testing.T, kind, path string, data []byte, mut func(d *evidence.Derivation)) evidence.ManifestRecord {
	t.Helper()
	p := rc.s.Parent
	d := &evidence.Derivation{ParentID: p.ID, ParentSHA256: p.SHA256, Partition: 1, PartitionOffset: rc.start, FSType: "f2fs"}
	mut(d)
	rec, err := rc.s.Case.Capture(p.Source.DeviceID, "forged1", path, evidence.Source{Kind: kind, DeviceID: p.Source.DeviceID, Derived: d},
		func(w io.Writer) error { _, err := w.Write(data); return err })
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVerifyRollForwardClean(t *testing.T) {
	rc := newRFCase(t)
	rep := rfVerify(t, rc.s.Case)
	if !rep.OK() {
		t.Fatalf("problems: %v", rep.Problems)
	}
	if want := 1 + len(rc.blocks) + len(rc.files); rep.RollForwardRechecked != want {
		t.Errorf("RollForwardRechecked = %d, want %d", rep.RollForwardRechecked, want)
	}
	for _, n := range rep.Notices {
		if strings.Contains(n, `kind "report"`) {
			t.Errorf("a report R12 rendered again still has its notice: %s", n)
		}
	}
	// without R12 the report is noticed, not reproduced
	plain, err := rc.s.Case.VerifyWith(context.Background(), examine.RecoveredCheck(), examine.AllocationCheck(nil), examine.GapCheck())
	if err != nil {
		t.Fatal(err)
	}
	if plain.RollForwardRechecked != 0 {
		t.Errorf("checks without R12 counted %d", plain.RollForwardRechecked)
	}
	evidencetest.RequireNotice(t, plain, `kind "report"`)
}

func TestVerifyRollForwardReportRewrittenConsistently(t *testing.T) {
	rc := newRFCase(t)
	forged := []byte(`{"type":"chain","format":"minutiae-f2fs-rollforward/1","partition":1,"nodes":0,"complete":true}` + "\n")
	recordstest.RewriteArtifactConsistently(t, rc.s.Case.Dir, rc.report.ID, forged)
	rep := rfVerify(t, rc.s.Case)
	got := rfProblems(rep)
	if len(got) != 1 || !strings.Contains(got[0], "differs from a fresh rendering") || !strings.Contains(got[0], "report") {
		t.Fatalf("rollforward problems %q (all: %v)", got, rep.Problems)
	}
	if rep.OK() {
		t.Error("a forged report verified")
	}
}

func TestVerifyRollForwardBlockNotInChain(t *testing.T) {
	rc := newRFCase(t)
	_, nodes := jrnChain(t, rc.s)
	blk := nodes[len(nodes)-1].Block + 5
	off := rc.start + int64(blk)*bs
	rc.craft(t, evidence.KindJournal, fmt.Sprintf("journal/p1-f2fs/seq1-fsblk%d.bin", blk), rc.disk[off:off+bs], func(d *evidence.Derivation) {
		d.Runs = []evidence.Run{{Offset: off, Length: bs}}
		d.Recovery = &evidence.Recovery{
			Class: evidence.ClassJournalBlock, Method: "f2fs-rollforward", Scope: "unallocated", Algorithm: evidence.AlgorithmRecover,
			Alloc: evidence.AllocSummary{Free: bs}, Journal: &evidence.JournalRef{Seq: jrnSeq(t, rc.s), Region: "live"},
		}
	})
	got := rfProblems(rfVerify(t, rc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "block") || !strings.Contains(got[0], "is not a node of the chain") {
		t.Fatalf("rollforward problems %q", got)
	}
}

func TestVerifyRollForwardPostCheckpointNotInChain(t *testing.T) {
	rc := newRFCase(t)
	good := rc.files[0].Source.Derived
	rc.craft(t, evidence.KindRecover, "recovered/p1-f2fs/000099-x.bin", []byte("x"), func(d *evidence.Derivation) {
		d.FSPath, d.FSID = "/rf/x.bin", "rollforward:999"
		d.Runs = good.Runs[:1]
		rv := *good.Recovery
		d.Recovery = &rv
		d.Runs = []evidence.Run{{Offset: good.Runs[0].Offset, Length: 1}}
		d.Recovery.Alloc = evidence.AllocSummary{Free: 1}
	})
	got := rfProblems(rfVerify(t, rc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "is not a post-checkpoint file of the chain") {
		t.Fatalf("rollforward problems %q", got)
	}
}

func TestVerifyRollForwardRunsNotAPrefix(t *testing.T) {
	rc := newRFCase(t)
	good := rc.files[0].Source.Derived
	shifted := evidence.Run{Offset: good.Runs[0].Offset + bs, Length: bs}
	rc.craft(t, evidence.KindRecover, "recovered/p1-f2fs/000098-y.bin", rc.disk[shifted.Offset:shifted.Offset+bs], func(d *evidence.Derivation) {
		d.FSPath, d.FSID = good.FSPath, good.FSID
		d.Runs = []evidence.Run{shifted}
		rv := *good.Recovery
		rv.Assumptions = append(append([]string(nil), rv.Assumptions...), fmt.Sprintf("size-declared=%d", good.Runs[0].Length))
		rv.Alloc = evidence.AllocSummary{Free: bs}
		d.Recovery = &rv
	})
	got := rfProblems(rfVerify(t, rc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "runs are not a prefix of the map") {
		t.Fatalf("rollforward problems %q", got)
	}
}

// Ruling C7: a report R12 did not render again keeps the notice.
func TestVerifyRollForwardUnrecomputedReportKeepsNotice(t *testing.T) {
	rc := newRFCase(t)
	other := evidencetest.AddImage(t, rc.s.Case, make([]byte, 64<<10)) // a raw image: no partition to roll forward
	evidencetest.AddRecovered(t, rc.s.Case, other, make([]byte, 64<<10), evidencetest.RecoveredSpec{
		Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: "f2fs-rollforward",
		Path: "reports/p1-f2fs/rollforward.jsonl", FSType: "f2fs", Data: []byte("{}\n"),
		Mutate: func(r *evidence.Recovery) {
			r.Confidence = nil
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"}
			r.Params = map[string]string{"max_bytes": "1000"}
		},
	})
	rep := rfVerify(t, rc.s.Case)
	if got := rfProblems(rep); len(got) != 1 || !strings.Contains(got[0], "report") {
		t.Fatalf("rollforward problems %q", got)
	}
	evidencetest.RequireNotice(t, rep, `kind "report": 1 artifact(s)`)
}

func TestVerifyChecksIncludeRollForward(t *testing.T) {
	rc := newRFCase(t)
	rfVerify(t, rc.s.Case)
	runs := auditByAction(t, rc.s.Case, "verify.run")
	if len(runs) == 0 {
		t.Fatal("no verify.run entry")
	}
	if got := runs[len(runs)-1].Details["rollforward_rechecked"]; fmt.Sprint(got) != "5" {
		t.Errorf("verify.run rollforward_rechecked = %v, want 5", got)
	}
}

// truncated forges a post-checkpoint artifact of good's file, cut to its first bs bytes with runs, size and
// allocation summary rewritten consistently; mut adjusts the recovery record.
func (rc *rfCase) truncated(t *testing.T, path string, mut func(r *evidence.Recovery)) evidence.ManifestRecord {
	t.Helper()
	good := rc.files[0].Source.Derived
	cut := evidence.Run{Offset: good.Runs[0].Offset, Length: bs}
	return rc.craft(t, evidence.KindRecover, path, rc.disk[cut.Offset:cut.Offset+bs], func(d *evidence.Derivation) {
		d.FSPath, d.FSID = good.FSPath, good.FSID
		d.Runs = []evidence.Run{cut}
		rv := *good.Recovery
		rv.Assumptions = append([]string(nil), good.Recovery.Assumptions...)
		rv.Alloc = evidence.AllocSummary{Free: bs}
		mut(&rv)
		d.Recovery = &rv
	})
}

// 3H-R13 e: the artifact's size must be the fresh candidate's, or smaller with the size-declared assumption.
func TestVerifyRollForwardTruncatedWithoutAssumption(t *testing.T) {
	rc := newRFCase(t)
	rc.truncated(t, "recovered/p1-f2fs/000097-t.bin", func(*evidence.Recovery) {})
	got := rfProblems(rfVerify(t, rc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "size") {
		t.Fatalf("rollforward problems %q", got)
	}
}

func TestVerifyRollForwardTruncatedWithSizeDeclared(t *testing.T) {
	rc := newRFCase(t)
	size := rc.files[0].Source.Derived.Runs[0].Length
	rc.truncated(t, "recovered/p1-f2fs/000096-t.bin", func(r *evidence.Recovery) {
		r.Assumptions = append(r.Assumptions, fmt.Sprintf("size-declared=%d", size), "prefix-only")
	})
	if got := rfProblems(rfVerify(t, rc.s.Case)); len(got) != 0 {
		t.Fatalf("a declared cut verified with problems %q", got)
	}
}

func TestVerifyRollForwardTruncatedWrongSizeDeclared(t *testing.T) {
	rc := newRFCase(t)
	rc.truncated(t, "recovered/p1-f2fs/000095-t.bin", func(r *evidence.Recovery) {
		r.Assumptions = append(r.Assumptions, "size-declared=1")
	})
	if got := rfProblems(rfVerify(t, rc.s.Case)); len(got) != 1 {
		t.Fatalf("rollforward problems %q", got)
	}
}

func TestVerifyRollForwardMethodMismatch(t *testing.T) {
	rc := newRFCase(t)
	good := rc.files[0].Source.Derived
	rc.craft(t, evidence.KindRecover, "recovered/p1-f2fs/000094-m.bin", rc.disk[good.Runs[0].Offset:good.Runs[0].Offset+good.Runs[0].Length], func(d *evidence.Derivation) {
		d.FSPath, d.FSID = good.FSPath, good.FSID
		d.Runs = append([]evidence.Run(nil), good.Runs...)
		rv := *good.Recovery
		rv.Method = "f2fs-rollforward-other"
		d.Recovery = &rv
	})
	got := rfProblems(rfVerify(t, rc.s.Case))
	if len(got) != 1 || !strings.Contains(got[0], "method") {
		t.Fatalf("rollforward problems %q", got)
	}
}
