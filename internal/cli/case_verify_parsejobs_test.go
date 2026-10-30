package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// TestCaseVerifyComposesParseJobsCheck: `case verify` always composes the parse-job cross-check (4H, spec 17
// Q16): a case after a real parse run verifies clean, and the same case with the parse.job.start of one job
// removed from a rechained audit log (so the chain alone cannot see it) exits 4 and names the problem,
// in the text and in --json.
func TestCaseVerifyComposesParseJobsCheck(t *testing.T) {
	p := newPfx(t, "a", "b")
	if code, out, errOut := runP(t, registry(t, "", good("a"), good("b")), "parse", "run", "--case", p.dir); code != 0 {
		t.Fatalf("parse run: exit %d: %s %s", code, out, errOut)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", p.dir); code != 0 || strings.Contains(out, "parse jobs:") {
		t.Fatalf("clean case: exit %d:\n%s", code, out)
	}

	evidencetest.RechainAudit(t, p.dir, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		for i, e := range es {
			if e.Action == artparse.ActionParseJobStart {
				return append(es[:i:i], es[i+1:]...)
			}
		}
		t.Fatal("no parse.job.start in the audit log")
		return es
	})
	code, out := run(t, Deps{}, "case", "verify", "--case", p.dir)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM: parse jobs: ") {
		t.Fatalf("forged case: exit %d, want 4 with a parse jobs problem:\n%s", code, out)
	}

	code, out = run(t, Deps{}, "case", "verify", "--case", p.dir, "--json")
	if code != ExitIntegrity {
		t.Fatalf("--json: exit %d:\n%s", code, out)
	}
	var rep struct {
		Problems []string `json:"problems"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&rep); err != nil { // the error line follows the report
		t.Fatalf("--json output: %v\n%s", err, out)
	}
	found := false
	for _, pr := range rep.Problems {
		found = found || strings.HasPrefix(pr, "parse jobs: ")
	}
	if !found {
		t.Errorf("--json problems %q hold no parse jobs problem", rep.Problems)
	}
}
