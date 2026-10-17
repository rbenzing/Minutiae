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

// Bounds on what an Observation retains, whatever the volume of output.
const (
	maxProgress = 256 // progress lines kept, the first ones; the rest are counted
	maxFailing  = 16  // failing-input paths kept; the rest are counted
)

// Observation is what was seen of one `go test -fuzz` process. It holds a bounded amount of
// state: the verdict flags are updated for every line, so they stay right after the retained
// lists are full, and everything that was not retained is counted, never dropped silently.
type Observation struct {
	Progress    []Progress // the first maxProgress progress lines
	Exit        int
	TimedOut    bool
	Failing     []string // the first maxFailing "Failing input written to ..." paths, in order
	Started     bool     // a "fuzz:" line, "=== RUN", or an "ok"/"PASS" line was seen
	BuildFailed bool     // a "[build failed]" or "[setup failed]" line was seen

	ProgressLines int  // every progress line seen, kept or not
	FailingTotal  int  // every failing-input path seen, kept or not
	Overlong      int  // lines over maxLine, which are not parsed at all
	SawExecs      bool // some progress line had a non-zero execution count
	SawStall      bool // a stall (see stallTracker) occurred at any point of the run

	tracker stallTracker
}

// AddLine records one output line (without its newline).
func (o *Observation) AddLine(line string) {
	if len(line) > maxLine {
		o.Overlong++
		return
	}
	line = strings.TrimSuffix(line, "\r")
	if p, ok := ParseProgress(line); ok {
		o.ProgressLines++
		if len(o.Progress) < maxProgress {
			o.Progress = append(o.Progress, p)
		}
		o.Started = true
		o.SawExecs = o.SawExecs || p.Execs > 0
		o.SawStall = o.tracker.add(p) || o.SawStall
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
		o.FailingTotal++
		if len(o.Failing) < maxFailing {
			o.Failing = append(o.Failing, path)
		}
	}
}

// Stall thresholds. The engine prints a status line about every 3 s, so a stall needs
// stallLines consecutive "fuzzing" lines with one unchanged non-zero count spanning at least
// stallSpan. That cannot happen in under about 13 s of fuzzing after the first non-zero
// count, so a target given a PerTarget below about 16 s can freeze unseen (a longer run is
// needed to see it): the plan accepts such values, it just cannot classify a stall in them.
const (
	stallLines = 5
	stallSpan  = 12 * time.Second
)

// stallTracker finds the stall incrementally, in constant space.
type stallTracker struct {
	start Progress // the first line of the current run of unchanged counts
	n     int      // lines in that run, 0 when there is none
}

// add feeds the next progress line and reports whether it completes a stall.
func (s *stallTracker) add(p Progress) bool {
	if p.Execs == 0 || p.Phase != "fuzzing" {
		s.n = 0
		return false
	}
	if s.n > 0 && s.start.Execs == p.Execs {
		s.n++
	} else {
		s.start, s.n = p, 1
	}
	return s.n >= stallLines && p.Elapsed-s.start.Elapsed >= stallSpan
}

// Classify decides the outcome, in this order: a failing input, a timeout, a build failure,
// a non-zero exit, no execution at all, a stall, otherwise OK. A run in which the engine
// never reported a non-zero execution count is NoProgress, whatever its reported rate. That
// includes a run whose output has no coverage status lines at all (the "fuzz: elapsed"
// shapes are the only ones parsed): it fails closed as NoProgress, never as OK.
func Classify(o Observation) Outcome {
	switch {
	case o.FailingTotal > 0 || len(o.Failing) > 0:
		return Failed
	case o.TimedOut:
		return Timeout
	case o.BuildFailed:
		return BuildFailed
	case o.Exit != 0:
		return Failed
	case !o.Started || (!o.SawExecs && !anyExecs(o.Progress)):
		return NoProgress
	case o.SawStall || stalled(o.Progress):
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

// stalled reports whether the lines contain a stall (see stallTracker).
func stalled(ps []Progress) bool {
	var t stallTracker
	for _, p := range ps {
		if t.add(p) {
			return true
		}
	}
	return false
}
