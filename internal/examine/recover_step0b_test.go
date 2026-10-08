package examine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
)

// C59: exactly 16 candidates are all considered with no cut warning; 16 overlapping partners are listed
// with no "further runs" attribute.
func TestRecoverExactlySixteenCandidatesIsNotCut(t *testing.T) {
	maps := make([]fstest.RecoverMap, filesys.MaxCandidatesPerEntry)
	for i := range maps {
		maps[i] = fstest.RecoverMap{Method: mtfsRecM}
	}
	e := newRecEnv(t, delNode("/a.bin", pat(1, bs), maps...))
	plan, err := e.s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true, AllCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || len(plan.Items[0].Candidates) != filesys.MaxCandidatesPerEntry {
		t.Fatalf("plan = %+v, want all 16 candidates", plan.Items)
	}
	e.recover(t, examine.RecoverOptions{All: true, AllCandidates: true})
	if anyContains(warnReasons(t, e.c), "candidates offered") {
		t.Errorf("warnings %q report a cut at exactly 16", warnReasons(t, e.c))
	}
}

func TestRecoverSixteenOverlapPartnersHaveNoFurtherRuns(t *testing.T) {
	const n = 17 // 16 partners each
	buildBS = 16384
	t.Cleanup(func() { buildBS = bs })
	nodes := []fstest.Node{delNode("/e00.bin", pat(1, bs))}
	for i := 1; i < n; i++ {
		nodes = append(nodes, delNode(fmt.Sprintf("/e%02d.bin", i), pat(byte(i+1), bs)))
	}
	lay := layout(t, nodes)
	for i := 1; i < n; i++ {
		nodes[i].Recover = []fstest.RecoverMap{{Method: mtfsRecM, Size: bs, Runs: lay["/e00.bin"]}}
	}
	e := newRecEnv(t, nodes...)
	e.recover(t, examine.RecoverOptions{All: true})
	recs := recovered(t, e.c)
	if len(recs) != n {
		t.Fatalf("%d artifacts, want %d", len(recs), n)
	}
	for _, r := range recs {
		as := r.Source.Derived.Recovery.Assumptions
		listed := 0
		for _, a := range as {
			listed += btoi(strings.HasPrefix(a, "overlap="))
			if strings.HasPrefix(a, "overlap-other-runs") {
				t.Errorf("%s: %q present with exactly 16 partners", r.Source.Derived.FSPath, a)
			}
		}
		if listed != 16 {
			t.Errorf("%s: %d overlap partners listed, want 16", r.Source.Derived.FSPath, listed)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
