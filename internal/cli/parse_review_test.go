package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// bidi and c1 are a bidi override and a C1 control, built from code points so the source stays ASCII.
var (
	bidi = string(rune(0x202e))
	c1   = string(rune(0x85))
	// evilText holds an escape sequence, a bidi override and a C1 control.
	evilText = "x\x1b[31m" + bidi + c1
	// pFile is a manifest path with a bidi override and a C1 control in it.
	pFile = "artifacts/D1/A1/files/gone" + bidi + c1 + ".dat"
)

func noRaw(t *testing.T, what, s string) {
	t.Helper()
	if r, bad := rawControl(s); bad {
		t.Errorf("%s: raw %U reached the terminal: %q", what, r, s)
	}
}

// TestParsePlanExitsFourOnIntegrity: a plan that meets an artifact whose bytes differ from the manifest
// exits 4 and names it; it never exits 0.
func TestParsePlanExitsFourOnIntegrity(t *testing.T) {
	p := newPfx(t, "a", "b")
	p.flip("a")
	code, _, errOut := runP(t, registry(t, "", good("a"), good("b")), "parse", "plan", "--case", p.dir)
	if code != ExitIntegrity {
		t.Fatalf("exit %d, want %d: %s", code, ExitIntegrity, errOut)
	}
	if !strings.Contains(errOut, "1 integrity failure(s): "+p.recs["a"].ID) {
		t.Errorf("stderr lacks the integrity line naming the artifact:\n%s", errOut)
	}
}

// TestParsePlanOnCancelledContextExits1: a cancelled plan is exit 1, not 0.
func TestParsePlanOnCancelledContextExits1(t *testing.T) {
	p := newPfx(t, "a")
	root := newRootCmd(Deps{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, Registry: nil, ParserRegistry: registry(t, "", good("a"))})
	root.SetArgs([]string{"parse", "plan", "--case", p.dir})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := root.ExecuteContext(ctx); ExitCode(err) == 0 {
		t.Errorf("a plan on a cancelled context exited 0 (%v)", err)
	}
}

// TestParseEscapesManifestTextInPlanAndRun: case-supplied text that is not the logical path (an artifact id,
// a manifest path in an integrity reason, a device id in the reason an artifact has no logical path) is
// escaped in the plan rows, the unnamed list and the run status line.
func TestParseEscapesManifestTextInPlanAndRun(t *testing.T) {
	reg := registry(t, "", good("b"))

	t.Run("artifact id", func(t *testing.T) {
		p := newPfx(t, "b")
		p.edit(func(c *evidence.Case) {
			pcapture(t, c, "files/copy.dat", evidence.Source{Kind: "file", RemotePath: ppath("b")}, pData)
		})
		// A forged record naming the copy under an id holding control text.
		recordstest.AppendManifestLine(t, p.dir, forged(p, "files/copy.dat", "id"+evilText))
		for _, args := range [][]string{{"parse", "plan"}, {"parse", "run"}} {
			code, out, errOut := runP(t, reg, append(args, "--case", p.dir)...)
			noRaw(t, strings.Join(args, " ")+" stdout", out)
			noRaw(t, strings.Join(args, " ")+" stderr", errOut)
			if code == 2 {
				t.Fatalf("%v: usage error %s", args, errOut)
			}
		}
	})

	t.Run("integrity reason", func(t *testing.T) {
		p := newPfx(t, "b")
		recordstest.AppendManifestLine(t, p.dir, forged(p, pFile, "gone"))
		code, out, errOut := runP(t, reg, "parse", "plan", "--case", p.dir)
		if code != ExitIntegrity {
			t.Fatalf("exit %d: %s %s", code, out, errOut)
		}
		noRaw(t, "plan stdout", out)
		noRaw(t, "plan stderr", errOut)
	})

	t.Run("unnamed reason", func(t *testing.T) {
		p := newPfx(t, "b")
		p.edit(func(c *evidence.Case) {
			src := evidence.Source{Kind: "file", RemotePath: ppath("u"), DeviceID: "D" + evilText}
			if _, err := c.Capture("D1", "A1", "files/u.dat", src, func(w io.Writer) error {
				_, err := io.WriteString(w, pData)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
		code, out, errOut := runP(t, reg, "parse", "plan", "--case", p.dir)
		if !strings.Contains(out, "unnamed ") {
			t.Fatalf("exit %d: no unnamed line:\n%s%s", code, out, errOut)
		}
		noRaw(t, "plan stdout", out)
		if !strings.Contains(out, `\x1b[31m`) {
			t.Errorf("the device id is not shown in escaped form:\n%s", out)
		}
	})
}

func forged(p *pfx, path, id string) evidence.ManifestRecord {
	rec := p.recs["b"]
	rec.ID = id
	rec.Path = path
	rec.Source.RemotePath = ppath("b")
	return rec
}

func TestThousands(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 48210: "48,210", 100000: "100,000", 1234567: "1,234,567", -1234: "-1,234", -999: "-999",
	} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"1KiB": 1 << 10, "1 KiB": 1 << 10, "16MiB": 16 << 20, "1GiB": 1 << 30, "64GiB": 64 << 30, "123": 123, "5B": 5,
	} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "1.5GiB", "-1", "1TiB", "lots", "17179869185GiB", "17592186044417MiB", "9223372036854775808"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) accepted", in)
		}
	}
}

