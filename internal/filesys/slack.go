package filesys

import (
	"fmt"
	"sort"
)

// SlackReporter is implemented by a filesystem that supports the slack export: it occupies exactly
// [0, Info().Size) of its partition and its regular files implement AllocatedRunner. ReportsSlack says whether
// the instance meets that; a wrapper that always has the method but may forward to a filesystem without it says
// so by returning false.
type SlackReporter interface{ ReportsSlack() bool }

// ReportsSlack reports whether fsys (or the filesystem it wraps) is a SlackReporter that says true. A nil or
// typed-nil filesystem reports false.
func ReportsSlack(fsys FileSystem) bool {
	s, ok := As[SlackReporter](fsys)
	return ok && s.ReportsSlack()
}

// ErrNoSlack means the filesystem reader does not report allocation units, so no slack can be derived.
var ErrNoSlack = fmt.Errorf("%w: the filesystem reader does not report allocation units (no slack)", ErrUnsupported)

// SlackOutcome is what TailSlack decided about one file.
type SlackOutcome string

// The outcomes of TailSlack. Only SlackFound carries a run.
const (
	SlackFound         SlackOutcome = "slack"          // the tail of the last allocation unit lies after the last data byte
	SlackExactFit      SlackOutcome = "exact-fit"      // the data ends on a unit boundary
	SlackNoRuns        SlackOutcome = "no-runs"        // size 0, inline content or no data run
	SlackPrefix        SlackOutcome = "prefix"         // the runs cover only a prefix: the last unit is not known
	SlackTrailingHole  SlackOutcome = "trailing-hole"  // the last run is a hole (sparse or uninitialised tail)
	SlackInvalidRuns   SlackOutcome = "invalid-runs"   // the runs break the File.Runs contract
	SlackAllocMismatch SlackOutcome = "alloc-mismatch" // AllocatedRuns is not the unit-rounded data runs
)

// isData reports whether r is a data run (not a hole, not empty).
func isData(r Run) bool { return r.Offset >= 0 && r.Length > 0 }

// roundUp returns n rounded up to a multiple of unit; ok is false for unit <= 0, negative n or overflow.
func roundUp(n, unit int64) (int64, bool) {
	if unit <= 0 || n < 0 {
		return 0, false
	}
	s, ok := AddOK(n, unit-1)
	if !ok {
		return 0, false
	}
	return MulOK(s/unit, unit)
}

// unitRuns is UnitRuns with its failure reported: ok is false for unit <= 0 or an overflow.
func unitRuns(runs []Run, unit int64) (out []Run, ok bool) {
	if unit <= 0 {
		return nil, false
	}
	for _, r := range runs {
		if !isData(r) {
			continue
		}
		l, ok := roundUp(r.Length, unit)
		if !ok {
			return nil, false
		}
		if _, ok := AddOK(r.Offset, l); !ok {
			return nil, false
		}
		out = append(out, Run{Offset: r.Offset, Length: l})
	}
	return out, true
}

// UnitRuns returns one run per data run of runs, in order, with the length rounded up to a multiple of unit
// (the allocation unit, a cluster or a block). Holes and empty runs are dropped. It returns nil for a unit of
// at most 0, an overflow, or when there is no data run. The result is a fresh slice.
func UnitRuns(runs []Run, unit int64) []Run {
	out, _ := unitRuns(runs, unit)
	return out
}

func allocErr(format string, a ...any) error {
	return &CorruptError{Structure: "allocation units", Offset: -1, Reason: fmt.Sprintf(format, a...)}
}

// CheckAllocatedRuns verifies the AllocatedRunner contract: alloc holds exactly one run per data run of runs, in
// the same order, with the same offset and the length rounded up to a multiple of unit; every alloc run lies
// inside [0, fsSize) and no two overlap. A violation is a *CorruptError (Structure "allocation units"). The
// inputs are not modified.
func CheckAllocatedRuns(runs, alloc []Run, unit, fsSize int64) error {
	if unit <= 0 {
		return allocErr("allocation unit %d is not positive", unit)
	}
	if fsSize < 0 {
		return allocErr("negative filesystem size %d", fsSize)
	}
	want, ok := unitRuns(runs, unit)
	if !ok {
		return allocErr("the data runs cannot be rounded to %d-byte units (overflow)", unit)
	}
	if len(alloc) != len(want) {
		return allocErr("%d allocation runs for %d data runs", len(alloc), len(want))
	}
	for i, w := range want {
		if a := alloc[i]; a != w {
			return allocErr("allocation run %d is %d+%d, want %d+%d (the data run rounded to %d-byte units)", i, a.Offset, a.Length, w.Offset, w.Length, unit)
		}
		if end, ok := AddOK(w.Offset, w.Length); !ok || end > fsSize {
			return allocErr("allocation run %d (%d+%d) lies outside the %d-byte filesystem", i, w.Offset, w.Length, fsSize)
		}
	}
	sorted := append([]Run(nil), alloc...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	for i := 1; i < len(sorted); i++ {
		// The ends were checked above, so the sums cannot overflow.
		if prev := sorted[i-1]; prev.Offset+prev.Length > sorted[i].Offset {
			return allocErr("allocation runs %d+%d and %d+%d overlap", prev.Offset, prev.Length, sorted[i].Offset, sorted[i].Length)
		}
	}
	return nil
}

// TailSlack derives the slack of one live regular file: the bytes after its last data byte up to the end of the
// allocation unit that holds it. size, runs (File.Runs) and alloc (AllocatedRuns) come from the reader; unit is
// Info().BlockSize and fsSize Info().Size. The run is filesystem-relative with Length in 1..unit-1 and comes
// only with SlackFound. Every other outcome carries a zero Run; SlackInvalidRuns and SlackAllocMismatch carry
// the reason as the string. The tests run in this order: no runs, runs that break the contract or cover only a
// prefix, no data run (an all-hole file), a trailing hole, the allocation contract, then exact fit or slack.
func TailSlack(size int64, runs, alloc []Run, unit, fsSize int64) (Run, SlackOutcome, string) {
	if size < 0 {
		return Run{}, SlackInvalidRuns, fmt.Sprintf("negative size %d", size)
	}
	if size == 0 || len(runs) == 0 {
		return Run{}, SlackNoRuns, ""
	}
	covered, err := CheckRunsPrefix(runs, size, fsSize)
	if err != nil {
		return Run{}, SlackInvalidRuns, err.Error()
	}
	if covered < size {
		return Run{}, SlackPrefix, ""
	}
	last := -1
	for i, r := range runs {
		if r.Length > 0 {
			last = i
		}
	}
	data := false
	for _, r := range runs {
		if isData(r) {
			data = true
			break
		}
	}
	if !data {
		return Run{}, SlackNoRuns, ""
	}
	if !isData(runs[last]) {
		return Run{}, SlackTrailingHole, ""
	}
	if err := CheckAllocatedRuns(runs, alloc, unit, fsSize); err != nil {
		return Run{}, SlackAllocMismatch, err.Error()
	}
	l, a := runs[last], alloc[len(alloc)-1]
	// Both sums are inside [0, fsSize]: CheckRunsPrefix and CheckAllocatedRuns validated them.
	dataEnd, allocEnd := l.Offset+l.Length, a.Offset+a.Length
	if allocEnd == dataEnd {
		return Run{}, SlackExactFit, ""
	}
	return Run{Offset: dataEnd, Length: allocEnd - dataEnd}, SlackFound, ""
}
