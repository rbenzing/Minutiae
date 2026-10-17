package main

import (
	"strconv"
	"strings"
	"time"
)

// Progress is one parsed "fuzz:" status line of `go test -fuzz`. The recorded shapes
// (testdata/progress) are
//
//	fuzz: elapsed: 3s, gathering baseline coverage: 0/1234 completed
//	fuzz: elapsed: 19s, gathering baseline coverage: 1234/1234 completed, now fuzzing with 1 workers
//	fuzz: elapsed: 21s, execs: 5129 (1332/sec), new interesting: 4 (total: 1238)
//	fuzz: elapsed: 3s, minimizing
//
// Execs is the engine's own count, which does not include minimisation work, so a reported
// Rate of 0 is not evidence that the run is healthy.
type Progress struct {
	Elapsed        time.Duration
	Execs, Rate    uint64
	NewInteresting uint64
	Total          uint64 // the corpus total; for a baseline line the number of entries to gather
	Phase          string // "baseline", "fuzzing", "minimizing"
}

// maxLine is the longest line ParseProgress looks at.
const maxLine = 4096

// ParseProgress parses one status line. Anything that is not exactly one of the recorded
// shapes, including a number above MaxUint64, is (Progress{}, false).
func ParseProgress(line string) (Progress, bool) {
	if len(line) > maxLine {
		return Progress{}, false
	}
	line = strings.TrimSuffix(line, "\r")
	rest, ok := strings.CutPrefix(line, "fuzz: elapsed: ")
	if !ok {
		return Progress{}, false
	}
	el, rest, ok := strings.Cut(rest, ", ")
	if !ok {
		return Progress{}, false
	}
	elapsed, ok := parseElapsed(el)
	if !ok {
		return Progress{}, false
	}
	p := Progress{Elapsed: elapsed}
	switch {
	case rest == "minimizing":
		p.Phase = "minimizing"
		return p, true
	case strings.HasPrefix(rest, "gathering baseline coverage: "):
		return parseBaseline(p, strings.TrimPrefix(rest, "gathering baseline coverage: "))
	case strings.HasPrefix(rest, "execs: "):
		return parseExecs(p, strings.TrimPrefix(rest, "execs: "))
	}
	return Progress{}, false
}

func parseElapsed(s string) (time.Duration, bool) {
	if s == "" || strings.Trim(s, "0123456789hms") != "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d >= 0
}

// parseBaseline parses "<done>/<total> completed" optionally followed by ", now fuzzing with <n> workers".
func parseBaseline(p Progress, s string) (Progress, bool) {
	counts, tail, ok := strings.Cut(s, " completed")
	if !ok {
		return Progress{}, false
	}
	p.Phase = "baseline"
	if tail != "" {
		w, ok := strings.CutPrefix(tail, ", now fuzzing with ")
		if !ok {
			return Progress{}, false
		}
		n, ok := strings.CutSuffix(w, " workers")
		if !ok {
			return Progress{}, false
		}
		if _, ok := parseUint(n); !ok {
			return Progress{}, false
		}
		p.Phase = "fuzzing"
	}
	done, total, ok := strings.Cut(counts, "/")
	if !ok {
		return Progress{}, false
	}
	if _, ok := parseUint(done); !ok {
		return Progress{}, false
	}
	if p.Total, ok = parseUint(total); !ok {
		return Progress{}, false
	}
	return p, true
}

// parseExecs parses "<n> (<r>/sec), new interesting: <k> (total: <t>)".
func parseExecs(p Progress, s string) (Progress, bool) {
	var ok bool
	var num string
	if num, s, ok = strings.Cut(s, " ("); !ok {
		return Progress{}, false
	}
	if p.Execs, ok = parseUint(num); !ok {
		return Progress{}, false
	}
	if num, s, ok = strings.Cut(s, "/sec), new interesting: "); !ok {
		return Progress{}, false
	}
	if p.Rate, ok = parseUint(num); !ok {
		return Progress{}, false
	}
	if num, s, ok = strings.Cut(s, " (total: "); !ok {
		return Progress{}, false
	}
	if p.NewInteresting, ok = parseUint(num); !ok {
		return Progress{}, false
	}
	if num, ok = strings.CutSuffix(s, ")"); !ok {
		return Progress{}, false
	}
	if p.Total, ok = parseUint(num); !ok {
		return Progress{}, false
	}
	p.Phase = "fuzzing"
	return p, true
}

// parseUint accepts decimal digits only (no sign, no space) and fails above MaxUint64.
func parseUint(s string) (uint64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// Outcome is how one target's run ended.
type Outcome string

const (
	OK          Outcome = "ok"
	Failed      Outcome = "failed"
	Stalled     Outcome = "stalled"
	NoProgress  Outcome = "no-progress"
	Timeout     Outcome = "timeout"
	BuildFailed Outcome = "build-failed"
	Skipped     Outcome = "skipped"
)

// Observation is what was seen of one `go test -fuzz` process.
type Observation struct {
	Progress    []Progress
	Exit        int
	TimedOut    bool
	Failing     []string // "Failing input written to ..." paths, in order
	Started     bool     // a "fuzz:" line, "=== RUN", or an "ok"/"PASS" line was seen
	BuildFailed bool     // a "[build failed]" or "[setup failed]" line was seen
}

// AddLine records one output line (without its newline).
func (o *Observation) AddLine(line string) {
	if len(line) > maxLine {
		return
	}
	line = strings.TrimSuffix(line, "\r")
	if p, ok := ParseProgress(line); ok {
		o.Progress = append(o.Progress, p)
		o.Started = true
		return
	}
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "fuzz: "), strings.HasPrefix(line, "=== RUN"),
		line == "PASS", strings.HasPrefix(line, "ok  \t"), strings.HasPrefix(line, "ok\t"):
		o.Started = true
	case strings.HasPrefix(line, "FAIL") && (strings.HasSuffix(line, "[build failed]") || strings.HasSuffix(line, "[setup failed]")):
		o.BuildFailed = true
	}
	if path, ok := strings.CutPrefix(trimmed, "Failing input written to "); ok && path != "" {
		o.Failing = append(o.Failing, path)
	}
}

// Stall thresholds: this many consecutive "fuzzing" lines with an unchanged non-zero count
// over at least this much elapsed time.
const (
	stallLines = 5
	stallSpan  = 12 * time.Second
)

// Classify decides the outcome, in this order: a failing input, a timeout, a build failure,
// a non-zero exit, no execution at all, a stall, otherwise OK. A run in which the engine
// never reported a non-zero execution count is NoProgress, whatever its reported rate.
func Classify(o Observation) Outcome {
	switch {
	case len(o.Failing) > 0:
		return Failed
	case o.TimedOut:
		return Timeout
	case o.BuildFailed:
		return BuildFailed
	case o.Exit != 0:
		return Failed
	case !o.Started || !anyExecs(o.Progress):
		return NoProgress
	case stalled(o.Progress):
		return Stalled
	}
	return OK
}

func anyExecs(ps []Progress) bool {
	for _, p := range ps {
		if p.Execs > 0 {
			return true
		}
	}
	return false
}

// stalled reports a run of stallLines or more consecutive "fuzzing" lines that share one
// non-zero Execs and span at least stallSpan.
func stalled(ps []Progress) bool {
	start := 0
	for i, p := range ps {
		if p.Execs == 0 || p.Phase != "fuzzing" {
			start = i + 1
			continue
		}
		if i > start && ps[start].Execs != p.Execs {
			start = i
		}
		if i-start+1 >= stallLines && p.Elapsed-ps[start].Elapsed >= stallSpan {
			return true
		}
	}
	return false
}
