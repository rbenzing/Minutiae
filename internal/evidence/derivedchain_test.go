package evidence

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// chainOf builds a manifest map a0 <- a1 <- ... <- an (a0 is the root, a_i is
// derived from a_{i-1}), every hop with the parent's real sha256.
func chainOf(n int) map[string]ManifestRecord {
	m := map[string]ManifestRecord{}
	for i := 0; i <= n; i++ {
		r := ManifestRecord{ID: fmt.Sprintf("a%d", i), SHA256: fmt.Sprintf("sha%d", i)}
		if i > 0 {
			r.Source.Derived = &Derivation{ParentID: fmt.Sprintf("a%d", i-1), ParentSHA256: fmt.Sprintf("sha%d", i-1)}
		}
		m[r.ID] = r
	}
	return m
}

// TestDerivedChainWalk: the multi-hop part of the derived-chain check. A hop that
// is missing or carries the wrong parent hash is reported by the single-hop check
// (checkDerived), not here, so one broken parent is reported once (ruling R14).
func TestDerivedChainWalk(t *testing.T) {
	cyc := chainOf(2)
	a0 := cyc["a0"]
	a0.Source.Derived = &Derivation{ParentID: "a2", ParentSHA256: "sha2"}
	cyc["a0"] = a0

	self := map[string]ManifestRecord{"s": {ID: "s", Source: Source{Derived: &Derivation{ParentID: "s"}}}}

	missing := chainOf(3)
	delete(missing, "a1")

	wrongSHA := chainOf(3)
	a2 := wrongSHA["a2"]
	a2.Source.Derived = &Derivation{ParentID: "a1", ParentSHA256: "not-the-parent"}
	wrongSHA["a2"] = a2

	tests := []struct {
		name string
		by   map[string]ManifestRecord
		id   string
		want chainResult
	}{
		{"root, no derivation", chainOf(0), "a0", chainResult{}},
		{"one hop", chainOf(1), "a1", chainResult{}},
		{"16 hops is the limit and fine", chainOf(16), "a16", chainResult{}},
		{"17 hops is too deep", chainOf(17), "a17", chainResult{TooDeep: true}},
		{"an artifact inside the limit of a long chain is fine", chainOf(17), "a16", chainResult{}},
		{"a cycle", cyc, "a1", chainResult{Cycle: []string{"a0", "a1", "a2"}}},
		{"a cycle seen from a member", cyc, "a0", chainResult{Cycle: []string{"a0", "a1", "a2"}}},
		{"an artifact deriving from itself", self, "s", chainResult{Cycle: []string{"s"}}},
		{"missing parent: left to the single-hop check", missing, "a3", chainResult{}},
		{"wrong parent sha: left to the single-hop check", wrongSHA, "a3", chainResult{}},
		{"an id that is not in the manifest", chainOf(1), "ghost", chainResult{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := derivedChainWalk(tc.by, tc.id)
			if got.TooDeep != tc.want.TooDeep || !slices.Equal(got.Cycle, tc.want.Cycle) {
				t.Fatalf("derivedChainWalk = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// captureChain captures a root and n artifacts each derived from the previous
// one (with the parent's real sha256) and returns them in order.
func captureChain(t *testing.T, c *Case, n int) []ManifestRecord {
	t.Helper()
	var out []ManifestRecord
	for i := 0; i <= n; i++ {
		src := Source{Kind: "file", DeviceID: "dev1"}
		if i > 0 {
			p := out[i-1]
			src.Derived = &Derivation{ParentID: p.ID, ParentSHA256: p.SHA256}
		}
		rec, err := c.Capture("dev1", "acq1", fmt.Sprintf("chain%d", i), src, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "artifact %d", i)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func countContaining(list []string, sub string) int {
	n := 0
	for _, s := range list {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func TestVerifyDerivedChainDepthLimit(t *testing.T) {
	c := newTestCase(t)
	captureChain(t, c, 16)
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("a chain of 16 hops must verify: %q", r.Problems)
	}

	c2 := newTestCase(t)
	chain := captureChain(t, c2, 17)
	r := mustVerify(t, c2)
	if n := countContaining(r.Problems, "derived chain"); n != 1 || len(r.Problems) != 1 {
		t.Fatalf("problems = %q, want exactly one derived chain problem", r.Problems)
	}
	if !strings.Contains(r.Problems[0], chain[17].ID) {
		t.Errorf("the problem does not name the deepest artifact %s: %s", chain[17].ID, r.Problems[0])
	}
}

// TestBrokenParentIsReportedOnce: a missing parent and a wrong parent hash are
// reported by the single-hop check only; the multi-hop walk adds nothing.
func TestBrokenParentIsReportedOnce(t *testing.T) {
	c := newTestCase(t)
	root, err := c.Capture("dev1", "acq1", "root", testSrc, func(w io.Writer) error { _, err := io.WriteString(w, "root"); return err })
	if err != nil {
		t.Fatal(err)
	}
	mid, err := c.Capture("dev1", "acq1", "mid", Source{Kind: "file", DeviceID: "dev1", Derived: &Derivation{ParentID: root.ID, ParentSHA256: "wrong"}},
		func(w io.Writer) error { _, err := io.WriteString(w, "mid"); return err })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capture("dev1", "acq1", "leaf", Source{Kind: "file", DeviceID: "dev1", Derived: &Derivation{ParentID: mid.ID, ParentSHA256: mid.SHA256}},
		func(w io.Writer) error { _, err := io.WriteString(w, "leaf"); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capture("dev1", "acq1", "orphan", Source{Kind: "file", DeviceID: "dev1", Derived: &Derivation{ParentID: "ghost", ParentSHA256: "x"}},
		func(w io.Writer) error { _, err := io.WriteString(w, "orphan"); return err }); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if len(r.Problems) != 2 {
		t.Fatalf("problems = %q, want one for the wrong hash and one for the missing parent", r.Problems)
	}
	if countContaining(r.Problems, "differs from the derivation's recorded parent sha256") != 1 || countContaining(r.Problems, `"ghost", which is not in the manifest`) != 1 {
		t.Errorf("problems = %q", r.Problems)
	}
	if countContaining(r.Problems, "derived chain") != 0 {
		t.Errorf("the walk repeated a single-hop problem: %q", r.Problems)
	}
}

// cycleRecs hand-builds a cycle of n artifacts <prefix>00.. (each derived from the next, the last
// from the first) and `tails` artifacts <prefix>t0.. deriving from the first member, every hop
// carrying its parent's real SHA256. Capture generates ids and cannot close a cycle.
func cycleRecs(prefix string, n, tails int) []ManifestRecord {
	id := func(i int) string { return fmt.Sprintf("%s%02d", prefix, i%n) }
	var out []ManifestRecord
	for i := 0; i < n; i++ {
		out = append(out, ManifestRecord{
			ID: id(i), Path: id(i), SHA256: "sha-" + id(i),
			Source: Source{Derived: &Derivation{ParentID: id(i + 1), ParentSHA256: "sha-" + id(i+1)}},
		})
	}
	for i := 0; i < tails; i++ {
		tid := fmt.Sprintf("%st%d", prefix, i)
		out = append(out, ManifestRecord{
			ID: tid, Path: tid, SHA256: "sha-" + tid,
			Source: Source{Derived: &Derivation{ParentID: id(0), ParentSHA256: "sha-" + id(0)}},
		})
	}
	return out
}

func derivedProblems(t *testing.T, recs []ManifestRecord) []string {
	t.Helper()
	c := newTestCase(t)
	var rep VerifyReport
	c.checkDerived(&rep, recs)
	return rep.Problems
}

func TestVerifyReportsACycleOnce(t *testing.T) {
	probs := derivedProblems(t, cycleRecs("c", 5, 2))
	if len(probs) != 1 {
		t.Fatalf("problems = %q, want exactly one", probs)
	}
	p := probs[0]
	for _, want := range []string{"derived chain has a cycle", "5 artifacts", "2 further", `"c00"`, `"c04"`} {
		if !strings.Contains(p, want) {
			t.Errorf("problem %q lacks %q", p, want)
		}
	}
	if i0, i4 := strings.Index(p, `"c00"`), strings.Index(p, `"c04"`); i0 < 0 || i4 < i0 {
		t.Errorf("member ids not in sorted order: %q", p)
	}
	if rep := (VerifyReport{Problems: probs}); rep.OK() {
		t.Error("a report with a cycle problem is OK")
	}
}

func TestVerifyReportsTwoCyclesTwice(t *testing.T) {
	recs := append(cycleRecs("z", 2, 0), cycleRecs("b", 3, 0)...)
	probs := derivedProblems(t, recs)
	if len(probs) != 2 {
		t.Fatalf("problems = %q, want two", probs)
	}
	if !strings.Contains(probs[0], "3 artifacts") || !strings.Contains(probs[1], "2 artifacts") {
		t.Errorf("order is not by smallest id (b before z): %q", probs)
	}
}

func TestVerifyCycleListsAtMostEightMembers(t *testing.T) {
	probs := derivedProblems(t, cycleRecs("m", 12, 0))
	if len(probs) != 1 {
		t.Fatalf("problems = %q", probs)
	}
	if n := strings.Count(probs[0], `"m`); n != 8 {
		t.Errorf("%d ids named, want 8: %s", n, probs[0])
	}
	if !strings.Contains(probs[0], "and 4 more") || !strings.Contains(probs[0], "12 artifacts") {
		t.Errorf("problem = %s", probs[0])
	}
}
