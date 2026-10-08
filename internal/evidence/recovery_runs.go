package evidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// MaxRecoveredRuns is the most runs one recovered artifact may record.
const MaxRecoveredRuns = 1 << 20

// maxRunsLine is the longest line (without its newline) of a runs sidecar; a
// run is two integers, so a longer line is not a run.
const maxRunsLine = 256

// DerivedRuns returns the image runs of a derived artifact: the inline
// Derived.Runs, else the lines of its runs sidecar, which must be a regular file of kind runs
// derived from the same parent. byID is the manifest indexed by artifact id (a caller that
// checks many artifacts builds it once); nil makes DerivedRuns read the manifest itself. The sidecar (strict JSON, one Run per
// line, no unknown fields, at most MaxRecoveredRuns lines of at most 256
// bytes). An artifact with neither has no runs and no error.
func (c *Case) DerivedRuns(rec ManifestRecord, byID map[string]ManifestRecord) ([]Run, error) {
	d := rec.Source.Derived
	switch {
	case d == nil:
		return nil, nil
	case len(d.Runs) > 0:
		return append([]Run(nil), d.Runs...), nil
	case d.RunsArtifact == "":
		return nil, nil
	}
	if byID == nil {
		recs, err := c.Manifest()
		if err != nil {
			return nil, err
		}
		byID = make(map[string]ManifestRecord, len(recs))
		for _, r := range recs {
			if _, dup := byID[r.ID]; !dup {
				byID[r.ID] = r
			}
		}
	}
	scRec, ok := byID[d.RunsArtifact]
	if !ok {
		return nil, fmt.Errorf("runs artifact %q is not in the manifest", d.RunsArtifact)
	}
	sc := &scRec
	if sc.Source.Kind != "runs" || sc.Source.Derived == nil || sc.Source.Derived.ParentID != d.ParentID {
		return nil, fmt.Errorf("runs artifact %q is not a runs sidecar of parent %q", sc.ID, d.ParentID)
	}
	rel := filepath.FromSlash(sc.Path)
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("runs artifact %q: path %q lies outside the case", sc.ID, sc.Path)
	}
	full := filepath.Join(c.Dir, rel)
	if fi, err := os.Lstat(full); err != nil {
		return nil, fmt.Errorf("runs artifact %q: %w", sc.ID, err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("runs artifact %q: %q is not a regular file", sc.ID, sc.Path)
	}
	f, err := os.Open(full) //nolint:gosec // a case-relative path checked above
	if err != nil {
		return nil, fmt.Errorf("runs artifact %q: %w", sc.ID, err)
	}
	defer func() { _ = f.Close() }()
	runs, err := readRunLines(f)
	if err != nil {
		return nil, fmt.Errorf("runs artifact %q: %w", sc.ID, err)
	}
	return runs, nil
}

func readRunLines(r io.Reader) ([]Run, error) {
	br := bufio.NewReaderSize(r, 2*maxRunsLine)
	var runs []Run
	for n := 1; ; n++ {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("line %d is longer than %d bytes", n, maxRunsLine)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		atEOF := err != nil
		if atEOF && len(line) == 0 {
			return runs, nil
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) > maxRunsLine {
			return nil, fmt.Errorf("line %d is longer than %d bytes", n, maxRunsLine)
		}
		if len(runs) >= MaxRecoveredRuns {
			return nil, fmt.Errorf("more than %d runs", MaxRecoveredRuns)
		}
		var run Run
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&run); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("line %d: more than one JSON value", n)
		}
		runs = append(runs, run)
		if atEOF {
			return runs, nil
		}
	}
}