// TestParseLimitFlagBoundaries pins the inclusive ends of --mem-budget and --max-records.
func TestParseLimitFlagBoundaries(t *testing.T) {
	p := newPfx(t, "a")
	reg := registry(t, "", good("a"))
	for _, tt := range []struct {
		flag, val string
		code      int
	}{
		{"--mem-budget", "15MiB", 2},
		{"--mem-budget", "16MiB", 0},
		{"--mem-budget", "64GiB", 0},
		{"--mem-budget", "65GiB", 2},
		{"--mem-budget", "17179869185GiB", 2}, // 2^34+1 GiB wraps to 1 GiB in a 64-bit shift
		{"--max-records", "1", 1},
		{"--max-records", "1000000000", 0},
		{"--max-records", "1000000001", 2},
		{"--max-records", "0", 2},
	} {
		t.Run(tt.flag+"="+tt.val, func(t *testing.T) {
			code, out, errOut := runP(t, reg, "parse", "plan", "--case", p.dir)
			if code != 0 {
				t.Fatalf("baseline plan exit %d: %s %s", code, out, errOut)
			}
			code, _, errOut = runP(t, reg, "parse", "run", "--case", p.dir, "--reparse", tt.flag, tt.val)
			if code != tt.code {
				t.Errorf("exit %d, want %d: %s", code, tt.code, errOut)
			}
		})
	}
}

// TestParseRunPrintsDiscoveryNote: run, like plan, says on stderr that discovery trusts the manifest.
func TestParseRunPrintsDiscoveryNote(t *testing.T) {
	p := newPfx(t, "a")
	_, out, errOut := runP(t, registry(t, "", good("a")), "parse", "run", "--case", p.dir)
	if !strings.Contains(errOut, discoveryNote) || strings.Contains(out, discoveryNote) {
		t.Errorf("the discovery note must be on stderr only:\nstdout %s\nstderr %s", out, errOut)
	}
}

func TestPartialRunErrorKeepsTheStopReason(t *testing.T) {
	if got := (partialRunError{n: 2, stopped: "cancelled"}).Error(); !strings.Contains(got, "2 job(s) incomplete") || !strings.Contains(got, "cancelled") {
		t.Errorf("%q drops the stop reason or the count", got)
	}
	if got := (partialRunError{n: 3}).Error(); strings.Contains(got, "stopped early") {
		t.Errorf("%q names a stop that did not happen", got)
	}
	if got := (partialRunError{stopped: "cancelled"}).Error(); !strings.Contains(got, "the run stopped early: cancelled") {
		t.Errorf("%q", got)
	}
}

// TestPlanTextEscapesStatusAndReason: the status and reason of a plan row are printed escaped even when
// a lower layer let a raw control through.
func TestPlanTextEscapesStatusAndReason(t *testing.T) {
	var b strings.Builder
	err := writePlanText(&b, artparse.Plan{
		Rows:     []artparse.PlanRow{{Status: "refused", Reason: "path " + evilText}},
		ByStatus: map[string]int{"refused" + evilText: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	noRaw(t, "plan text", b.String())
	if !strings.Contains(b.String(), `refused:path x\x1b[31m`) {
		t.Errorf("the reason is not shown escaped:\n%s", b.String())
	}
}

// TestPlanTextMarksIncompleteArtifacts: a row whose artifact is incomplete says so.
func TestPlanTextMarksIncompleteArtifacts(t *testing.T) {
	var j artparse.Job
	j.Primary.Artifact.Incomplete = true
	var b strings.Builder
	if err := writePlanText(&b, artparse.Plan{Rows: []artparse.PlanRow{{Job: j, Status: "new", Incomplete: true}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "[incomplete]") {
		t.Errorf("no [incomplete] marker:\n%s", b.String())
	}
}

// TestRunSummaryTextSaysTheRunStoppedEarly: a stop reason is part of the summary, escaped.
func TestRunSummaryTextSaysTheRunStoppedEarly(t *testing.T) {
	var b strings.Builder
	writeRunSummaryText(&b, artparse.RunSummary{ParseID: "p1", Stopped: "cancelled"}, false)
	if !strings.Contains(b.String(), "the run stopped early: cancelled") {
		t.Errorf("summary lacks the stop:\n%s", b.String())
	}
}
