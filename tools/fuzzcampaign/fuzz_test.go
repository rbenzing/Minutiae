package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordedLines returns every line of the recorded logs in testdata/progress.
func recordedLines(tb testing.TB) []string {
	tb.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "progress", "*.txt"))
	if err != nil || len(files) == 0 {
		tb.Fatalf("no recorded logs: %v", err)
	}
	var out []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, strings.Split(string(data), "\n")...)
	}
	return out
}

func FuzzParseProgress(f *testing.F) {
	for _, l := range recordedLines(f) {
		f.Add(l)
	}
	f.Fuzz(func(t *testing.T, line string) {
		p, ok := ParseProgress(line)
		if !ok {
			if p != (Progress{}) {
				t.Fatalf("not a progress line but a non-zero result: %+v", p)
			}
			return
		}
		switch p.Phase {
		case "baseline", "fuzzing", "minimizing":
		default:
			t.Fatalf("Phase %q is none of the three phases", p.Phase)
		}
		if p.Elapsed < 0 {
			t.Fatalf("negative Elapsed %v", p.Elapsed)
		}
	})
}

func FuzzClassify(f *testing.F) {
	f.Add(strings.Join(recordedLines(f), "\n"), 0, uint8(0))
	f.Add("fuzz: elapsed: 3s, execs: 5 (1/sec), new interesting: 0 (total: 1)\nok\tx\t1s\n", 0, uint8(0))
	f.Add("", 1, uint8(3))
	f.Fuzz(func(t *testing.T, text string, exit int, flags uint8) {
		o := Observation{Exit: exit, TimedOut: flags&1 != 0}
		for _, line := range strings.Split(text, "\n") {
			o.AddLine(line)
		}
		if flags&2 != 0 {
			o.BuildFailed = true
		}
		if flags&4 != 0 {
			o.Started = true
		}
		got := Classify(o)
		switch got {
		case OK, Failed, Stalled, NoProgress, Timeout, BuildFailed:
		default:
			t.Fatalf("Classify returned %q", got)
		}
		if got == OK {
			seen := false
			for _, p := range o.Progress {
				if p.Execs > 0 {
					seen = true
				}
			}
			if !seen {
				t.Fatalf("Classify returned ok for a run with no execution: %+v", o)
			}
		}
	})
}
