package evidence_test

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func TestRecoveryCheckSlackTakesNoScope(t *testing.T) {
	r := evidence.Recovery{Class: evidence.ClassSlack, Method: evidence.MethodSlackFile, Confidence: ip(10), Algorithm: "x"}
	if got := r.Check(evidence.KindSlack); len(got) != 0 {
		t.Errorf("slack without scope: %v, want none", got)
	}
	if ci, ok := evidence.LookupClass(evidence.ClassSlack); !ok || ci.Scope != "" || ci.ScopeRequired {
		t.Errorf("slack class row %+v (found %v): want no scope and none required", ci, ok)
	}
	r.Scope = "unallocated"
	got := r.Check(evidence.KindSlack)
	if len(got) != 1 || !strings.Contains(got[0], `is not allowed for class "slack"`) {
		t.Errorf("slack with scope unallocated: %v, want one problem containing %q", got, `is not allowed for class "slack"`)
	}
}

func TestCheckRecoveredRunsSlackRules(t *testing.T) {
	const n = 100
	mk := func(method string, a evidence.AllocSummary) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: "a1", Path: "artifacts/d/a/slack/p1-fat/slack.bin", Size: n, Source: evidence.Source{Kind: evidence.KindSlack, Derived: &evidence.Derivation{
			Recovery: &evidence.Recovery{Class: evidence.ClassSlack, Method: method, Alloc: a},
		}}}
	}
	pre := `artifact "a1" ("artifacts/d/a/slack/p1-fat/slack.bin"): `
	runs := []evidence.Run{{Offset: 4096, Length: n}}
	file, vol := evidence.MethodSlackFile, evidence.MethodSlackVolume
	freeMsg := func(n int) string {
		return fmt.Sprintf("%d free bytes in a slack artifact (slack lies in allocated units or outside the filesystem)", n)
	}
	allocMsg := func(n int) string { return fmt.Sprintf("%d allocated bytes in a volume-slack artifact", n) }
	for _, tc := range []struct {
		name   string
		method string
		alloc  evidence.AllocSummary
		want   []string
	}{
		{"file allocated", file, evidence.AllocSummary{Allocated: n}, nil},
		{"file unknown", file, evidence.AllocSummary{Unknown: n}, nil},
		{"file allocated and unknown", file, evidence.AllocSummary{Allocated: 60, Unknown: 40}, nil},
		{"file with a free byte", file, evidence.AllocSummary{Free: 1, Allocated: n - 1}, []string{pre + freeMsg(1)}},
		{"file all free", file, evidence.AllocSummary{Free: n}, []string{pre + freeMsg(100)}},
		{"volume unknown", vol, evidence.AllocSummary{Unknown: n}, nil},
		{"volume with an allocated byte", vol, evidence.AllocSummary{Allocated: 1, Unknown: n - 1}, []string{pre + allocMsg(1)}},
		{"volume with a free byte", vol, evidence.AllocSummary{Free: 1, Unknown: n - 1}, []string{pre + freeMsg(1)}},
		{"volume with both", vol, evidence.AllocSummary{Free: 1, Allocated: 2, Unknown: n - 3}, []string{pre + freeMsg(1), pre + allocMsg(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evidence.CheckRecoveredRuns(mk(tc.method, tc.alloc), runs)
			if !slices.Equal(got, tc.want) {
				t.Errorf("CheckRecoveredRuns = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerifySlackArtifactShape(t *testing.T) {
	file := []evidence.SlackMapLine{
		{ImageOffset: 4796, Length: 324, FSPath: "/a.txt", FSID: "dirent:2:3", FileSize: 700, Exported: true},
		{ImageOffset: 9000, Length: 64, FSPath: "/b.txt", FSID: "dirent:2:4", FileSize: 3, Uniform: true, Exported: true},
	}
	vol := []evidence.SlackMapLine{{ImageOffset: 8192, Length: 4096, Uniform: true, Exported: true}, {ImageOffset: 12288, Length: 100, Exported: true}}

	t.Run("file and volume artifacts are clean under R1-R5", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{Lines: file})
		evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{Method: evidence.MethodSlackVolume, Lines: vol})
		rep := verifyOf(t, c)
		evidencetest.RequireClean(t, rep)
		evidencetest.RequireNotice(t, rep, `kind "slack": 2 artifact(s) are not reproduced byte for byte by this build`)
	})
	t.Run("a forged free count", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{Lines: file, Mutate: func(r *evidence.Recovery) {
			r.Alloc.Free, r.Alloc.Allocated = 1, r.Alloc.Allocated-1
		}})
		evidencetest.RequireProblem(t, verifyOf(t, c), "1 free bytes in a slack artifact")
	})
	t.Run("an allocated byte in volume slack", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{Method: evidence.MethodSlackVolume, Lines: vol, Mutate: func(r *evidence.Recovery) {
			r.Alloc.Allocated, r.Alloc.Unknown = 1, r.Alloc.Unknown-1
		}})
		evidencetest.RequireProblem(t, verifyOf(t, c), "1 allocated bytes in a volume-slack artifact")
	})
	t.Run("a sidecar in another directory", func(t *testing.T) {
		c, parent, data := recoverParent(t)
		_, sc := evidencetest.AddSlack(t, c, parent, data, evidencetest.SlackSpec{Lines: file})
		evidencetest.RewriteManifest(t, c.Dir, sc.ID, func(r *evidence.ManifestRecord) {
			r.Path = fmt.Sprintf("%s/sub/%s", path.Dir(r.Path), path.Base(r.Path))
		})
		evidencetest.RequireProblem(t, verifyOf(t, c), "is not a runs sidecar in the same directory")
	})
}
