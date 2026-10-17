package main

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseProgress(t *testing.T) {
	ok := []struct {
		line string
		want Progress
	}{
		// Lines from the Task 1 report or from the scratch-module logs in testdata/progress.
		{
			"fuzz: elapsed: 0s, gathering baseline coverage: 0/1 completed",
			Progress{Elapsed: 0, Total: 1, Phase: "baseline"},
		},
		{
			"fuzz: elapsed: 19s, gathering baseline coverage: 1234/1234 completed, now fuzzing with 1 workers",
			Progress{Elapsed: 19 * time.Second, Total: 1234, Phase: "fuzzing"},
		},
		{
			"fuzz: elapsed: 13s, gathering baseline coverage: 860/860 completed, now fuzzing with 12 workers",
			Progress{Elapsed: 13 * time.Second, Total: 860, Phase: "fuzzing"},
		},
		{
			"fuzz: elapsed: 36s, execs: 56721 (3288/sec), new interesting: 32 (total: 1266)",
			Progress{Elapsed: 36 * time.Second, Execs: 56721, Rate: 3288, NewInteresting: 32, Total: 1266, Phase: "fuzzing"},
		},
		{
			"fuzz: elapsed: 34s, execs: 931 (0/sec), new interesting: 0 (total: 860)",
			Progress{Elapsed: 34 * time.Second, Execs: 931, Rate: 0, NewInteresting: 0, Total: 860, Phase: "fuzzing"},
		},
		{
			"fuzz: elapsed: 3s, execs: 75148 (25044/sec), new interesting: 7 (total: 8)",
			Progress{Elapsed: 3 * time.Second, Execs: 75148, Rate: 25044, NewInteresting: 7, Total: 8, Phase: "fuzzing"},
		},
		{
			"fuzz: elapsed: 1m16s, execs: 4053 (0/sec), new interesting: 0 (total: 860)",
			Progress{Elapsed: 76 * time.Second, Execs: 4053, Total: 860, Phase: "fuzzing"},
		},
		// Constructed edge cases (not log lines): a long duration, a maximal count, a CR ending.
		{
			"fuzz: elapsed: 1h2m3s, execs: 1 (1/sec), new interesting: 0 (total: 1)",
			Progress{Elapsed: time.Hour + 2*time.Minute + 3*time.Second, Execs: 1, Rate: 1, Total: 1, Phase: "fuzzing"},
		},
		{"fuzz: elapsed: 3s, minimizing", Progress{Elapsed: 3 * time.Second, Phase: "minimizing"}},
		{
			"fuzz: elapsed: 18s, execs: 18446744073709551615 (0/sec), new interesting: 0 (total: 1)\r",
			Progress{Elapsed: 18 * time.Second, Execs: math.MaxUint64, Total: 1, Phase: "fuzzing"},
		},
	}
	for _, c := range ok {
		got, isProgress := ParseProgress(c.line)
		if !isProgress || got != c.want {
			t.Errorf("ParseProgress(%q) = %+v, %v; want %+v, true", c.line, got, isProgress, c.want)
		}
	}
	notProgress := []string{
		"",
		"fuzz: minimizing 1535-byte failing input file",
		"--- FAIL: FuzzBoom (3.84s)",
		"PASS",
		"ok  \tgithub.com/rbenzing/minutiae/internal/decode/sqlitedb\t49.834s",
		"FAIL\tscratch/build [build failed]",
		"fuzz: elapsed: 3s, minimizing now",
		"fuzz: elapsed: 3s, execs: 5 (1/sec), new interesting: 0 (total: 1) extra",
		"fuzz: elapsed: 3s, execs: 5 (1/sec), new interesting: 0 (total: 1",
		"fuzz: elapsed: 3s, execs: 5 (1/sec) new interesting: 0 (total: 1)",
		"fuzz: elapsed: -3s, execs: 5 (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: +3s, execs: 5 (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3, execs: 5 (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: , execs: 5 (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3s, execs: x (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3s, execs: -5 (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3s, execs: 5 (1.5/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3s, gathering baseline coverage: 5 completed",
		"fuzz: elapsed: 3s, gathering baseline coverage: /9 completed",
		"fuzz: elapsed: 3s, gathering baseline coverage: 1/9 completed, now fuzzing",
		" fuzz: elapsed: 3s, minimizing",
	}
	for _, line := range notProgress {
		if got, isProgress := ParseProgress(line); isProgress {
			t.Errorf("ParseProgress(%q) = %+v, true; want false", line, got)
		}
	}
	long := "fuzz: elapsed: 3s, minimizing" + strings.Repeat(" ", 1<<20)
	if _, isProgress := ParseProgress(long); isProgress {
		t.Errorf("a 1 MiB line was accepted")
	}
	if n := testing.AllocsPerRun(10, func() { ParseProgress(long) }); n > 2 {
		t.Errorf("ParseProgress of a 1 MiB line allocates %v times, want at most 2", n)
	}
	// The limit is 4096 bytes. Leading zeros keep the line valid at any length, so only the
	// length decides: 4096 bytes parse, 4097 do not.
	const head, tail = "fuzz: elapsed: ", "3s, minimizing"
	at := head + strings.Repeat("0", 4096-len(head)-len(tail)) + tail
	if _, isProgress := ParseProgress(at); !isProgress || len(at) != 4096 {
		t.Errorf("a valid line of %d bytes was rejected", len(at))
	}
	if _, isProgress := ParseProgress(at[:len(head)] + "0" + at[len(head):]); isProgress {
		t.Errorf("a valid line of 4097 bytes was accepted")
	}
}

