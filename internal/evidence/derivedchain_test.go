package evidence

import (
	"fmt"
	"io"
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
		want string // "" = no problem; else a substring
	}{
		{"root, no derivation", chainOf(0), "a0", ""},
		{"one hop", chainOf(1), "a1", ""},
		{"16 hops is the limit and fine", chainOf(16), "a16", ""},
		{"17 hops is too deep", chainOf(17), "a17", "derived chain"},
		{"17 hops is too deep: names the limit", chainOf(17), "a17", "16"},
		{"an artifact inside the limit of a long chain is fine", chainOf(17), "a16", ""},
		{"a cycle", cyc, "a1", "cycle"},
		{"a cycle seen from a member", cyc, "a0", "derived chain"},
		{"an artifact deriving from itself", self, "s", "cycle"},
		{"missing parent: left to the single-hop check", missing, "a3", ""},
		{"wrong parent sha: left to the single-hop check", wrongSHA, "a3", ""},
		{"an id that is not in the manifest", chainOf(1), "ghost", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := derivedChainProblem(tc.by, tc.id)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("derivedChainProblem = %q, want no problem", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("derivedChainProblem = %q, want it to contain %q", got, tc.want)
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
