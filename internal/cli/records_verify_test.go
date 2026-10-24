package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// splitRun runs a command with stdout and stderr captured apart.
func splitRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runSplit(context.Background(), t, Deps{}, args...)
}

func countAction(acts []string, want string) int {
	n := 0
	for _, a := range acts {
		if a == want {
			n++
		}
	}
	return n
}

// recordsReads are the four records reads, each with a command that succeeds on recDataset(t, false).
func recordsReads(dir string) [][]string {
	return [][]string{
		{"records", "list", "--case", dir},
		{"records", "show", "1", "--case", dir},
		{"records", "stats", "--case", dir},
		{"records", "search", "hello", "--case", dir},
	}
}

func TestRecordsListPrintsNotVerifiedNotice(t *testing.T) {
	rc := recDataset(t, false)
	code, out, errOut := splitRun(t, "records", "list", "--case", rc.dir)
	if code != 0 || !strings.HasPrefix(errOut, "NOTICE: not verified:") ||
		!strings.Contains(errOut, "no case verify is recorded in the audit log (the log ends at seq ") {
		t.Fatalf("fresh case: exit %d stderr %q", code, errOut)
	}
	if strings.Contains(out, "NOTICE") || !strings.Contains(out, "hello world") {
		t.Fatalf("notice reached stdout or listing missing: %q", out)
	}
	if code, o, _ := splitRun(t, "case", "verify", "--case", rc.dir); code != 0 {
		t.Fatalf("verify: %d %s", code, o)
	}
	acts := auditActions(t, rc.dir)
	code, out2, errOut := splitRun(t, "records", "list", "--case", rc.dir)
	if code != 0 || !strings.Contains(errOut, "the last case verify in the audit log is seq ") || !strings.Contains(errOut, ", ok); the log ends at seq ") {
		t.Fatalf("after verify: exit %d stderr %q", code, errOut)
	}
	if out2 != out {
		t.Fatalf("stdout changed by the notice:\n%s\nvs\n%s", out2, out)
	}
	if len(acts) == 0 {
		t.Fatal("no audit")
	}
}

func TestRecordsReadsAllPrintTheNotice(t *testing.T) {
	rc := recDataset(t, false)
	for _, args := range recordsReads(rc.dir) {
		code, out, errOut := splitRun(t, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d %s %s", args, code, out, errOut)
		}
		if n := strings.Count(errOut, "NOTICE: not verified:"); n != 1 || strings.Contains(out, "NOTICE") {
			t.Fatalf("%v: stderr %q stdout %q", args, errOut, out)
		}
		_, jout, jerr := splitRun(t, append(args, "--json")...)
		if !strings.Contains(jerr, "NOTICE: not verified:") || strings.Contains(jout, "NOTICE") {
			t.Fatalf("%v --json: stderr %q stdout %q", args, jerr, jout)
		}
	}
}

func TestRecordsNoticeNeverSaysVerified(t *testing.T) {
	rc := recDataset(t, false)
	check := func(label string) {
		for _, args := range recordsReads(rc.dir) {
			code, _, errOut := splitRun(t, args...)
			if code != 0 || !strings.Contains(errOut, "not verified") || strings.Contains(errOut, "verified now") {
				t.Fatalf("%s %v: exit %d stderr %q", label, args, code, errOut)
			}
		}
	}
	check("fresh")
	recordstest.SetRecordSummary(t, rc.dir, 2, "altered afterwards")
	if code, _, _ := splitRun(t, "case", "verify", "--case", rc.dir); code != ExitIntegrity {
		t.Fatalf("verify of a tampered case: exit %d", code)
	}
	check("after FAILED")
	_, _, errOut := splitRun(t, "records", "list", "--case", rc.dir)
	if !strings.Contains(errOut, "FAILED") {
		t.Fatalf("a failed verify.run is not named: %q", errOut)
	}
}

func TestRecordsVerifyFlagVerifiesFirst(t *testing.T) {
	rc := recDataset(t, false)
	for _, args := range recordsReads(rc.dir) {
		before := countAction(auditActions(t, rc.dir), "verify.run")
		code, out, errOut := splitRun(t, append(args, "--verify")...)
		if code != 0 || out == "" || !strings.Contains(errOut, "verified now: ") || strings.Contains(errOut, "not verified") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
		acts := auditActions(t, rc.dir)
		if got := countAction(acts, "verify.run"); got != before+1 {
			t.Fatalf("%v: %d verify.run entries, want %d", args, got, before+1)
		}
		// exact tail: this command's case.open, then the verify.run, and nothing after it (the read appends nothing)
		if n := len(acts); n < 2 || acts[n-2] != "case.open" || acts[n-1] != "verify.run" {
			t.Fatalf("%v: audit tail %v, want [... case.open verify.run]", args, acts[max(0, len(acts)-3):])
		}
	}
}

func TestRecordsVerifyFailurePrintsNoListing(t *testing.T) {
	rc := recDataset(t, false)
	recordstest.SetRecordSummary(t, rc.dir, 2, "altered\x1b[31m afterwards")
	for _, args := range recordsReads(rc.dir) {
		code, out, errOut := splitRun(t, append(args, "--verify")...)
		if code != ExitIntegrity || out != "" || !strings.Contains(errOut, "PROBLEM:") || strings.Contains(errOut, "\x1b") {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
}

// withoutNotVerified drops the not-verified notice lines of a stderr capture, so a test about something else
// still sees only the stderr it is about. Tests of the notice itself read the capture unfiltered.
func withoutNotVerified(s string) string {
	var kept []string
	for _, l := range strings.SplitAfter(s, "\n") {
		if !strings.HasPrefix(l, "NOTICE: not verified:") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "")
}

// The cancel arrives after the case is open, while the verify runs, so the cancel-only branch of
// recordsPreamble is the one exercised (not a failure to open).
func TestRecordsVerifyCancelledExits1(t *testing.T) {
	rc := recDataset(t, false)
	orig := runVerify
	t.Cleanup(func() { runVerify = orig })
	runVerify = func(ctx context.Context, c *evidence.Case, p *evidence.VerifyProgress) (evidence.VerifyReport, error) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		return orig(cctx, c, p)
	}
	code, out, errOut := runSplit(context.Background(), t, Deps{}, "records", "list", "--verify", "--case", rc.dir)
	if code != ExitError || out != "" || !strings.Contains(errOut, "verify cancelled") || strings.Contains(errOut, "integrity") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if countAction(auditActions(t, rc.dir), "verify.run") != 1 {
		t.Fatal("the cancelled verify must still be audited once")
	}
}