func TestParseProgressSaturatesHugeNumbers(t *testing.T) {
	over := strconv.FormatUint(math.MaxUint64, 10) + "0"
	for _, line := range []string{
		"fuzz: elapsed: 3s, execs: " + over + " (1/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3s, execs: 1 (" + over + "/sec), new interesting: 0 (total: 1)",
		"fuzz: elapsed: 3s, execs: 1 (1/sec), new interesting: " + over + " (total: 1)",
		"fuzz: elapsed: 3s, execs: 1 (1/sec), new interesting: 0 (total: " + over + ")",
		"fuzz: elapsed: 3s, gathering baseline coverage: 0/" + over + " completed",
		"fuzz: elapsed: 9999999999999999999999h, minimizing",
	} {
		if got, isProgress := ParseProgress(line); isProgress {
			t.Errorf("ParseProgress(%q) = %+v, true; want false (never wrapped)", line, got)
		}
	}
}

// series builds consecutive "fuzzing" progress lines three seconds apart, one per execs value.
func series(execs ...uint64) []Progress {
	var out []Progress
	for i, e := range execs {
		out = append(out, Progress{Elapsed: time.Duration(3*(i+1)) * time.Second, Execs: e, Phase: "fuzzing"})
	}
	return out
}

func TestClassify(t *testing.T) {
	stalledFive := series(10, 10, 10, 10, 10)
	cases := []struct {
		name string
		o    Observation
		want Outcome
	}{
		{"failing input beats timeout", Observation{Failing: []string{"x"}, TimedOut: true, Exit: 1, Started: true, Progress: series(5)}, Failed},
		{"failing input beats a build failure", Observation{Failing: []string{"x"}, BuildFailed: true, Started: true}, Failed},
		{"failing input beats a clean exit", Observation{Failing: []string{"x"}, Started: true, Progress: series(5)}, Failed},
		{"timeout beats build failure", Observation{TimedOut: true, BuildFailed: true, Exit: 1}, Timeout},
		{"timeout beats a non-zero exit", Observation{TimedOut: true, Exit: 1, Started: true, Progress: series(5)}, Timeout},
		{"build failure beats a non-zero exit", Observation{BuildFailed: true, Exit: 1}, BuildFailed},
		{"a non-zero exit is failed", Observation{Exit: 2, Started: true, Progress: series(5, 50)}, Failed},
		{"a non-zero exit with no progress is failed", Observation{Exit: 1}, Failed},
		{"exit 0 with nothing started is no-progress", Observation{}, NoProgress},
		{"exit 0 and nothing started, even with progress recorded, is no-progress", Observation{Progress: series(5, 50)}, NoProgress},
		{"only a zero execs line is no-progress", Observation{Started: true, Progress: []Progress{pl(3, 0, "fuzzing")}}, NoProgress},
		{"only baseline lines are no-progress", Observation{Started: true, Progress: []Progress{{Elapsed: 3 * time.Second, Total: 9, Phase: "baseline"}}}, NoProgress},
		{"no lines at all but started is no-progress", Observation{Started: true}, NoProgress},
		{"five unchanged non-zero lines are stalled", Observation{Started: true, Progress: stalledFive}, Stalled},
		{"four unchanged non-zero lines are ok", Observation{Started: true, Progress: series(10, 10, 10, 10)}, OK},
		{"four unchanged lines over 12 s are ok", Observation{Started: true, Progress: []Progress{
			pl(1, 7, "fuzzing"),
			pl(5, 7, "fuzzing"),
			pl(9, 7, "fuzzing"),
			pl(13, 7, "fuzzing"),
		}}, OK},
		{"five unchanged lines in under 12 s are ok", Observation{Started: true, Progress: []Progress{
			pl(1, 7, "fuzzing"),
			pl(2, 7, "fuzzing"),
			pl(3, 7, "fuzzing"),
			pl(4, 7, "fuzzing"),
			pl(5, 7, "fuzzing"),
		}}, OK},
		{"five unchanged lines over exactly 12 s are stalled", Observation{Started: true, Progress: []Progress{
			pl(1, 7, "fuzzing"),
			pl(5, 7, "fuzzing"),
			pl(8, 7, "fuzzing"),
			pl(10, 7, "fuzzing"),
			pl(13, 7, "fuzzing"),
		}}, Stalled},
		{"five unchanged lines over 11 s are ok", Observation{Started: true, Progress: []Progress{
			pl(1, 7, "fuzzing"),
			pl(5, 7, "fuzzing"),
			pl(8, 7, "fuzzing"),
			pl(10, 7, "fuzzing"),
			pl(12, 7, "fuzzing"),
		}}, OK},
		{"unchanged runs that are interrupted by progress are ok", Observation{Started: true, Progress: series(10, 10, 10, 11, 11, 11, 12, 12, 12)}, OK},
		{"a stall that ends in progress is still a stall in the middle", Observation{Started: true, Progress: series(10, 20, 20, 20, 20, 20, 30)}, Stalled},
		{"a stall while minimizing is not a stall", Observation{Started: true, Progress: []Progress{
			pl(3, 10, "fuzzing"),
			pl(6, 10, "minimizing"),
			pl(9, 10, "minimizing"),
			pl(12, 10, "minimizing"),
			pl(15, 10, "minimizing"),
			pl(18, 10, "minimizing"),
		}}, OK},
		{"a stall before the first non-zero line does not count", Observation{Started: true, Progress: []Progress{
			pl(3, 0, "fuzzing"),
			pl(6, 0, "fuzzing"),
			pl(9, 0, "fuzzing"),
			pl(12, 0, "fuzzing"),
			pl(15, 0, "fuzzing"),
			pl(18, 4, "fuzzing"),
		}}, OK},
		{"steady progress is ok", Observation{Started: true, Progress: series(10, 20, 30, 40, 50, 60)}, OK},
		{"a reported rate of 0 with growing execs is ok", Observation{Started: true, Progress: []Progress{
			pl(3, 10, "fuzzing"),
			pl(6, 20, "fuzzing"),
		}}, OK},
		{"a reported rate of 0 with a frozen count is never ok", Observation{Started: true, Progress: []Progress{
			pl(3, 10, "fuzzing"),
			pl(6, 10, "fuzzing"),
			pl(9, 10, "fuzzing"),
			pl(12, 10, "fuzzing"),
			pl(15, 10, "fuzzing"),
		}}, Stalled},
	}
	for _, c := range cases {
		if got := Classify(c.o); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// observe turns a recorded go test log into an Observation the way the runner will.
func observe(t *testing.T, name string, exit int) Observation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "progress", name))
	if err != nil {
		t.Fatal(err)
	}
	o := Observation{Exit: exit}
	for _, line := range strings.Split(string(data), "\n") {
		o.AddLine(line)
	}
	return o
}

