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

// markIDs is a check that marks ids as rechecked by R12, once per call.
func markIDs(calls ...[]string) evidence.VerifyCheck {
	return func(_ context.Context, _ *evidence.Case, _ []evidence.ManifestRecord, r *evidence.VerifyReport) {
		for _, ids := range calls {
			r.MarkRollForwardRan(ids)
		}
	}
}

// 3H-R13 e, ruling C7: MarkRollForwardRan takes ids, and only a real f2fs-rollforward report that was marked
// is subtracted from the kind-"report" notice, once, however often it is marked.
func TestVerifyReportNoticeCountsUncheckedReports(t *testing.T) {
	c, parent, data := recoverParent(t)
	add := func(method, name string) string {
		return evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
			Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: method,
			Path: "reports/p1-mtfs/" + name, Runs: []evidence.Run{{Offset: 0, Length: 64}},
		}).ID
	}
	rf1, rf2 := add("f2fs-rollforward", "rollforward1.jsonl"), add("f2fs-rollforward", "rollforward2.jsonl")
	ext4 := add("ext4-journal", "journal.jsonl")
	const pre = `kind "report": `
	const post = ` artifact(s) are not reproduced byte for byte`
	for _, tc := range []struct {
		name  string
		check evidence.VerifyCheck
		want  int // -1 = no notice
	}{
		{"nothing marked", nil, 3},
		{"nil ids", markIDs(nil), 3},
		{"empty ids", markIDs([]string{}), 3},
		{"one f2fs report marked leaves the ext4 one", markIDs([]string{rf1}), 2},
		{"both f2fs reports marked, the ext4 one still counts", markIDs([]string{rf1, rf2}), 1},
		{"repeated calls and duplicate ids count once", markIDs([]string{rf1, rf1}, []string{rf1}, []string{rf1}), 2},
		{"a bogus id and the ext4 id reduce nothing", markIDs([]string{"nosuchid", ext4}), 3},
		{"over-marking next to a real one", markIDs([]string{rf1, "nosuchid", ext4, parent.ID}), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks []evidence.VerifyCheck
			if tc.check != nil {
				checks = append(checks, tc.check)
			}
			rep := verifyOf(t, c, checks...)
			evidencetest.RequireNotice(t, rep, fmt.Sprintf("%s%d%s", pre, tc.want, post))
		})
	}
	t.Run("all real f2fs reports of a case without ext4", func(t *testing.T) {
		c2, parent2, data2 := recoverParent(t)
		r := evidencetest.AddRecovered(t, c2, parent2, data2, evidencetest.RecoveredSpec{
			Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: "f2fs-rollforward",
			Path: "reports/p1-mtfs/rollforward.jsonl", Runs: []evidence.Run{{Offset: 0, Length: 64}},
		})
		requireNoNotice(t, verifyOf(t, c2, markIDs([]string{r.ID})), `kind "report"`)
	})
}

// The R9 notice counts post-checkpoint files and f2fs-rollforward block copies when R9 is not composed.
func TestVerifyAllocationNoticeCountsRollForwardArtifacts(t *testing.T) {
	c, parent, data := recoverParent(t)
	evidencetest.AddRecovered(t, c, parent, data, rfFile(60, nil))
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindJournal, Class: evidence.ClassJournalBlock, Method: "f2fs-rollforward", Path: "journal/p1-mtfs/j1.bin",
		Runs:   []evidence.Run{{Offset: 0, Length: 64}},
		Mutate: func(r *evidence.Recovery) { r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"} },
	})
	evidencetest.AddRecovered(t, c, parent, data, evidencetest.RecoveredSpec{
		Kind: evidence.KindJournal, Class: evidence.ClassJournalBlock, Method: "ext4-journal", Path: "journal/p1-mtfs/j2.bin",
		Runs:   []evidence.Run{{Offset: 0, Length: 64}},
		Mutate: func(r *evidence.Recovery) { r.Journal = &evidence.JournalRef{Seq: 2, Region: "live"} },
	})
	evidencetest.RequireNotice(t, verifyOf(t, c), "2 deleted-file or carved artifact(s): allocation not rechecked against the filesystem")
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

// 3G-R18: the kind-report notice counts the reports that neither R12 nor R13 recomputed; a mark of one family never
// silences the other's.
func TestVerifyReportNoticeCountsBothMarks(t *testing.T) {
	report := func(method, path string) evidencetest.RecoveredSpec {
		return evidencetest.RecoveredSpec{
			Kind: evidence.KindReport, Class: evidence.ClassJournalReport, Method: method, Path: path,
			Runs: []evidence.Run{{Offset: 0, Length: 64}},
		}
	}
	none := func(_, _ string) []string { return nil }
	onlyF := func(f, _ string) []string { return []string{f} }
	onlyE := func(_, e string) []string { return []string{e} }
	both := func(f, e string) []string { return []string{f, e} }
	for _, tc := range []struct {
		name       string
		r12, r13   func(f, e string) []string
		wantNotice string // "" = no kind-report notice
	}{
		{"neither", none, none, `kind "report": 2 artifact(s)`},
		{"only R12", onlyF, none, `kind "report": 1 artifact(s)`},
		{"only R13", none, onlyE, `kind "report": 1 artifact(s)`},
		{"both", onlyF, onlyE, ""},
		{"R12 names the ext4 report", onlyE, none, `kind "report": 2 artifact(s)`},
		{"R13 names the F2FS report", none, onlyF, `kind "report": 2 artifact(s)`},
		{"both name both", both, both, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, data := recoverParent(t)
			rf := evidencetest.AddRecovered(t, c, parent, data, report("f2fs-rollforward", "reports/p1-mtfs/rollforward.jsonl"))
			ex := evidencetest.AddRecovered(t, c, parent, data, report("ext4-journal", "reports/p1-mtfs/journal.jsonl"))
			mark := func(_ context.Context, _ *evidence.Case, _ []evidence.ManifestRecord, rep *evidence.VerifyReport) {
				rep.MarkRollForwardRan(tc.r12(rf.ID, ex.ID))
				rep.MarkJournalReportsChecked(tc.r13(rf.ID, ex.ID))
			}
			rep, err := c.VerifyWith(context.Background(), mark)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNotice == "" {
				for _, n := range rep.Notices {
					if strings.Contains(n, `kind "report"`) {
						t.Errorf("notice remains: %s", n)
					}
				}
				return
			}
			evidencetest.RequireNotice(t, rep, tc.wantNotice)
		})
	}
}
