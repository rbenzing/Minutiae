package examine_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// The reader's method token ([a-z][a-z0-9-]{1,31}, filesys.CheckCandidate) must be a subset of the
// evidence token ([a-z][a-z0-9_-]{1,63}, evidence.Recovery.Check), so a candidate a reader may offer
// can always be recorded. The two live in packages that may not import each other; this test, in
// the package that may import both, ties them together.
func TestReaderMethodTokenIsEvidenceValid(t *testing.T) {
	var tokens []string
	const first, rest = "abcdefghijklmnopqrstuvwxyz", "az09-"
	for _, f := range first {
		for _, r := range rest {
			tokens = append(tokens, string(f)+string(r))
		}
		tokens = append(tokens, string(f)+strings.Repeat("z9-a", 7)+"zz") // 1+30 = 31 chars after the first
		tokens = append(tokens, string(f)+strings.Repeat("-", 31))        // the 32-char maximum
	}
	checked := 0
	for _, m := range tokens {
		c := filesys.Candidate{Method: m}
		if _, err := filesys.CheckCandidate(c, 0); err != nil {
			continue // not a reader token
		}
		checked++
		for _, p := range (&evidence.Recovery{Method: m}).Check("deleted-file") {
			if strings.Contains(p, "is not a token") {
				t.Errorf("reader-valid method %q is not evidence-valid: %s", m, p)
			}
		}
	}
	if checked < 26*5 {
		t.Fatalf("only %d tokens were reader-valid; the generator is vacuous", checked)
	}
	// The sets differ (the evidence token allows '_' and 64 characters): the reader's is the stricter.
	if _, err := filesys.CheckCandidate(filesys.Candidate{Method: "ab_c"}, 0); err == nil {
		t.Error("the reader token must refuse '_'")
	}
}