// CheckRecoveredRuns applies rules R4 and R5 to one artifact and the runs
// DerivedRuns returned for it. It is pure. A recovered artifact records only
// the runs of the bytes it holds (the rest of what the metadata named is in
// Recovery.Excluded), so the run lengths always add up to the artifact size.
func CheckRecoveredRuns(rec ManifestRecord, runs []Run) []string {
	pre := fmt.Sprintf("artifact %q (%q): ", rec.ID, rec.Path)
	var out []string
	add := func(format string, a ...any) { out = append(out, pre+fmt.Sprintf(format, a...)) }

	if d := rec.Source.Derived; d != nil && len(d.Runs) > MaxInlineRuns {
		add("records %d inline runs (more than %d; they belong in a runs sidecar)", len(d.Runs), MaxInlineRuns)
	}
	if len(runs) == 0 && rec.Size > 0 && !derivedNeedsNoRuns(rec) {
		return append(out, pre+"records no runs")
	}
	if len(runs) > MaxRecoveredRuns {
		return append(out, fmt.Sprintf("%stoo many runs (%d, at most %d)", pre, len(runs), MaxRecoveredRuns))
	}
	var sum int64
	valid := true
	for i, r := range runs {
		switch {
		case r.Offset == -1:
			add("run %d is a hole (recovered artifacts have no holes)", i)
			valid = false
		case r.Offset < 0 || r.Length < 0 || r.Offset > math.MaxInt64-r.Length:
			add("run %d is not valid (negative or overflowing)", i)
			valid = false
		case r.Length == 0:
			add("run %d is empty (zero length)", i)
			valid = false
		case sum > math.MaxInt64-r.Length:
			add("run lengths add up to more than %d bytes", int64(math.MaxInt64))
			valid = false
		default:
			sum += r.Length
		}
	}
	if !valid {
		return out
	}
	if sum != rec.Size && (len(runs) > 0 || !derivedNeedsNoRuns(rec)) {
		add("runs cover %d bytes but the artifact holds %d", sum, rec.Size)
	}
	if rec.Incomplete && rec.Error == "" {
		add("is flagged incomplete but records no error")
	}

	d := rec.Source.Derived
	if d == nil || d.Recovery == nil {
		return out
	}
	rv := d.Recovery
	a := rv.Alloc
	if a.Free < 0 || a.Allocated < 0 || a.Unknown < 0 {
		return out // rule R2 reports negative counters
	}
	if a.Free > math.MaxInt64-a.Allocated || a.Free+a.Allocated > math.MaxInt64-a.Unknown {
		add("alloc: free+allocated+unknown overflows")
		return out
	}
	if total := a.Free + a.Allocated + a.Unknown; total != sum {
		add("alloc: free+allocated+unknown (%d bytes) does not equal the %d bytes of the recorded runs", total, sum)
	}
	if rv.Class == ClassDeletedFile {
		if a.Allocated > 0 {
			add("%d allocated bytes in a deleted-file artifact", a.Allocated)
		}
		if a.Unknown > 0 {
			add("%d unknown bytes in a deleted-file artifact", a.Unknown)
		}
	}
	// the scope of slack and journal classes comes from the class table, not from the stored field
	scope := rv.Scope
	if ci, ok := LookupClass(rv.Class); ok && ci.Scope != "" {
		scope = ci.Scope
	}
	switch rv.Class {
	case ClassCarved, ClassSlack, ClassJournalBlock, ClassJournalReport:
		if scope == "unallocated" && (a.Allocated > 0 || a.Unknown > 0) {
			add("scope unallocated but %d allocated and %d unknown bytes (class %q)", a.Allocated, a.Unknown, rv.Class)
		}
	}
	return out
}

// RecoveryTrail is where the recovery description of an artifact comes from.
type RecoveryTrail struct {
	ArtifactID    string    // the artifact that carries the Recovery: the artifact itself or its nearest recovered ancestor
	Recovery      *Recovery // that artifact's description
	Hops          int       // 0 = the artifact itself
	MinConfidence *int      // lowest Confidence on the whole chain; nil when none carries one
}

// RecoveryTrailOf walks the derivation chain from id (at most maxDerivedDepth
// hops, each artifact once) and returns the nearest artifact that carries a
// Recovery together with the lowest confidence found on the whole walk. It
// returns false when no artifact on the chain carries one. A parent missing
// from the manifest, a cycle or the depth bound ends the walk without error:
// verify reports those separately.
func RecoveryTrailOf(byID map[string]ManifestRecord, id string) (RecoveryTrail, bool) {
	var tr RecoveryTrail
	found := false
	seen := map[string]bool{}
	cur := id
	for hops := 0; hops <= maxDerivedDepth; hops++ {
		r, ok := byID[cur]
		if !ok || seen[cur] {
			break
		}
		seen[cur] = true
		d := r.Source.Derived
		var rv *Recovery
		if d != nil {
			rv = d.Recovery
		}
		if rv == nil && IsRecoveredKind(r.Source.Kind) {
			// bytes labelled recovered without a description are still recovered: no confidence
			// cap, class unknown, so the writer refuses a live record and R7 reports one
			rv = &Recovery{Class: "unknown"}
		}
		if d == nil && rv == nil {
			break
		}
		if rv != nil {
			if !found {
				tr.ArtifactID, tr.Recovery, tr.Hops = cur, rv, hops
				found = true
			}
			if rv.Confidence != nil && (tr.MinConfidence == nil || *rv.Confidence < *tr.MinConfidence) {
				c := *rv.Confidence
				tr.MinConfidence = &c
			}
		}
		if d == nil {
			break
		}
		cur = d.ParentID
	}
	return tr, found
}

// derivedNeedsNoRuns reports whether the artifact is a derived report rather
// than bytes copied from an image: only the journal-report class is.
func derivedNeedsNoRuns(rec ManifestRecord) bool {
	d := rec.Source.Derived
	return d != nil && d.Recovery != nil && d.Recovery.Class == ClassJournalReport
}
