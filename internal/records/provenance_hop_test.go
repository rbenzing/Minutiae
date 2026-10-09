package records

import (
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestChainProblemHopIndexes(t *testing.T) {
	t.Run("parent hash differs is hop 1", func(t *testing.T) {
		root := pnode("root", "")
		diff := pnode("child", "root")
		diff.Source.Derived.ParentSHA256 = "sha-other"
		p := resolve(t, []evidence.ManifestRecord{root, diff}, "child")
		if len(p.Problems) != 1 || p.Problems[0].Kind != ProblemParentHash || p.Problems[0].Hop != 1 {
			t.Fatalf("%+v", p.Problems)
		}
	})
	t.Run("audit problem at hop 1", func(t *testing.T) {
		root := pnode("root", "")
		a := pnode("a", "root")
		p := resolveChain([]evidence.ManifestRecord{root, a}, evidence.NewAuditIndex(auditOf(t, a)), "a")
		if len(p.Problems) != 1 || p.Problems[0].Kind != ProblemAuditMissing || p.Problems[0].Hop != 1 {
			t.Fatalf("%+v", p.Problems)
		}
	})
	t.Run("segment problems at hop 1", func(t *testing.T) {
		refs, recs := segs(3, func(i int) string {
			if i == 1 {
				return "missing"
			}
			return ""
		})
		man := append(withSegments(refs, recs), pnode("top", "part"))
		p := resolve(t, man, "top")
		if len(p.Problems) != 1 || p.Problems[0].Kind != ProblemSegment || p.Problems[0].Hop != 1 {
			t.Fatalf("%+v", p.Problems)
		}
		if p.Chain[1].SegmentsBad != 1 || p.Chain[0].SegmentsBad != 0 {
			t.Fatalf("%+v", p.Chain)
		}
	})
}

func TestProblemSetAddCapsEachKind(t *testing.T) {
	ps := &problemSet{perKind: map[ProblemKind]int{}}
	for i := 0; i < MaxProblemsPerKind+1; i++ {
		ps.add(ProblemCycle, i, fmt.Sprint(i))
	}
	if len(ps.list) != MaxProblemsPerKind || ps.suppressed != 1 || !ps.full(ProblemCycle) || ps.full(ProblemTooDeep) {
		t.Fatalf("listed %d suppressed %d", len(ps.list), ps.suppressed)
	}
	ps.add(ProblemTooDeep, 0, "x")
	if len(ps.list) != MaxProblemsPerKind+1 || ps.suppressed != 1 {
		t.Fatalf("another kind has its own cap: %d %d", len(ps.list), ps.suppressed)
	}
}

func TestChainNilAuditIndexIsEveryHopMissing(t *testing.T) {
	man := []evidence.ManifestRecord{pnode("a", "b"), pnode("b", "c"), pnode("c", "")}
	p := resolveChain(man, nil, "a")
	if len(p.Chain) != 3 || len(p.Problems) != 3 || p.OK() {
		t.Fatalf("%+v", p.Problems)
	}
	for i, pr := range p.Problems {
		if pr.Kind != ProblemAuditMissing || pr.Hop != i || p.Chain[i].Audit.State != evidence.AuditMissing {
			t.Fatalf("hop %d: %+v", i, pr)
		}
	}
}
