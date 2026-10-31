package evidence_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// rfFile is a post-checkpoint-file artifact on the recoverParent image (3H-R8, R13).
func rfFile(conf int, a *evidence.AllocSummary) evidencetest.RecoveredSpec {
	return evidencetest.RecoveredSpec{
		Class: evidence.ClassPostCheckpointFile, Method: "f2fs-rollforward", Path: "recovered/p1-mtfs/000001-a.bin",
		Runs: []evidence.Run{{Offset: 0, Length: 64}}, Confidence: &conf, Alloc: a,
	}
}

// 3H-R8: the ceiling of the class is 60 (a judgement, spec amendment), so case verify refuses more.
func TestVerifyPostCheckpointCeilingIs60(t *testing.T) {
	for _, tc := range []struct {
		conf int
		want string // "" = clean
	}{
		{60, ""},
		{20, ""},
		{61, `recovery: confidence 61 is above the ceiling 60 of class "post-checkpoint-file"`},
		{80, `recovery: confidence 80 is above the ceiling 60 of class "post-checkpoint-file"`},
	} {
		t.Run(fmt.Sprint(tc.conf), func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, rfFile(tc.conf, nil))
			rep := verifyOf(t, c)
			if tc.want == "" {
				evidencetest.RequireClean(t, rep)
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

// 3H-R13 a: a post-checkpoint file is held to the deleted-file rule, no allocated and no unknown byte (R5).
func TestVerifyPostCheckpointAllocatedBytesIsAProblem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alloc evidence.AllocSummary
		want  string
	}{
		{"allocated", evidence.AllocSummary{Free: 60, Allocated: 4}, "4 allocated bytes in a post-checkpoint-file artifact"},
		{"unknown", evidence.AllocSummary{Free: 54, Unknown: 10}, "10 unknown bytes in a post-checkpoint-file artifact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, rfFile(60, &tc.alloc))
			evidencetest.RequireProblem(t, verifyOf(t, c), tc.want)
		})
	}
}

// 3H-R13 b: a report artifact sits under p<N>-<fstype> of its own partition, like a journal block.
func TestVerifyReportPathNeedsPartitionDir(t *testing.T) {
	report := func(path string) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{
			Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: "f2fs-rollforward", Path: path,
			Runs: []evidence.Run{{Offset: 0, Length: 64}},
		}
	}
	for _, tc := range []struct {
		name, path, want string
	}{
		{"own partition", "reports/p1-mtfs/rollforward.jsonl", ""},
		{"no partition directory", "reports/rollforward.jsonl", "has no partition directory below the namespace"},
		{"another partition", "reports/p2-mtfs/rollforward.jsonl", `path directory "p2-mtfs" does not match derivation partition 1 ("mtfs")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			evidencetest.AddRecovered(t, c, parent, data, report(tc.path))
			rep := verifyOf(t, c)
			if tc.want == "" {
				evidencetest.RequireClean(t, rep)
				return
			}
			evidencetest.RequireProblem(t, rep, tc.want)
		})
	}
}

// 3H-R13 e: the kind-"report" notice counts only the reports the roll-forward check did not recheck.
func TestVerifyReportNoticeCountsUncheckedReports(t *testing.T) {
	c, parent, data := recoverParent(t)
	for i := 1; i <= 2; i++ {
		evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
			Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: "f2fs-rollforward",
			Path: fmt.Sprintf("reports/p1-mtfs/rollforward%d.jsonl", i), Runs: []evidence.Run{{Offset: 0, Length: 64}},
		})
	}
	// without the check: both are unreproduced
	rep := verifyOf(t, c)
	evidencetest.RequireNotice(t, rep, `kind "report": 2 artifact(s) are not reproduced byte for byte`)
	// with it having rechecked one of the two
	mark := func(n int) evidence.VerifyCheck {
		return func(_ context.Context, _ *evidence.Case, _ []evidence.ManifestRecord, r *evidence.VerifyReport) {
			r.MarkRollForwardRan(n)
		}
	}
	rep = verifyOf(t, c, mark(1))
	evidencetest.RequireNotice(t, rep, `kind "report": 1 artifact(s) are not reproduced byte for byte`)
	// all rechecked: no notice
	rep = verifyOf(t, c, mark(2))
	requireNoNotice(t, rep, `kind "report"`)
}

// 3H-R13 c: once the reproduce check ran, the block copies of kind journal are reproduced too.
func TestVerifyJournalNoticeGoneWhenReproduceRan(t *testing.T) {
	c, parent, data := recoverParent(t)
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindJournal, Class: evidence.ClassJournalBlock, Method: "f2fs-rollforward", Path: "journal/p1-mtfs/j1.bin",
		Runs:   []evidence.Run{{Offset: 0, Length: 64}},
		Mutate: func(r *evidence.Recovery) { r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"} },
	})
	evidencetest.RequireNotice(t, verifyOf(t, c), `kind "journal": 1 artifact(s) are not reproduced byte for byte`)
	rep := verifyOf(t, c, func(_ context.Context, _ *evidence.Case, _ []evidence.ManifestRecord, r *evidence.VerifyReport) {
		r.MarkReproduceRan()
	})
	requireNoNotice(t, rep, `kind "journal"`)
}

func requireNoNotice(t *testing.T, rep evidence.VerifyReport, substr string) {
	t.Helper()
	for _, n := range rep.Notices {
		if strings.Contains(n, substr) {
			t.Fatalf("unexpected notice %q", n)
		}
	}
}
