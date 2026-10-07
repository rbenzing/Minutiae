package records_test

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// forgedIngest ingests recs on a writer whose recovered-ness rule is switched
// off, as a buggy or hostile writer would: the batch digests are valid, so only
// verify rule R7 can object.
func forgedIngest(t *testing.T, c *evidence.Case, artifacts []string, recs ...records.Record) {
	t.Helper()
	w := newWriter(t, c, testParser, records.WriterOptions{})
	w.DisableRecoveredRule()
	if err := w.Start(ctx, records.StartOptions{Artifacts: artifacts}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
}

func requireOnlyProp(t *testing.T, rep evidence.VerifyReport, want ...string) {
	t.Helper()
	for _, w := range want {
		requireProblem(t, rep, w)
	}
	for _, bad := range []string{"digest mismatch", "derived chain"} {
		if p := problemsWith(rep, bad); len(p) != 0 {
			t.Errorf("unexpected %q problem: %q", bad, p)
		}
	}
	if len(rep.Problems) != len(want) {
		t.Errorf("want exactly %d problems, got %q", len(want), rep.Problems)
	}
}

func TestVerifyFlagsLiveRecordOnRecoveredArtifact(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	forgedIngest(t, c, []string{rec.ID}, liveRec(rec.ID))
	rep := mustVerify(t, c)
	requireOnlyProp(t, rep, "live record (recovered=0) on artifact")
	requireProblem(t, rep, "record 1:", rec.ID, "deleted-file")
}

func TestVerifyFlagsRecoveredRowAboveArtifactConfidence(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	forgedIngest(t, c, []string{rec.ID}, recoveredRec(rec.ID, ip(56)))
	rep := mustVerify(t, c)
	requireOnlyProp(t, rep, "confidence 56 is above the 55 of recovered artifact")
}

func TestVerifyFlagsRecoveredRowWithoutConfidence(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	forgedIngest(t, c, []string{rec.ID}, recoveredRec(rec.ID, nil))
	rep := mustVerify(t, c)
	requireOnlyProp(t, rep, "has no confidence, the recovered artifact")
	requireProblem(t, rep, "is capped at 55")
}

func TestVerifyFlagsLiveRecordOnDescendant(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "disk.img", mib)
	a := recordstest.AddRecovered(t, c, img, "recovered/p1-mtfs/000001-a.bin", []byte("abcdefgh"), ip(70))
	b := recordstest.AddDerived(t, c, a, "extracted/b.txt", "extract", []byte("abcd"), nil)
	forgedIngest(t, c, []string{b.ID}, liveRec(b.ID), recoveredRec(b.ID, ip(71)))
	rep := mustVerify(t, c)
	requireOnlyProp(t, rep, "live record (recovered=0) on artifact", "confidence 71 is above the 70 of recovered artifact")
}

func TestVerifyCleanRecoveredRecords(t *testing.T) {
	c, _, rec := recoveredCase(t, ip(55))
	w := startOn(t, c, rec.ID)
	for _, conf := range []int{55, 30} {
		if err := w.Add(ctx, recoveredRec(rec.ID, ip(conf))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.End(ctx); err != nil {
		t.Fatal(err)
	}
	if rep := mustVerify(t, c); !rep.OK() {
		t.Fatalf("problems: %q", rep.Problems)
	}
}

func TestVerifyR7DoesNotDoubleReportBrokenChain(t *testing.T) {
	c := recordstest.NewCase(t)
	img := recordstest.AddArtifact(t, c, "disk.img", mib)
	b := recordstest.AddDerived(t, c, img, "extracted/b.txt", "extract", []byte("abcdefgh"), nil)
	d := recordstest.AddDerived(t, c, b, "extracted/c.txt", "extract", []byte("abcd"), nil)
	recordstest.Ingest(t, c, testParser, []string{d.ID}, []records.Record{liveRec(d.ID)})
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recordstest.RemoveArtifactEverywhere(t, dir, b.ID)
	rep := mustVerify(t, openCase(t, dir))
	if n := countProblems(rep, "which is not in the manifest"); n != 1 {
		t.Errorf("the cut chain is reported %d times, want once: %q", n, rep.Problems)
	}
	if p := problemsWith(rep, "recovered"); len(p) != 0 {
		t.Errorf("R7 reported on a chain without a recovered artifact: %q", p)
	}
}
