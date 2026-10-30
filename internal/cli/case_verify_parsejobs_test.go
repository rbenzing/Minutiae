package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

type verifyJSON struct {
	Problems []string `json:"problems"`
	Notices  []string `json:"notices"`
}

// verifyJSONOf runs `case verify --json` and decodes the report (an error line follows it on failure).
func verifyJSONOf(t *testing.T, dir string) (int, verifyJSON) {
	t.Helper()
	code, out := run(t, Deps{}, "case", "verify", "--case", dir, "--json")
	var rep verifyJSON
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&rep); err != nil {
		t.Fatalf("--json output: %v\n%s", err, out)
	}
	return code, rep
}

func parseJobsLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "parse jobs: ") {
			out = append(out, l)
		}
	}
	return out
}

// TestCaseVerifyComposesParseJobsCheck: `case verify` always composes the parse-job cross-check (4H, spec 17
// Q16): a case after a real parse run verifies clean, and the same case with the parse.job.start and
// parse.job.end of one job removed from a rechained audit log (so the chain alone cannot see it) exits 4 and
// names the ingest left without its job, in the text and in --json.
func TestCaseVerifyComposesParseJobsCheck(t *testing.T) {
	p := newPfx(t, "a", "b")
	if code, out, errOut := runP(t, registry(t, "", good("a"), good("b")), "parse", "run", "--case", p.dir); code != 0 {
		t.Fatalf("parse run: exit %d: %s %s", code, out, errOut)
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", p.dir); code != 0 || strings.Contains(out, "parse jobs:") {
		t.Fatalf("clean case: exit %d:\n%s", code, out)
	}
	code, rep := verifyJSONOf(t, p.dir)
	if code != 0 || len(parseJobsLines(rep.Problems)) != 0 || len(parseJobsLines(rep.Notices)) != 0 {
		t.Fatalf("clean case --json: exit %d, problems %q notices %q", code, rep.Problems, rep.Notices)
	}

	evidencetest.RechainAudit(t, p.dir, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		var out []evidence.AuditEntry
		start, end := false, false
		for _, e := range es {
			switch {
			case e.Action == artparse.ActionParseJobStart && !start:
				start = true
			case e.Action == artparse.ActionParseJobEnd && !end:
				end = true
			default:
				out = append(out, e)
			}
		}
		if !start || !end {
			t.Fatal("no parse.job.start or parse.job.end in the audit log")
		}
		return out
	})
	code, out := run(t, Deps{}, "case", "verify", "--case", p.dir)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM: parse jobs: ") {
		t.Fatalf("forged case: exit %d, want 4 with a parse jobs problem:\n%s", code, out)
	}

	code, rep = verifyJSONOf(t, p.dir)
	lines := parseJobsLines(rep.Problems)
	if code != ExitIntegrity || len(lines) != 1 || !strings.Contains(lines[0], "has no preceding parse.job.start") {
		t.Errorf("--json: exit %d, parse jobs problems %q, want exactly the ingest without its job start", code, lines)
	}
}