func TestClassifyReadsRecordedLogs(t *testing.T) {
	cases := []struct {
		file string
		exit int
		want Outcome
	}{
		{"fuzzing.txt", 0, OK},
		// The retained excerpt of a frozen counter is only four lines over 8 s: too short to call
		// a stall, so it is OK. The Stalled verdict rests on the synthetic stall.txt alone.
		{"frozen-excerpt.txt", 0, OK},
		{"stall.txt", 0, Stalled},
		{"baseline.txt", 0, NoProgress},
		{"minimizing.txt", 1, Failed},
		{"failed.txt", 1, Failed},
		{"build-failed.txt", 1, BuildFailed},
		{"no-fuzz-tests.txt", 0, NoProgress},
	}
	for _, c := range cases {
		o := observe(t, c.file, c.exit)
		if got := Classify(o); got != c.want {
			t.Errorf("%s: Classify = %q, want %q (observation %+v)", c.file, got, c.want, o)
		}
		if _, err := os.Stat(filepath.Join("testdata", "progress", strings.TrimSuffix(c.file, ".txt")+".note")); err != nil {
			t.Errorf("%s has no companion .note: %v", c.file, err)
		}
	}
}

func TestObservationAddLine(t *testing.T) {
	failed := observe(t, "failed.txt", 1)
	if want := []string{`testdata\fuzz\FuzzBoom\5780fe0d64cfecb4`}; !reflect.DeepEqual(failed.Failing, want) {
		t.Errorf("failed.txt Failing = %q, want %q", failed.Failing, want)
	}
	if !failed.Started || failed.BuildFailed {
		t.Errorf("failed.txt Started=%v BuildFailed=%v", failed.Started, failed.BuildFailed)
	}
	mn := observe(t, "minimizing.txt", 1)
	var minimizing int
	for _, p := range mn.Progress {
		if p.Phase == "minimizing" {
			minimizing++
		}
	}
	if minimizing != 6 {
		t.Errorf("minimizing.txt has %d minimizing lines, want 6", minimizing)
	}
	if want := []string{`testdata\fuzz\FuzzSlowMin\fe410d8030d37f95`}; !reflect.DeepEqual(mn.Failing, want) {
		t.Errorf("minimizing.txt Failing = %q, want %q", mn.Failing, want)
	}
	build := observe(t, "build-failed.txt", 1)
	if !build.BuildFailed || build.Started || len(build.Progress) != 0 {
		t.Errorf("build-failed.txt: %+v", build)
	}
	none := observe(t, "no-fuzz-tests.txt", 0)
	if !none.Started || none.BuildFailed || len(none.Progress) != 0 {
		t.Errorf("no-fuzz-tests.txt: %+v", none)
	}
	var o Observation
	o.AddLine("FAIL\texample.com/x [setup failed]")
	if !o.BuildFailed {
		t.Errorf("a [setup failed] line did not set BuildFailed")
	}
	o = Observation{}
	o.AddLine("=== RUN   TestX")
	if !o.Started {
		t.Errorf("a === RUN line did not set Started")
	}
	o = Observation{}
	o.AddLine("some unrelated output Failing input written to")
	o.AddLine("--- FAIL: FuzzX (0.00s)")
	if len(o.Failing) != 0 || o.Started {
		t.Errorf("unrelated lines changed the observation: %+v", o)
	}
}

