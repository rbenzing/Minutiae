package main

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runMain(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = realMain(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageErrorsExit2(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	for name, args := range map[string][]string{
		"no subcommand":           {},
		"unknown subcommand":      {"frobnicate"},
		"unknown flag":            {"plan", "-nope"},
		"missing -out":            {"run", "-mode", "host"},
		"unknown profile":         {"plan", "-profile", "nope"},
		"unknown mode":            {"run", "-out", out, "-mode", "vm"},
		"missing -linux-binary":   {"run", "-out", out, "-mode", "docker", "-image", "x"},
		"missing -image (docker)": {"run", "-out", out, "-mode", "docker", "-linux-binary", "x"},
		"bad -pkg name":           {"plan", "-pkg", "internal/a b"},
		"bad -pkg shell":          {"plan", "-pkg", "x;rm"},
		"per-target out of range": {"plan", "-fuzztime", "1s"},
		"prepare without -image":  {"prepare"},
	} {
		if code, _, stderr := runMain(t, args...); code != 2 || stderr == "" {
			t.Errorf("%s: exit %d, stderr %q; want 2 with a message", name, code, stderr)
		}
	}
}

func TestPlanSubcommandPrintsTheEstimate(t *testing.T) {
	code, out, stderr := runMain(t, "plan", "-src", "../..", "-profile", "decoders")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"FuzzPlist", "FuzzSQLiteDB", "FuzzTS", "estimate"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan lacks %q:\n%s", want, out)
		}
	}
}

func TestPlanListPrintsTheSelectionWithoutTheCapCheck(t *testing.T) {
	// 15 minutes per target over the whole repository cannot fit any cap, but -list still prints.
	code, out, stderr := runMain(t, "plan", "-list", "-src", "../..", "-profile", "all", "-fuzztime", "15m", "-total", "10m")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, "internal/decode/plist.FuzzPlist") {
		t.Errorf("the listing lacks FuzzPlist:\n%s", out)
	}
	if code, _, _ := runMain(t, "plan", "-src", "../..", "-profile", "all", "-fuzztime", "15m", "-total", "10m"); code == 0 {
		t.Errorf("the same plan without -list passed the cap check")
	}
}

func TestRunRefusesAnOutDirInsideTheSource(t *testing.T) {
	src := t.TempDir()
	code, _, stderr := runMain(t, "run", "-mode", "host", "-src", src, "-out", filepath.Join(src, "out"))
	if code != 2 || !strings.Contains(stderr, "-out") {
		t.Errorf("exit %d, stderr %q; want 2 naming -out", code, stderr)
	}
}

// On Windows a path such as /c/work/out (the Git Bash form) is rooted without a drive, and
// filepath.Abs would put it on the current drive as C:\c\work\out: a directory created
// somewhere the user never named. The tool refuses it.
func TestRootedPathWithoutADriveIsRefusedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Git Bash path form only misleads filepath.Abs on Windows")
	}
	code, _, stderr := runMain(t, "run", "-mode", "docker", "-image", "x", "-linux-binary", "y", "-out", "/c/work/out", "-src", ".")
	if code != 2 || !strings.Contains(stderr, "drive") {
		t.Errorf("exit %d, stderr %q; want 2 naming the missing drive", code, stderr)
	}
}