// pl is a progress line at the given second.
func pl(sec int, execs uint64, phase string) Progress {
	return Progress{Elapsed: time.Duration(sec) * time.Second, Execs: execs, Phase: phase}
}

func TestObservationKeepsBoundedState(t *testing.T) {
	const lines = 100000
	var o Observation
	// A long run: 20 growing counts, then a frozen count over 15 s, then more growth.
	add := func(sec int, execs uint64) {
		o.AddLine("fuzz: elapsed: " + strconv.Itoa(sec) + "s, execs: " + strconv.FormatUint(execs, 10) +
			" (1/sec), new interesting: 0 (total: 1)")
	}
	n := 0
	for ; n < 20; n++ {
		add(n, uint64(n+1))
	}
	for i := 0; i < 6; i++ { // six frozen lines, 3 s apart: 15 s
		add(20+3*i, 777)
		n++
	}
	for ; n < lines; n++ {
		add(n+100, uint64(n)+1000)
	}
	if len(o.Progress) != maxProgress {
		t.Errorf("retained %d progress lines, want exactly %d", len(o.Progress), maxProgress)
	}
	if o.ProgressLines != lines {
		t.Errorf("ProgressLines = %d, want %d", o.ProgressLines, lines)
	}
	if !o.SawExecs || !o.SawStall {
		t.Errorf("SawExecs=%v SawStall=%v; the verdict must survive the retention cap", o.SawExecs, o.SawStall)
	}
	// The frozen run is inside the retained prefix here, so rebuild a run whose stall is
	// beyond it: only the flags can know.
	var late Observation
	for i := range maxProgress + 10 {
		late.AddLine("fuzz: elapsed: " + strconv.Itoa(i) + "s, execs: " + strconv.Itoa(i+1) + " (1/sec), new interesting: 0 (total: 1)")
	}
	for i := range 6 {
		late.AddLine("fuzz: elapsed: " + strconv.Itoa(1000+3*i) + "s, execs: 999999 (0/sec), new interesting: 0 (total: 1)")
	}
	late.Started = true
	if stalled(late.Progress) {
		t.Fatalf("the stall is inside the retained lines; the test would prove nothing")
	}
	if got := Classify(late); got != Stalled {
		t.Errorf("Classify of a stall beyond the retained lines = %q, want %q", got, Stalled)
	}
	if late.ProgressLines-len(late.Progress) != 10+6 {
		t.Errorf("dropped-line count wrong: %d lines, %d kept", late.ProgressLines, len(late.Progress))
	}

	var f Observation
	for i := range 3 * maxFailing {
		f.AddLine("    Failing input written to testdata/fuzz/FuzzX/" + strconv.Itoa(i))
	}
	if len(f.Failing) != maxFailing || f.FailingTotal != 3*maxFailing {
		t.Errorf("Failing kept %d of %d, want %d of %d", len(f.Failing), f.FailingTotal, maxFailing, 3*maxFailing)
	}
	if f.Failing[0] != "testdata/fuzz/FuzzX/0" || f.Failing[maxFailing-1] != "testdata/fuzz/FuzzX/"+strconv.Itoa(maxFailing-1) {
		t.Errorf("Failing is not the first %d paths: %q", maxFailing, f.Failing)
	}
	if got := Classify(f); got != Failed {
		t.Errorf("Classify = %q, want failed", got)
	}
	// A count alone (nothing retained) still fails the run.
	if got := Classify(Observation{FailingTotal: 1, Started: true}); got != Failed {
		t.Errorf("FailingTotal alone: Classify = %q, want failed", got)
	}
}

func TestObservationCountsOverlongLines(t *testing.T) {
	var o Observation
	o.AddLine(strings.Repeat("x", maxLine+1))
	o.AddLine("fuzz: elapsed: " + strings.Repeat("0", maxLine) + "3s, minimizing")
	o.AddLine(strings.Repeat("y", maxLine)) // exactly the limit: looked at, not counted
	if o.Overlong != 2 {
		t.Errorf("Overlong = %d, want 2", o.Overlong)
	}
	if o.ProgressLines != 0 || o.Started {
		t.Errorf("an overlong line was parsed: %+v", o)
	}
}
